//! `#[derive(Colbin)]`: the straight-line encode and decode for one struct.
//!
//! Go reaches the wire two ways — a plan resolved per type and switched over per
//! field, and `codec.Generate`, which emits the calls a hand-written codec would
//! make. The generated form measured 5.4 ns to encode a ten-field record against
//! the plan's 18, because a key, an offset and a width that are constants are
//! constants the compiler can fold. Rust has no reflection to fall back on, so
//! this macro is the only path and it emits the fast one.
//!
//! What it generates, for
//!
//! ```ignore
//! #[derive(Colbin)]
//! struct Charge {
//!     #[cb(0)] company_id: u32,
//!     #[cb(1)] user_id: u32,
//!     #[cb(2)] note: String,
//! }
//! ```
//!
//! is an `impl Colbin for Charge` whose write is three `wire::Writer` calls with
//! literal keys, and whose read is a `match` on a literal key per arm.
//!
//! # Attributes
//!
//! ```ignore
//! #[cb(5)]                    // explicit wire id, as Go's `cb:"5"`
//! #[cb(name = "CompanyID")]   // the name colbin hashes, when Rust's differs
//! #[cb(name = "qty", 5)]      // both, as Go's `cb:"qty,5"`
//! #[cb(skip)]                 // not encoded, as Go's `cb:"-"`
//!
//! #[cb(wide)]                 // on the struct: eight-bit keys even when four fit
//! #[cb(packed5)]              // on the struct: pack string fields (implies wide)
//! ```
//!
//! A Rust field is `snake_case` where the Go field it mirrors is not, and an
//! un-numbered field's id is the hash of the *Go* name — so an untagged field
//! only lines up when the two names match. `#[cb(name = "...")]` is how a
//! mirrored struct says so, and an explicit id sidesteps the question entirely.
//!
//! # The key width is decided here, exactly as Go decides it
//!
//! Four-bit keys are the default and the fast path. A type goes wide when it has
//! to: an id above fifteen, which four key bits cannot carry, or any id derived
//! from a name, which lands anywhere in 0..=255. That is the same rule
//! `codec/wide.go` applies, so the two sides agree without being told.

use proc_macro::TokenStream;
use quote::{format_ident, quote};
use syn::spanned::Spanned;
use syn::{Data, DeriveInput, Fields, Ident, Type, parse_macro_input};

/// The largest key four bits carry, which is what decides the width.
const MAX_NARROW_KEY: u16 = 15;
/// What eight key bits buy.
const MAX_FIELDS: usize = 256;

#[proc_macro_derive(Colbin, attributes(cb))]
pub fn derive_colbin(input: TokenStream) -> TokenStream {
    let input = parse_macro_input!(input as DeriveInput);
    match expand(input) {
        Ok(tokens) => tokens.into(),
        Err(err) => err.to_compile_error().into(),
    }
}

