package codec

// The tree program: a narrow type's nested structs written as one walk.
//
// A configuration document is structs inside structs, and most of them hold
// one field: `rules.ordered-imports.options.grouped-imports` is four structs
// around a bool. Walked as plans, each level is an appendNarrowStruct, a
// writePlan and a writeFields, and their frames were most of such a document's
// encode — a level cost more than the fields in it.
//
// So a narrow type that nests a narrow struct by value gets a program over the
// whole tree: its fields and its value structs' fields, at their offsets from
// the root record, with each struct's header opened before its fields and its
// length closed after them. The leaves a program writes itself are the ones
// writeFields writes inline — a number, a bool, a string — and a pointer to one;
// anything else, a list or a map or a struct behind a pointer, is writeFields on
// that field alone, as it would have been anyway. The bytes are writeFields'.
//
// A tree of nothing but those leaves is pure, and a pure tree at the root needs
// no scratch at all: its depth is the type's, fixed and bounded, and nothing in
// it can fail. appendRecord writes one the way appendPlan writes a flat record.

import (
	"unsafe"

	"github.com/ivanjoz/colbin/internal/wirefmt"
	"github.com/ivanjoz/colbin/wire"
)

// maxTreeDepth is how many structs deep a tree program writes inline; a deeper
// struct is written by a call, as every struct was before.
const maxTreeDepth = 8

const (
	treeValue uint8 = iota // a scalar or a string, inline
	treePointer            // a pointer to one: nothing when nil
	treeOpen               // a value struct's header
	treeClose              // its length
	treeCall               // anything else, through writeFields
	treeRun                // a run of values, through the row kernel
	treeStructs            // a slice of structs, through appendNarrowStructs
)

// treeRunMin is the fewest values in a row that a tree program hands the row
// kernel rather than writing itself: a record's strings, five of them in a
// row, were slower one at a time than in the kernel, and a value or two alone
// slower through the kernel's call.
const treeRunMin = 3

type treeOp struct {
	kind uint8
	key  uint8 // the field's key, as writeFields passes it
	op   fieldOp
	// field indexes fields and singles, for a pointer and a call.
	field  uint16
	offset uint32 // from the root record
}

type treeProgram struct {
	ops []treeOp
	// fields is the field of each pointer and call op, its offset taken from
	// the root record, and singles a one-field plan around each call's.
	fields  []planField
	singles []*typePlan
	// runs is the row program of each run op, by its field, over offsets from
	// the root record.
	runs []*rowProgram
	// pure says no op is a call: see the file comment.
	pure bool
}

// compileTree builds a plan's tree program, or nil for a plan that nests no
// narrow struct by value: a flat one is the row program's.
func compileTree(plan *typePlan) *treeProgram {
	if plan.isWide || plan.fromSchema {
		return nil
	}
	tree := &treeProgram{pure: true}
	nested, fits := false, true
	var emit func(plan *typePlan, base uintptr, depth int)
	emit = func(plan *typePlan, base uintptr, depth int) {
		for _, field := range plan.fields {
			field.offset += base
			if field.offset > 1<<32-1 || len(tree.fields) >= 1<<16 {
				fits = false
				return
			}
			op := treeOp{key: field.key, op: field.op, offset: uint32(field.offset)}
			switch {
			case !field.indirect && field.op == opStruct && field.sub != nil &&
				!field.sub.isWide && !field.sub.fromSchema && depth < maxTreeDepth:
				nested = true
				op.kind = treeOpen
				tree.ops = append(tree.ops, op)
				emit(field.sub, field.offset, depth+1)
				tree.ops = append(tree.ops, treeOp{kind: treeClose})
				continue
			case !field.indirect && treeInline(field.op):
				op.kind = treeValue
			case !field.indirect && field.op == opPointer:
				op.kind, op.field = treePointer, uint16(len(tree.fields))
				tree.fields = append(tree.fields, field)
				tree.singles = append(tree.singles, nil)
			default:
				op.kind, op.field = treeCall, uint16(len(tree.fields))
				if !field.indirect && field.op == opStructs {
					op.kind = treeStructs
				}
				tree.fields = append(tree.fields, field)
				tree.singles = append(tree.singles, &typePlan{fields: []planField{field}})
				tree.pure = false
			}
			tree.ops = append(tree.ops, op)
		}
	}
	emit(plan, 0, 0)
	if !nested || !fits {
		return nil
	}
	tree.ops = tree.gatherRuns()
	return tree
}

