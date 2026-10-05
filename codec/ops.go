package codec

import "reflect"

// valueOps is the one place a value op says how it is written and read.
//
// Every per-field switch over these ops — the flat walks, the nested walks'
// value arms, a pointer's pointee, the JSON walks, and the source Generate emits
// — is generated from this table rather than written out by hand. They are
// written out at all, rather than shared behind a call, because the flat walks
// are the hot path and an inline switch there is worth several nanoseconds a
// record; what that cost before was fourteen hand-kept copies that had started to
// disagree. ops_gen.go is the output, and TestValueOpsAreGenerated fails when it
// is stale and rewrites it under COLBIN_UPDATE=1.
//
// The writer and reader method names are shared by wire's two key widths, which
// is what lets one row serve both.
var valueOps = []valueOp{
	{op: opBool, goType: reflect.TypeFor[bool](), write: "Bool", read: "Bool()", json: jsonBool},
	{op: opInt8, goType: reflect.TypeFor[int8](), write: "Int", widen: "int64", read: "I8()", json: jsonSigned},
	{op: opInt16, goType: reflect.TypeFor[int16](), write: "Int", widen: "int64", read: "I16()", json: jsonSigned},
	{op: opInt32, goType: reflect.TypeFor[int32](), write: "I32", read: "I32()", json: jsonSigned},
	{op: opInt64, goType: reflect.TypeFor[int64](), write: "Int", read: "Int()", json: jsonSigned},
	{op: opUint8, goType: reflect.TypeFor[uint8](), write: "U16", widen: "uint16", read: "U8()", json: jsonUnsigned},
	{op: opUint16, goType: reflect.TypeFor[uint16](), write: "U16", read: "U16()", json: jsonUnsigned},
	{op: opUint32, goType: reflect.TypeFor[uint32](), write: "U32", read: "U32()", json: jsonUnsigned},
	{op: opUint64, goType: reflect.TypeFor[uint64](), write: "Uint", read: "Uint()", json: jsonUnsigned},
	{op: opFloat32, goType: reflect.TypeFor[float32](), write: "F32", read: "F32()", json: jsonFloat32},
	{op: opFloat64, goType: reflect.TypeFor[float64](), write: "F64", read: "F64()", json: jsonFloat64},
	// A string packs when packed5 is on, and String reads either form: the
	// header says which.
	{op: opString, goType: reflect.TypeFor[string](), write: "String", read: "String()", json: jsonText},
	// A blob is copied out rather than aliased: the message is usually a read
	// buffer the caller reuses.
	{op: opBytes, goType: reflect.TypeFor[[]byte](), write: "Bytes", read: "Bytes()", json: jsonBlob},
	{op: opInt8s, goType: reflect.TypeFor[[]int8](), write: "Int8s", read: "Int8s(nil)", json: jsonSignedArray},
	{op: opInt16s, goType: reflect.TypeFor[[]int16](), write: "Int16s", read: "Int16s(nil)", json: jsonSignedArray},
	{op: opInt32s, goType: reflect.TypeFor[[]int32](), write: "Int32s", read: "Int32s(nil)", json: jsonSignedArray},
	{op: opInt64s, goType: reflect.TypeFor[[]int64](), write: "Ints", read: "Ints(nil)", json: jsonSignedArray},
	{op: opUint16s, goType: reflect.TypeFor[[]uint16](), write: "Uint16s", read: "Uint16s(nil)", json: jsonUnsignedArray},
	{op: opUint32s, goType: reflect.TypeFor[[]uint32](), write: "Uint32s", read: "Uint32s(nil)", json: jsonUnsignedArray},
	{op: opUint64s, goType: reflect.TypeFor[[]uint64](), write: "Uint64s", read: "Uint64s(nil)", json: jsonUnsignedArray},
	{op: opStrings, goType: reflect.TypeFor[[]string](), write: "Strings", read: "Strings(nil)", json: jsonTextArray},
}

// valueOp is one row of valueOps.
type valueOp struct {
	op fieldOp
	// goType is what the field holds, and what the generated code reads it as.
	goType reflect.Type
	// write is the writer method, which takes the key and the value.
	write string
	// widen is the type the value is converted to for write, when the method
	// takes a wider one than the field holds.
	widen string
	// read is the reader call that yields a goType.
	read string
	json jsonKind
}

// jsonKind is how the JSON walk hands a value op's result to its sink.
type jsonKind uint8

const (
	jsonBool jsonKind = iota
	jsonSigned
	jsonUnsigned
	jsonFloat32
	jsonFloat64
	jsonText
	jsonBlob
	jsonSignedArray
	jsonUnsignedArray
	jsonTextArray
)

// goTypeName spells a row's type the way Go source does, which reflect does not
// for a []byte.
func goTypeName(row valueOp) string {
	if row.op == opBytes {
		return "[]byte"
	}
	return row.goType.String()
}

// valueOpOf finds an op's row, or reports that the op is not a value op.
func valueOpOf(op fieldOp) (valueOp, bool) {
	for _, row := range valueOps {
		if row.op == op {
			return row, true
		}
	}
	return valueOp{}, false
}