fn expand(input: DeriveInput) -> syn::Result<proc_macro2::TokenStream> {
    let name = &input.ident;
    let Data::Struct(data) = &input.data else {
        return Err(syn::Error::new(
            input.span(),
            "colbin: Colbin can only be derived for a struct — the format encodes records",
        ));
    };
    let Fields::Named(named) = &data.fields else {
        return Err(syn::Error::new(
            data.fields.span(),
            "colbin: Colbin needs named fields, since a field's name is what its wire id is \
             hashed from",
        ));
    };
    let options = TypeAttrs::parse(&input)?;

    // Every field, skipped ones included, because `colbin_zero` constructs the
    // whole struct — and Go zeroes a `cb:"-"` field on decode too.
    let all_idents: Vec<_> = named
        .named
        .iter()
        .map(|field| field.ident.clone().expect("named"))
        .collect();

    let mut fields = Vec::new();
    for field in &named.named {
        let attrs = FieldAttrs::parse(field)?;
        if attrs.skip {
            continue;
        }
        let ident = field.ident.clone().expect("named");
        let shape = Shape::of(&field.ty)?;
        let wire_name = attrs.name.unwrap_or_else(|| ident.to_string());
        fields.push(DerivedField {
            ident,
            ty: field.ty.clone(),
            wire_name,
            id: attrs.id,
            shape,
        });
    }
    if fields.is_empty() {
        return Err(syn::Error::new(
            named.span(),
            "colbin: a record needs at least one encodable field",
        ));
    }
    if fields.len() > MAX_FIELDS {
        return Err(syn::Error::new(
            named.span(),
            "colbin: a one-byte key holds 256 fields",
        ));
    }
    reject_duplicate_ids(&fields)?;

    // The key width, by the rule `codec/wide.go` states: a derived id lands
    // anywhere in 0..=255, and an explicit one above fifteen does not fit the
    // nibble. Either way the run goes wide, and so do packed strings, whose
    // encoding lives in a descriptor a narrow field does not have.
    let derived = fields.iter().any(|field| field.id.is_none());
    let past_narrow = fields
        .iter()
        .any(|field| field.id.is_some_and(|id| id > MAX_NARROW_KEY));
    let wide = derived || past_narrow || options.wide || options.packed5;

    let keys = KeyConstants::build(name, &fields);
    let key_items = keys.items();

    // A nested record is zeroed through its own `colbin_zero` rather than
    // through `Default`, so that deriving does not also require deriving
    // `Default` on every type in the tree. A skipped field has no shape to go
    // by and takes `Default`, which is what Go's `SetZero` does to it.
    let zero = all_idents.iter().map(|ident| {
        match fields
            .iter()
            .find(|field| &field.ident == ident)
            .map(|field| (&field.ty, &field.shape))
        {
            Some((ty, Shape::Struct)) => {
                quote!(#ident: <#ty as ::colbin::Colbin>::colbin_zero())
            }
            _ => quote!(#ident: ::core::default::Default::default()),
        }
    });

    let writes = fields.iter().enumerate().map(|(index, field)| {
        let key = keys.expression(index);
        field.shape.write(&field.ident, &key, wide, options.packed5)
    });

    let read_arms = |reader_wide: bool| {
        fields
            .iter()
            .enumerate()
            .map(|(index, field)| {
                let ident = &field.ident;
                let pattern = keys.pattern(index);
                let read = field.shape.read(&field.ty, reader_wide);
                quote!(#pattern => out.#ident = #read,)
            })
            .collect::<Vec<_>>()
    };
    let narrow_reads = read_arms(false);
    let wide_reads = read_arms(true);

    let columns = Columns::build(&fields, &keys);
    let column_specs = columns.specs();
    let column_i64 = columns.get_i64();
    let column_str = columns.get_str();
    let set_column_i64 = columns.set_i64();
    let set_column_str = columns.set_str();

    let writer = if wide {
        quote!(::colbin::wire::Writer8)
    } else {
        quote!(::colbin::wire::Writer)
    };

    Ok(quote! {
        #key_items

        #[automatically_derived]
        impl ::colbin::Colbin for #name {
            const WIDE_KEYS: bool = #wide;
            const COLUMNS: &'static [::colbin::ColumnSpec] = #column_specs;

            fn colbin_zero() -> Self {
                Self { #(#zero),* }
            }

            fn colbin_write_run(&self, buf: &mut ::std::vec::Vec<u8>) {
                #[allow(unused_mut, unused_variables)]
                let mut w = #writer::new(buf);
                #(#writes)*
            }

            fn colbin_read_run(
                body: &[u8],
                wide_keys: bool,
            ) -> ::core::result::Result<Self, ::colbin::Error> {
                // A field the message omits means the value was zero, so the
                // destination starts cleared rather than holding whatever it had.
                let mut out = <Self as ::colbin::Colbin>::colbin_zero();
                if wide_keys {
                    // The key width is what the descriptor that opened this run
                    // said, not what this type would have written: a peer may
                    // have written the other one.
                    #[allow(unused_mut, unused_variables)]
                    let mut r = ::colbin::wire::Reader8::new(body);
                    while r.more() {
                        #[allow(unreachable_patterns)]
                        match r.key() {
                            #(#wide_reads)*
                            // A wide field sizes itself, so one this type does
                            // not declare is stepped over — which is schema
                            // evolution without a coordinated deploy.
                            _ => if !r.skip() { break },
                        }
                    }
                    r.err()?;
                } else {
                    #[allow(unused_mut, unused_variables)]
                    let mut r = ::colbin::wire::Reader::new(body);
                    while r.more() {
                        #[allow(unreachable_patterns)]
                        match r.key() {
                            #(#narrow_reads)*
                            // Four descriptor bits have no room for a class, so
                            // nothing can size a field it cannot classify: an
                            // unknown narrow key ends the run.
                            key => return ::core::result::Result::Err(
                                ::colbin::Error::UnknownKey(key)),
                        }
                    }
                    r.err()?;
                }
                ::core::result::Result::Ok(out)
            }

            #column_i64
            #column_str
            #set_column_i64
            #set_column_str
        }
    })
}

/// Refuses two fields asking for the same id, which is a record definition that
/// cannot round-trip and is silent if it reaches the wire.
fn reject_duplicate_ids(fields: &[DerivedField]) -> syn::Result<()> {
    for (index, field) in fields.iter().enumerate() {
        let Some(id) = field.id else { continue };
        for other in &fields[index + 1..] {
            if other.id == Some(id) {
                return Err(syn::Error::new(
                    other.ident.span(),
                    format!(
                        "colbin: field id {id} is on both `{}` and `{}`",
                        field.ident, other.ident
                    ),
                ));
            }
        }
    }
    Ok(())
}

struct DerivedField {
    ident: Ident,
    /// The declared type, which the generated code names where inference cannot
    /// reach it: a map's own type, and a nested record's zero.
    ty: Type,
    wire_name: String,
    id: Option<u16>,
    shape: Shape,
}

/// The wire keys, as constants the generated code can both write and match on.
///
/// When every field numbers itself the keys are literals and the `match` is a
/// jump table. When any is derived they come from `colbin::assign_ids`, which is
/// a `const fn` for exactly this reason: the ids stay compile-time constants and
/// there is still only one implementation of the hash-and-probe rule, rather
/// than a third copy of it living in a proc macro.
struct KeyConstants {
    literals: Option<Vec<u16>>,
    names: Vec<Ident>,
    table: Ident,
    specs: Vec<(String, i16)>,
}

impl KeyConstants {
    fn build(type_name: &Ident, fields: &[DerivedField]) -> Self {
        let literals = fields
            .iter()
            .map(|field| field.id)
            .collect::<Option<Vec<_>>>();
        // Upper-cased, because these are matched on as constant patterns and a
        // lower-case letter in one draws `non_upper_case_globals` at every use.
        // Two types whose names differ only in case would collide here, which is
        // a compile error rather than anything silent.
        let upper = type_name.to_string().to_uppercase();
        let table = format_ident!("_COLBIN_KEYS_{}", upper);
        let names = (0..fields.len())
            .map(|index| format_ident!("_COLBIN_KEY_{}_{}", upper, index))
            .collect();
        let specs = fields
            .iter()
            .map(|field| (field.wire_name.clone(), field.id.map_or(-1, |id| id as i16)))
            .collect();
        Self {
            literals,
            names,
            table,
            specs,
        }
    }

    /// The constant items the impl needs, which is nothing at all when every id
    /// is a literal.
    fn items(&self) -> proc_macro2::TokenStream {
        if self.literals.is_some() {
            return quote!();
        }
        let table = &self.table;
        let count = self.specs.len();
        let names = self.specs.iter().map(|(name, _)| name);
        let declared = self.specs.iter().map(|(_, id)| id);
        let per_field = self.names.iter().enumerate().map(|(index, name)| {
            quote! {
                #[doc(hidden)]
                #[allow(non_upper_case_globals)]
                const #name: u8 = #table[#index];
            }
        });
        quote! {
            #[doc(hidden)]
            #[allow(non_upper_case_globals)]
            const #table: [u8; #count] =
                ::colbin::assign_ids(&[#(#names),*], &[#(#declared),*]);
            #(#per_field)*
        }
    }

    /// The key as an expression, for a writer call.
    fn expression(&self, index: usize) -> proc_macro2::TokenStream {
        match &self.literals {
            Some(ids) => {
                let id = ids[index] as u8;
                quote!(#id)
            }
            None => {
                let name = &self.names[index];
                quote!(#name)
            }
        }
    }

    /// The key as a pattern, for a decode arm. A constant is a legal pattern, so
    /// the derived form is still a `match` and not an if-chain.
    fn pattern(&self, index: usize) -> proc_macro2::TokenStream {
        self.expression(index)
    }
}

/// The transposition accessors, for a type every field of which the column codec
/// carries. A type holding a blob, a slice, a map or a nested struct is not
/// transposable and gets an empty column list, which is what stops the writer
/// from ever choosing a table for it.
struct Columns<'a> {
    fields: Vec<(usize, &'a DerivedField)>,
    keys: Vec<proc_macro2::TokenStream>,
    transposable: bool,
}

impl<'a> Columns<'a> {
    fn build(fields: &'a [DerivedField], keys: &KeyConstants) -> Self {
        let transposable = fields.iter().all(|field| field.shape.columnable());
        Self {
            fields: fields.iter().enumerate().collect(),
            keys: (0..fields.len())
                .map(|index| keys.expression(index))
                .collect(),
            transposable,
        }
    }

    fn specs(&self) -> proc_macro2::TokenStream {
        if !self.transposable {
            return quote!(&[]);
        }
        let entries = self.fields.iter().map(|(index, field)| {
            let key = &self.keys[*index];
            let kind = if matches!(field.shape, Shape::Str) {
                quote!(::colbin::ColumnKind::Str)
            } else {
                quote!(::colbin::ColumnKind::Int)
            };
            quote!(::colbin::ColumnSpec { key: #key, kind: #kind })
        });
        quote!(&[#(#entries),*])
    }

    fn get_i64(&self) -> proc_macro2::TokenStream {
        if !self.transposable {
            return quote!();
        }
        let arms = self.fields.iter().filter_map(|(index, field)| {
            let ident = &field.ident;
            let value = field.shape.to_column_i64(ident)?;
            Some(quote!(#index => #value,))
        });
        quote! {
            fn colbin_column_i64(&self, column: usize) -> i64 {
                match column {
                    #(#arms)*
                    _ => 0,
                }
            }
        }
    }

    fn get_str(&self) -> proc_macro2::TokenStream {
        if !self.transposable {
            return quote!();
        }
        let arms = self.fields.iter().filter_map(|(index, field)| {
            if !matches!(field.shape, Shape::Str) {
                return None;
            }
            let ident = &field.ident;
            Some(quote!(#index => &self.#ident,))
        });
        quote! {
            fn colbin_column_str(&self, column: usize) -> &str {
                match column {
                    #(#arms)*
                    _ => "",
                }
            }
        }
    }

    fn set_i64(&self) -> proc_macro2::TokenStream {
        if !self.transposable {
            return quote!();
        }
        let arms = self.fields.iter().filter_map(|(index, field)| {
            let ident = &field.ident;
            let assign = field.shape.set_column_expr(ident)?;
            Some(quote!(#index => #assign,))
        });
        quote! {
            fn colbin_set_column_i64(&mut self, column: usize, value: i64) {
                match column {
                    #(#arms)*
                    _ => {}
                }
            }
        }
    }

    fn set_str(&self) -> proc_macro2::TokenStream {
        if !self.transposable {
            return quote!();
        }
        let arms = self.fields.iter().filter_map(|(index, field)| {
            if !matches!(field.shape, Shape::Str) {
                return None;
            }
            let ident = &field.ident;
            Some(quote!(#index => self.#ident = value,))
        });
        quote! {
            fn colbin_set_column_str(&mut self, column: usize, value: ::std::string::String) {
                match column {
                    #(#arms)*
                    _ => {}
                }
            }
        }
    }
}

/// Options on the struct itself.
#[derive(Default)]
struct TypeAttrs {
    wide: bool,
    packed5: bool,
}

impl TypeAttrs {
    fn parse(input: &DeriveInput) -> syn::Result<Self> {
        let mut out = Self::default();
        for attr in &input.attrs {
            if !attr.path().is_ident("cb") {
                continue;
            }
            attr.parse_nested_meta(|meta| {
                if meta.path.is_ident("wide") {
                    out.wide = true;
                    return Ok(());
                }
                if meta.path.is_ident("packed5") {
                    out.packed5 = true;
                    return Ok(());
                }
                Err(meta.error("colbin: a struct takes `wide` or `packed5`, as in #[cb(wide)]"))
            })?;
        }
        Ok(out)
    }
}

/// Options on one field.
#[derive(Default)]
struct FieldAttrs {
    id: Option<u16>,
    name: Option<String>,
    skip: bool,
}

impl FieldAttrs {
    /// Parses `#[cb(...)]`.
    ///
    /// Hand-rolled rather than through `syn::Meta`, because a bare integer —
    /// `#[cb(1)]`, which is the form Go's `cb:"1"` maps to — is not a `Meta` at
    /// all, and going through one rejects it before any fallback can look.
    fn parse(field: &syn::Field) -> syn::Result<Self> {
        let mut out = Self::default();
        for attr in &field.attrs {
            if !attr.path().is_ident("cb") {
                continue;
            }
            attr.parse_args_with(|input: syn::parse::ParseStream<'_>| {
                while !input.is_empty() {
                    if input.peek(syn::LitInt) {
                        out.id = Some(parse_id(&input.parse::<syn::LitInt>()?)?);
                    } else if input.peek(syn::Ident) {
                        let option: Ident = input.parse()?;
                        if option == "skip" {
                            out.skip = true;
                        } else if option == "name" {
                            let _: syn::Token![=] = input.parse()?;
                            out.name = Some(input.parse::<syn::LitStr>()?.value());
                        } else {
                            return Err(syn::Error::new(option.span(), OPTIONS));
                        }
                    } else {
                        return Err(input.error(OPTIONS));
                    }
                    if input.is_empty() {
                        break;
                    }
                    let _: syn::Token![,] = input.parse()?;
                }
                Ok(())
            })?;
        }
        Ok(out)
    }
}

const OPTIONS: &str = "colbin: expected a field id, `name = \"GoFieldName\"` or `skip`, as in \
     #[cb(5)], #[cb(name = \"CompanyID\")] or #[cb(name = \"qty\", 5)]";

fn parse_id(lit: &syn::LitInt) -> syn::Result<u16> {
    let value: u32 = lit.base10_parse()?;
    if value >= MAX_FIELDS as u32 {
        return Err(syn::Error::new(
            lit.span(),
            "colbin: a field id is one byte, so 0..=255",
        ));
    }
    Ok(value as u16)
}

/// The value forms the derive carries, and everything each one needs to
/// generate: its writer call, its reader expression, and — for the scalars — its
/// column accessors.
enum Shape {
    Bool,
    Int(u8),
    Uint(u8),
    Float(u8),
    Str,
    /// `Vec<u8>`, which is Go's `[]byte`: a blob rather than an integer column.
    Bytes,
    /// A slice of integers, through the VEC class.
    Ints,
    /// A slice of strings, through the homogeneous LIST class.
    Strings,
    /// An optional scalar or string. An absent key means `None`, so a `Some`
    /// holding a zero has to say so out loud — which is the one place this
    /// format writes a zero rather than omitting it.
    Optional(Box<Shape>),
    /// A nested struct, which is another key run at its own key width.
    Struct,
    /// A slice of nested structs, written row-wise or transposed.
    Structs,
    /// A map, whose keys are values rather than field ids.
    Map,
}

impl Shape {
    fn of(ty: &Type) -> syn::Result<Self> {
        if let Some(inner) = generic_element(ty, "Vec") {
            return Ok(match Self::of(inner)? {
                Self::Uint(8) => Self::Bytes,
                Self::Int(_) | Self::Uint(_) => Self::Ints,
                Self::Str => Self::Strings,
                Self::Struct => Self::Structs,
                Self::Bool | Self::Float(_) => {
                    return Err(syn::Error::new(
                        ty.span(),
                        "colbin: the format carries slices of integers, of strings and of \
                         structs; a slice of bools or floats has no column form",
                    ));
                }
                _ => {
                    return Err(syn::Error::new(
                        ty.span(),
                        "colbin: a slice of slices, of options or of maps is not carried",
                    ));
                }
            });
        }
        if let Some(inner) = generic_element(ty, "Option") {
            let shape = Self::of(inner)?;
            if !shape.columnable() {
                return Err(syn::Error::new(
                    ty.span(),
                    "colbin: an optional field carries a scalar or a string. A composite \
                     already carries a length, so absence is expressible for it, but a nil \
                     composite and an empty one are the same thing on this wire",
                ));
            }
            return Ok(Self::Optional(Box::new(shape)));
        }
        let Some(ident) = path_ident(ty) else {
            return Err(syn::Error::new(ty.span(), unsupported()));
        };
        Ok(match ident.as_str() {
            "bool" => Self::Bool,
            "i8" => Self::Int(8),
            "i16" => Self::Int(16),
            "i32" => Self::Int(32),
            "i64" => Self::Int(64),
            "u8" => Self::Uint(8),
            "u16" => Self::Uint(16),
            "u32" => Self::Uint(32),
            "u64" => Self::Uint(64),
            "f32" => Self::Float(32),
            "f64" => Self::Float(64),
            "String" => Self::Str,
            // Their width is platform dependent and never reaches the wire, so a
            // message written on one host would decode differently on another.
            "isize" | "usize" => {
                return Err(syn::Error::new(
                    ty.span(),
                    "colbin: isize and usize have a platform-dependent width, which never \
                     reaches the wire; use a fixed-width integer",
                ));
            }
            "HashMap" | "BTreeMap" => Self::Map,
            // Anything else is taken to be another record. It has to implement
            // Colbin, which is where a type that is not one is reported.
            _ => Self::Struct,
        })
    }

    /// Whether the column codec carries this shape, which is what makes a slice
    /// of the enclosing struct a candidate for a table.
    fn columnable(&self) -> bool {
        matches!(
            self,
            Self::Bool | Self::Int(_) | Self::Uint(_) | Self::Float(_) | Self::Str
        )
    }

    /// The writer call for the field, including the omit-zero test the writer
    /// itself makes for everything but an optional.
    fn write(
        &self,
        ident: &Ident,
        key: &proc_macro2::TokenStream,
        wide: bool,
        packed: bool,
    ) -> proc_macro2::TokenStream {
        match self {
            Self::Bool => quote!(w.bool(#key, self.#ident);),
            Self::Int(8) => quote!(w.i64(#key, i64::from(self.#ident));),
            Self::Int(16) => quote!(w.i64(#key, i64::from(self.#ident));),
            Self::Int(32) => quote!(w.i32(#key, self.#ident);),
            Self::Int(_) => quote!(w.i64(#key, self.#ident);),
            Self::Uint(8) => quote!(w.u8(#key, self.#ident);),
            Self::Uint(16) => quote!(w.u16(#key, self.#ident);),
            Self::Uint(32) => quote!(w.u32(#key, self.#ident);),
            Self::Uint(_) => quote!(w.u64(#key, self.#ident);),
            Self::Float(32) => quote!(w.f32(#key, self.#ident);),
            Self::Float(_) => quote!(w.f64(#key, self.#ident);),
            // The encoding is a code in the field's own descriptor, so a packed
            // string and a raw one sit side by side on the same wire and the
            // reader is never told which to expect.
            Self::Str if packed => quote!(w.packed_string(#key, &self.#ident);),
            Self::Str => quote!(w.string(#key, &self.#ident);),
            Self::Bytes => quote!(w.bytes(#key, &self.#ident);),
            Self::Ints => quote!(w.ints(#key, &self.#ident);),
            Self::Strings => quote!(w.strings(#key, &self.#ident);),
            Self::Struct => {
                let call = helper("write_struct", wide);
                quote!(#call(&mut w, #key, &self.#ident);)
            }
            Self::Structs => {
                let call = helper("write_structs", wide);
                quote!(#call(&mut w, #key, &self.#ident);)
            }
            Self::Map => {
                let write = if wide {
                    quote!(write_element8)
                } else {
                    quote!(write_element)
                };
                quote! {
                    if !self.#ident.is_empty() {
                        let mark = w.open_map(#key, self.#ident.len());
                        for (key, value) in &self.#ident {
                            ::colbin::MapValue::#write(key, &mut w);
                            ::colbin::MapValue::#write(value, &mut w);
                        }
                        w.close(mark);
                    }
                }
            }
            Self::Optional(inner) => {
                let zero = inner.zero_test();
                let explicit = inner.explicit_zero(key, wide);
                let present = inner.write_present(key, packed);
                quote! {
                    if let ::core::option::Option::Some(value) = &self.#ident {
                        if #zero { #explicit } else { #present }
                    }
                }
            }
        }
    }

    /// The writer call for an optional's pointee, which is already known to be
    /// non-zero and is reached through a borrow rather than through `self`.
    fn write_present(
        &self,
        key: &proc_macro2::TokenStream,
        packed: bool,
    ) -> proc_macro2::TokenStream {
        match self {
            Self::Bool => quote!(w.bool(#key, *value);),
            Self::Int(8) | Self::Int(16) => quote!(w.i64(#key, i64::from(*value));),
            Self::Int(32) => quote!(w.i32(#key, *value);),
            Self::Int(_) => quote!(w.i64(#key, *value);),
            Self::Uint(8) => quote!(w.u8(#key, *value);),
            Self::Uint(16) => quote!(w.u16(#key, *value);),
            Self::Uint(32) => quote!(w.u32(#key, *value);),
            Self::Uint(_) => quote!(w.u64(#key, *value);),
            Self::Float(32) => quote!(w.f32(#key, *value);),
            Self::Float(_) => quote!(w.f64(#key, *value);),
            Self::Str if packed => quote!(w.packed_string(#key, value);),
            Self::Str => quote!(w.string(#key, value);),
            _ => quote!(),
        }
    }

    /// The test that says a present optional holds its type's zero, and so has
    /// to be written explicitly rather than omitted.
    fn zero_test(&self) -> proc_macro2::TokenStream {
        match self {
            Self::Bool => quote!(!*value),
            Self::Int(_) | Self::Uint(_) => quote!(*value == 0),
            // Compared by value, not by bits: Go's isZeroScalar does the same, so
            // a pointer to -0.0 writes an explicit +0.0 on both sides.
            Self::Float(_) => quote!(*value == 0.0),
            Self::Str => quote!(value.is_empty()),
            _ => quote!(false),
        }
    }

    /// How a present zero is spelled. The signed nibble and the unsigned one are
    /// different tables over the same four bits, so a zero has to be written in
    /// the one its reader will use — which is a distinction the wide descriptor
    /// does not have.
    fn explicit_zero(
        &self,
        key: &proc_macro2::TokenStream,
        wide: bool,
    ) -> proc_macro2::TokenStream {
        match self {
            Self::Str => quote!(w.empty_string(#key);),
            Self::Int(_) if !wide => quote!(w.zero_signed(#key);),
            _ => quote!(w.zero(#key);),
        }
    }

    /// The reader expression for the field.
    fn read(&self, ty: &Type, wide: bool) -> proc_macro2::TokenStream {
        let inner = match self {
            Self::Optional(inner) => inner.as_ref(),
            other => other,
        };
        let value = match inner {
            Self::Bool => quote!(r.bool()),
            Self::Int(8) => quote!(r.i8()),
            Self::Int(16) => quote!(r.i16()),
            Self::Int(32) => quote!(r.i32()),
            Self::Int(_) => quote!(r.i64()),
            Self::Uint(8) => quote!(r.u8()),
            Self::Uint(16) => quote!(r.u16()),
            Self::Uint(32) => quote!(r.u32()),
            Self::Uint(_) => quote!(r.u64()),
            Self::Float(32) => quote!(r.f32()),
            Self::Float(_) => quote!(r.f64()),
            // A wide string reads through the packed reader whether or not this
            // type writes packed ones: the descriptor's `enc` code says which
            // encoding is there, so a message written with packing on reads back
            // with it off. A narrow descriptor has no room for that code, so a
            // narrow string is always raw.
            Self::Str if wide => quote!(r.packed_string()),
            Self::Str => quote!(r.string()),
            Self::Bytes => quote!(r.bytes().to_vec()),
            Self::Ints => quote!(r.ints()),
            Self::Strings => quote!(r.strings()),
            Self::Struct => {
                let call = helper("read_struct", wide);
                quote!(#call(&mut r))
            }
            Self::Structs => {
                let call = helper("read_structs", wide);
                quote!(#call(&mut r))
            }
            Self::Map => {
                let counted = if wide {
                    quote!(r.map())
                } else {
                    quote!(r.counted())
                };
                let read = if wide {
                    quote!(read_element8)
                } else {
                    quote!(read_element)
                };
                quote! {{
                    let mut map: #ty = ::core::default::Default::default();
                    if let ::core::option::Option::Some((count, mut entries)) = #counted {
                        for _ in 0..count {
                            let key = ::colbin::MapValue::#read(&mut entries);
                            let value = ::colbin::MapValue::#read(&mut entries);
                            if entries.err().is_err() {
                                break;
                            }
                            map.insert(key, value);
                        }
                        r.fail_with(entries.err());
                    }
                    map
                }}
            }
            Self::Optional(_) => unreachable!("an optional does not nest"),
        };
        if matches!(self, Self::Optional(_)) {
            return quote!(::core::option::Option::Some(#value));
        }
        value
    }

    /// This field as a column value, widened to `i64` — which is what the
    /// blocked column codec takes. A bool travels as 0 or 1 and a float as its
    /// bit pattern, whose transforms find nothing but whose raw blocks cost
    /// exactly the element width.
    fn to_column_i64(&self, ident: &Ident) -> Option<proc_macro2::TokenStream> {
        Some(match self {
            Self::Bool => quote!(if self.#ident { 1 } else { 0 }),
            Self::Int(_) | Self::Uint(_) => quote!(self.#ident as i64),
            Self::Float(32) => quote!(self.#ident.to_bits() as i64),
            Self::Float(_) => quote!(self.#ident.to_bits() as i64),
            Self::Str => return None,
            _ => return None,
        })
    }

    fn set_column_expr(&self, ident: &Ident) -> Option<proc_macro2::TokenStream> {
        Some(match self {
            Self::Bool => quote!(self.#ident = value == 1),
            Self::Int(8) => quote!(self.#ident = value as i8),
            Self::Int(16) => quote!(self.#ident = value as i16),
            Self::Int(32) => quote!(self.#ident = value as i32),
            Self::Int(_) => quote!(self.#ident = value),
            Self::Uint(8) => quote!(self.#ident = value as u8),
            Self::Uint(16) => quote!(self.#ident = value as u16),
            Self::Uint(32) => quote!(self.#ident = value as u32),
            Self::Uint(_) => quote!(self.#ident = value as u64),
            Self::Float(32) => quote!(self.#ident = f32::from_bits(value as u32)),
            Self::Float(_) => quote!(self.#ident = f64::from_bits(value as u64)),
            Self::Str => return None,
            _ => return None,
        })
    }
}

/// The composite helper for a key width. The two are separate functions rather
/// than one with a flag, for the reason the whole format keeps the widths apart:
/// a width the compiler cannot see is a width it cannot fold.
fn helper(name: &str, wide: bool) -> proc_macro2::TokenStream {
    let ident = format_ident!("{}{}", name, if wide { "8" } else { "" });
    quote!(::colbin::codec::#ident)
}

fn unsupported() -> &'static str {
    "colbin: the derive carries bool, the fixed-width integers and floats, String, Vec<u8>, \
     slices of integers, strings and structs, Option of a scalar, a nested struct and a map"
}

/// The element type of a `Vec<T>` or an `Option<T>`, or `None` for anything
/// else.
fn generic_element<'a>(ty: &'a Type, wrapper: &str) -> Option<&'a Type> {
    let Type::Path(path) = ty else { return None };
    let segment = path.path.segments.last()?;
    if segment.ident != wrapper {
        return None;
    }
    let syn::PathArguments::AngleBracketed(args) = &segment.arguments else {
        return None;
    };
    args.args.iter().find_map(|arg| match arg {
        syn::GenericArgument::Type(ty) => Some(ty),
        _ => None,
    })
}

/// The last segment of a plain path type, so `std::string::String` and `String`
/// resolve alike.
fn path_ident(ty: &Type) -> Option<String> {
    let Type::Path(path) = ty else { return None };
    let segment = path.path.segments.last()?;
    Some(segment.ident.to_string())
}
