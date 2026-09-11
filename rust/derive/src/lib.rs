//! `#[derive(Colbin)]`: a typed encode and decode for one struct.
//!
//! The dynamic path hands back a `Record` — a `Vec<(u8, Value)>` with a 32-byte
//! `Value` per field — which costs an allocation per record and a 40-byte move
//! per field before the caller has read anything. This generates field access
//! instead: one `match` on the field's position, and the value goes straight into
//! the struct field it belongs to.
//!
//! What it generates, for
//!
//! ```ignore
//! #[derive(Colbin)]
//! struct Token {
//!     #[cb(name = "CompanyID")] company_id: i32,
//!     #[cb(3)]                  id: i32,
//!     user: String,
//! }
//! ```
//!
//! is an `impl Colbin for Token` whose read is
//!
//! ```ignore
//! match index {
//!     0 => self.company_id = r.i32(),
//!     1 => self.id = r.i32(),
//!     2 => self.user = r.string()?,
//!     ...
//! }
//! ```
//!
//! # Field ids are not computed here
//!
//! Deliberately. The macro emits the field *names* and lets
//! `Schema::from_fields` assign the ids at runtime, exactly as every other
//! caller does, and the generated `match` dispatches on declaration index. The
//! alternative — hashing the names in the macro so the match could be on the id
//! itself — would put a third copy of the id algorithm in the tree, after Go's
//! and the Rust runtime's, for the sake of one array lookup per field.
//!
//! # Attributes
//!
//! ```ignore
//! #[cb(5)]                    // explicit wire id, as Go's `cb:"5"`
//! #[cb(name = "CompanyID")]   // the name colbin hashes, when Rust's differs
//! #[cb(name = "qty", 5)]      // both, as Go's `cb:"qty,5"`
//! #[cb(skip)]                 // not encoded, as Go's `cb:"-"`
//! ```
//!
//! A Rust field is `snake_case` where the Go field it mirrors is not, and the
//! hash is over the *Go* name — so an untagged field only lines up when the two
//! names match. `#[cb(name = "...")]` is how a mirrored struct says so, and an
//! explicit id sidesteps the question entirely.