// gatherRuns replaces each run of treeRunMin or more values with a run op.
func (tree *treeProgram) gatherRuns() []treeOp {
	var out []treeOp
	for start := 0; start < len(tree.ops); {
		end := start
		for end < len(tree.ops) && tree.ops[end].kind == treeValue {
			end++
		}
		if end-start < treeRunMin {
			out = append(out, tree.ops[start:max(end, start+1)]...)
			start = max(end, start+1)
			continue
		}
		program := &rowProgram{reserve: 8}
		for _, op := range tree.ops[start:end] {
			kind, _ := rowKind(&planField{op: op.op})
			program.ops = append(program.ops, rowOp{kind: kind, key: op.key << 4, offset: op.offset})
			if kind <= rowBool {
				program.reserve += fixedRoom
			}
		}
		out = append(out, treeOp{kind: treeRun, field: uint16(len(tree.runs))})
		tree.runs = append(tree.runs, program)
		start = end
	}
	return out
}

// treeInline is whether a tree program writes a field of op itself.
func treeInline(op fieldOp) bool {
	switch op {
	case opBool, opInt8, opInt16, opInt32, opInt64, opUint8, opUint16, opUint32,
		opUint64, opFloat32, opFloat64, opString:
		return true
	}
	return false
}

// write is writePlan for the program's plan, with packed strings off. Each
// struct it opens takes a level of buf's depth, as writePlan's would have; a
// pure program is run with no buf at all.
func (tree *treeProgram) write(writer *wire.Writer, record unsafe.Pointer, buf *scratch) {
	var marks [maxTreeDepth]int
	open := 0
	buffer := writer.Buffer
	for index := range tree.ops {
		op := &tree.ops[index]
		at := unsafe.Add(record, op.offset)
		switch op.kind {
		case treeValue:
			switch op.op {
			case opBool:
				buffer = wirefmt.AppendBool(buffer, op.key, *(*bool)(at))
			case opInt8:
				buffer = wirefmt.AppendInt(buffer, op.key, int64(*(*int8)(at)))
			case opInt16:
				buffer = wirefmt.AppendInt(buffer, op.key, int64(*(*int16)(at)))
			case opInt32:
				buffer = wirefmt.AppendI32(buffer, op.key, *(*int32)(at))
			case opInt64:
				buffer = wirefmt.AppendInt(buffer, op.key, *(*int64)(at))
			case opUint8:
				buffer = wirefmt.AppendU16(buffer, op.key, uint16(*(*uint8)(at)))
			case opUint16:
				buffer = wirefmt.AppendU16(buffer, op.key, *(*uint16)(at))
			case opUint32:
				buffer = wirefmt.AppendU32(buffer, op.key, *(*uint32)(at))
			case opUint64:
				buffer = wirefmt.AppendUint(buffer, op.key, *(*uint64)(at))
			case opFloat32:
				buffer = wirefmt.AppendF32(buffer, op.key, *(*float32)(at))
			case opFloat64:
				buffer = wirefmt.AppendF64(buffer, op.key, *(*float64)(at))
			case opString:
				buffer = wirefmt.AppendString(buffer, op.key, *(*string)(at))
			}
		case treePointer:
			if *(*unsafe.Pointer)(at) == nil {
				continue
			}
			writer.Buffer = buffer
			appendPointer(writer, &tree.fields[op.field], at, false)
			buffer = writer.Buffer
		case treeOpen:
			// wire's OpenStruct, onto the buffer in hand: the header and a
			// length byte that Close fills in.
			if buf != nil && !buf.enter() {
				writer.Buffer = buffer
				return
			}
			buffer = append(buffer, op.key<<4|wirefmt.NibbleLength, 0)
			marks[open] = len(buffer) - 1
			open++
		case treeClose:
			open--
			mark := marks[open]
			if body := len(buffer) - mark - 1; body <= wirefmt.InlineLength {
				buffer[mark] = uint8(body)
			} else {
				writer.Buffer = buffer
				writer.Close(wire.MarkAt(mark))
				buffer = writer.Buffer
			}
			if buf != nil {
				buf.leave()
			}
		case treeRun:
			writer.Buffer = buffer
			tree.runs[op.field].write(writer, record, buf)
			buffer = writer.Buffer
		case treeStructs:
			writer.Buffer = buffer
			appendNarrowStructs(writer, &tree.fields[op.field], at, buf)
			buffer = writer.Buffer
		default:
			writer.Buffer = buffer
			writeFields(writer, tree.singles[op.field], record, buf)
			buffer = writer.Buffer
		}
	}
	writer.Buffer = buffer
}