use proc_macro::TokenStream;
use quote::{format_ident, quote};
use syn::spanned::Spanned;
use syn::{Data, DeriveInput, Fields, Ident, Type, parse_macro_input};

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
            "colbin: Colbin needs named fields, since a field's name is what its wire id is hashed from",
        ));
    };

    // Every field, skipped ones included, because `colbin_zero` constructs the
    // whole struct. Go zeroes a `cb:"-"` field on decode too -- `decodeCompact`
    // calls SetZero on the destination -- so a skipped field is reset, not left
    // alone.
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
    if fields.len() > 254 {
        return Err(syn::Error::new(
            named.span(),
            "colbin: a record may hold at most 254 encodable fields",
        ));
    }

    let specs = fields.iter().map(|field| {
        let wire_name = &field.wire_name;
        let kind = field.shape.kind();
        let id = match field.id {
            Some(id) => quote!(::core::option::Option::Some(#id)),
            None => quote!(::core::option::Option::None),
        };
        quote! {
            ::colbin::FieldSpec { name: #wire_name, id: #id, kind: #kind }
        }
    });

    let zero = all_idents
        .iter()
        .map(|ident| quote!(#ident: ::core::default::Default::default()));

    let reads = fields.iter().enumerate().map(|(index, field)| {
        let ident = &field.ident;
        let read = field.shape.read();
        quote!(#index => self.#ident = #read,)
    });

    let writes = fields.iter().enumerate().map(|(index, field)| {
        let ident = &field.ident;
        let present = field.shape.present(ident);
        let write = field.shape.write(index, ident);
        quote! {
            if #present {
                #write
            }
        }
    });

    // Only signed integers have anything to say about ALL_POSITIVE: unsigned
    // values are never zigzagged, floats are not varints, and an integer slice
    // rides the array codec, which the flag does not govern.
    let sign_tests: Vec<_> = fields
        .iter()
        .filter(|field| matches!(field.shape, Shape::Int(_)))
        .map(|field| {
            let ident = &field.ident;
            quote!(self.#ident >= 0)
        })
        .collect();
    let all_positive = if sign_tests.is_empty() {
        quote!(true)
    } else {
        quote!(#(#sign_tests)&&*)
    };

    let field_count = fields.len();
    Ok(quote! {
        #[automatically_derived]
        impl ::colbin::Colbin for #name {
            const COLBIN_FIELDS: usize = #field_count;

            fn colbin_fields() -> ::std::vec::Vec<::colbin::FieldSpec<'static>> {
                ::std::vec![#(#specs),*]
            }

            fn colbin_zero() -> Self {
                Self { #(#zero),* }
            }

            fn colbin_read(
                &mut self,
                index: usize,
                r: &mut ::colbin::FieldReader<'_, '_>,
            ) -> ::core::result::Result<(), ::colbin::Error> {
                match index {
                    #(#reads)*
                    // The index came from a schema built out of the very list
                    // above, so this is only reachable through a hand-written
                    // call. Reported rather than panicked on, because the trait
                    // is public.
                    _ => return ::core::result::Result::Err(::colbin::Error::FieldIndex(index)),
                }
                ::core::result::Result::Ok(())
            }

            fn colbin_write(
                &self,
                w: &mut ::colbin::FieldWriter<'_, '_>,
            ) -> ::core::result::Result<(), ::colbin::Error> {
                // A field holding its zero value is skipped: compact mode's key
                // run *is* the presence information.
                #(#writes)*
                ::core::result::Result::Ok(())
            }

            fn colbin_all_positive(&self) -> bool {
                #all_positive
            }
        }
    })
}

struct DerivedField {
    ident: Ident,
    wire_name: String,
    id: Option<u8>,
    shape: Shape,
}

#[derive(Default)]
struct FieldAttrs {
    id: Option<u8>,
    name: Option<String>,
    skip: bool,
}

impl FieldAttrs {
    /// Parses `#[cb(...)]`.
    ///
    /// Hand-rolled rather than through `syn::Meta`, because a bare integer --
    /// `#[cb(1)]`, which is the form Go's `cb:"1"` maps to -- is not a `Meta` at
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

const OPTIONS: &str = "colbin: expected a field id, `name = \"GoFieldName\"` or `skip`, as in #[cb(5)], \
     #[cb(name = \"CompanyID\")] or #[cb(name = \"qty\", 5)]";

fn parse_id(lit: &syn::LitInt) -> syn::Result<u8> {
    let value: u16 = lit.base10_parse()?;
    if value > 254 {
        return Err(syn::Error::new(
            lit.span(),
            "colbin: a field id is 0..=254; 255 closes a record",
        ));
    }
    Ok(value as u8)
}

/// The value forms the derive can carry, and everything each one needs to
/// generate: its `Kind`, its reader call, its zero test and its writer call.
///
/// Composites — a nested struct, a slice of them, a map — are not here. They
/// need the schema of the type one level down at the point of the read, which
/// the flat forms do not, and the dynamic `Codec` carries them today. See
/// `rust/PLAN.md`.
#[derive(Clone, Copy, PartialEq, Eq)]
enum Shape {
    Bool,
    Int(u8),
    Uint(u8),
    Float(u8),
    Str,
    Bytes,
    Ints(u8),
    Uints(u8),
    Bools,
    Strs,
    Floats(u8),
}

impl Shape {
    fn of(ty: &Type) -> syn::Result<Self> {
        if let Some(inner) = vec_element(ty) {
            return Ok(match Self::of(inner)? {
                // `Vec<u8>` is Go's `[]byte`, a blob rather than an integer
                // column, which is why it is not `Uints(8)`.
                Self::Uint(8) => Self::Bytes,
                Self::Bool => Self::Bools,
                Self::Str => Self::Strs,
                Self::Int(width) => Self::Ints(width),
                Self::Uint(width) => Self::Uints(width),
                Self::Float(width) => Self::Floats(width),
                _ => {
                    return Err(syn::Error::new(
                        ty.span(),
                        "colbin: the derive carries a slice of scalars, strings or bytes; \
                         a slice of structs, of slices or of maps needs colbin::Codec",
                    ));
                }
            });
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
            // `isize`/`usize` are absent for varint's reason: their width is
            // platform dependent and never reaches the wire.
            "isize" | "usize" => {
                return Err(syn::Error::new(
                    ty.span(),
                    "colbin: isize and usize have a platform-dependent width, which never reaches \
                     the wire; use a fixed-width integer",
                ));
            }
            _ => return Err(syn::Error::new(ty.span(), unsupported())),
        })
    }

    fn kind(self) -> proc_macro2::TokenStream {
        let variant = match self {
            Self::Bool => "Bool",
            Self::Int(8) => "Int8",
            Self::Int(16) => "Int16",
            Self::Int(32) => "Int32",
            Self::Int(_) => "Int64",
            Self::Uint(8) => "Uint8",
            Self::Uint(16) => "Uint16",
            Self::Uint(32) => "Uint32",
            Self::Uint(_) => "Uint64",
            Self::Float(32) => "Float32",
            Self::Float(_) => "Float64",
            Self::Str => "String",
            Self::Bytes => "Bytes",
            Self::Ints(8) => "Int8s",
            Self::Ints(16) => "Int16s",
            Self::Ints(32) => "Int32s",
            Self::Ints(_) => "Int64s",
            Self::Uints(16) => "Uint16s",
            Self::Uints(32) => "Uint32s",
            Self::Uints(_) => "Uint64s",
            Self::Bools => "Bools",
            Self::Strs => "Strings",
            Self::Floats(32) => "Float32s",
            Self::Floats(_) => "Float64s",
        };
        let variant = format_ident!("{variant}");
        quote!(::colbin::Kind::#variant)
    }

    /// The reader call. The fallible forms allocate, so they carry a `?`.
    fn read(self) -> proc_macro2::TokenStream {
        match self {
            Self::Bool => quote!(r.bool()),
            Self::Int(width) => {
                let method = format_ident!("i{width}");
                quote!(r.#method())
            }
            Self::Uint(width) => {
                let method = format_ident!("u{width}");
                quote!(r.#method())
            }
            Self::Float(width) => {
                let method = format_ident!("f{width}");
                quote!(r.#method())
            }
            Self::Str => quote!(r.string()?),
            Self::Bytes => quote!(r.bytes()?),
            Self::Ints(width) => {
                let method = format_ident!("i{width}s");
                quote!(r.#method()?)
            }
            Self::Uints(width) => {
                let method = format_ident!("u{width}s");
                quote!(r.#method()?)
            }
            Self::Bools => quote!(r.bools()?),
            Self::Strs => quote!(r.strings()?),
            Self::Floats(width) => {
                let method = format_ident!("f{width}s");
                quote!(r.#method()?)
            }
        }
    }

    /// The test that decides whether the field is written at all.
    fn present(self, ident: &Ident) -> proc_macro2::TokenStream {
        match self {
            Self::Bool => quote!(self.#ident),
            Self::Int(_) | Self::Uint(_) => quote!(self.#ident != 0),
            // Compared on the bits so that negative zero survives: -0.0 == 0.0
            // is true, and omitting it would decode back as +0.0.
            Self::Float(_) => quote!(self.#ident.to_bits() != 0),
            Self::Str | Self::Bytes => quote!(!self.#ident.is_empty()),
            Self::Ints(_) | Self::Uints(_) | Self::Bools | Self::Strs | Self::Floats(_) => {
                quote!(!self.#ident.is_empty())
            }
        }
    }

    fn write(self, index: usize, ident: &Ident) -> proc_macro2::TokenStream {
        match self {
            Self::Bool => quote!(w.bool(#index, self.#ident);),
            Self::Int(width) => {
                let method = format_ident!("i{width}");
                quote!(w.#method(#index, self.#ident);)
            }
            Self::Uint(width) => {
                let method = format_ident!("u{width}");
                quote!(w.#method(#index, self.#ident);)
            }
            Self::Float(width) => {
                let method = format_ident!("f{width}");
                quote!(w.#method(#index, self.#ident);)
            }
            Self::Str => quote!(w.str(#index, &self.#ident);),
            Self::Bytes => quote!(w.bytes(#index, &self.#ident);),
            Self::Ints(width) => {
                let method = format_ident!("i{width}s");
                quote!(w.#method(#index, &self.#ident);)
            }
            Self::Uints(width) => {
                let method = format_ident!("u{width}s");
                quote!(w.#method(#index, &self.#ident);)
            }
            Self::Bools => quote!(w.bools(#index, &self.#ident);),
            Self::Strs => quote!(w.strings(#index, &self.#ident);),
            Self::Floats(width) => {
                let method = format_ident!("f{width}s");
                quote!(w.#method(#index, &self.#ident);)
            }
        }
    }
}

fn unsupported() -> &'static str {
    "colbin: the derive carries bool, the fixed-width integers and floats, String, Vec<u8> and \
     slices of those; a nested struct, a slice of structs or a map needs colbin::Codec"
}

/// The element type of a `Vec<T>`, or `None` for anything else.
fn vec_element(ty: &Type) -> Option<&Type> {
    let Type::Path(path) = ty else { return None };
    let segment = path.path.segments.last()?;
    if segment.ident != "Vec" {
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
    if !matches!(segment.arguments, syn::PathArguments::None) {
        return None;
    }
    Some(segment.ident.to_string())
}
