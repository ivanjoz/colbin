package codec

// Decoding into a value that already holds one.
//
// A decode clears its destination first, because a key the message omits means
// the field was zero. Clearing a slice field drops its backing array, so a
// decode into the same value every time allocated every slice again: on a page
// of sales, one []SaleLine per sale and the GC to collect them, where a decoder
// that keeps them allocates nothing.
//
// So a type with slice fields is cleared by a program instead: every field set
// to zero, but each slice set to length zero with its capacity kept. A list or
// a table that fits in that capacity is decoded into it (newSlice), and so is a
// list of integers or strings (the reads in ops_gen.go append to the field cut
// to zero). This is what encoding/json does with a slice it decodes into, and
// it has the same consequence: a slice taken from the value before the decode
// sees its elements overwritten.
//
// What a decode leaves at length zero with capacity behind it is a field the
// message did not carry, and settle sets it back to nil, which is what a cleared
// field was before. A list the message carries empty is made with no capacity,
// as before, so it stays an empty slice and not nil.

import (
	"reflect"
	"unsafe"
)

type resetKind uint8

const (
	// resetBytes clears size bytes holding no pointers.
	resetBytes resetKind = iota
	resetString
	// resetPointer is any one-word pointer: a pointer, a map, a channel, a func.
	resetPointer
	resetInterface
	// resetSlice keeps the slice's array and capacity, at length zero.
	resetSlice
)

type resetStep struct {
	offset uintptr
	size   uintptr
	kind   resetKind
}

// resetProgram clears a record of one type and keeps its slices' capacity.
type resetProgram struct {
	steps []resetStep
	// slices is the offset of every slice the steps keep, for settle.
	slices []uintptr
}

// maxResetArray is the longest array holding pointers that a program unrolls.
// Past it the type is cleared whole and keeps nothing.
const maxResetArray = 16

// compileReset builds the program for a struct type, or nil for a layout the
// program does not spell, which leaves the type to be cleared whole and a slice
// of it to be allocated afresh.
func compileReset(structType reflect.Type) *resetProgram {
	program := &resetProgram{}
	end, ok := program.walk(structType, 0, 0)
	if !ok {
		return nil
	}
	program.clearBytes(end, structType.Size())
	return program
}

// keeps is whether the program keeps anything a plain clear would drop. A
// record without slices is cleared whole, which is faster than the program.
func (p *resetProgram) keeps() bool {
	return p != nil && len(p.slices) != 0
}

// resetRows clears count records from data on, stride apart.
func (p *resetProgram) resetRows(data unsafe.Pointer, count int, stride uintptr) {
	if len(p.steps) == 1 && p.steps[0].kind == resetBytes && p.steps[0].size == stride {
		// A row of plain bytes: the rows are one run of them.
		clear(unsafe.Slice((*byte)(data), uintptr(count)*stride))
		return
	}
	for index := range count {
		p.reset(unsafe.Add(data, uintptr(index)*stride))
	}
}

// walk adds the steps for a value of type t at offset, given that everything
// from cleared up to offset is pointer-free bytes not yet cleared. It returns
// where the bytes still to clear start after it.
func (p *resetProgram) walk(t reflect.Type, offset, cleared uintptr) (uintptr, bool) {
	if !hasPointers(t) {
		return cleared, true
	}
	switch t.Kind() {
	case reflect.Struct:
		for index := range t.NumField() {
			field := t.Field(index)
			var ok bool
			if cleared, ok = p.walk(field.Type, offset+field.Offset, cleared); !ok {
				return 0, false
			}
		}
		return cleared, true
	case reflect.Array:
		if t.Len() > maxResetArray {
			return 0, false
		}
		for index := range t.Len() {
			var ok bool
			if cleared, ok = p.walk(t.Elem(), offset+uintptr(index)*t.Elem().Size(), cleared); !ok {
				return 0, false
			}
		}
		return cleared, true
	}
	var kind resetKind
	switch t.Kind() {
	case reflect.String:
		kind = resetString
	case reflect.Slice:
		kind = resetSlice
		p.slices = append(p.slices, offset)
	case reflect.Interface:
		kind = resetInterface
	case reflect.Pointer, reflect.Map, reflect.Chan, reflect.Func, reflect.UnsafePointer:
		kind = resetPointer
	default:
		return 0, false
	}
	p.clearBytes(cleared, offset)
	p.steps = append(p.steps, resetStep{offset: offset, size: t.Size(), kind: kind})
	return offset + t.Size(), true
}

func (p *resetProgram) clearBytes(from, to uintptr) {
	if to > from {
		p.steps = append(p.steps, resetStep{offset: from, size: to - from, kind: resetBytes})
	}
}

// hasPointers is whether a value of t holds anything the collector traces.
func hasPointers(t reflect.Type) bool {
	switch t.Kind() {
	case reflect.Struct:
		for index := range t.NumField() {
			if hasPointers(t.Field(index).Type) {
				return true
			}
		}
		return false
	case reflect.Array:
		return t.Len() > 0 && hasPointers(t.Elem())
	case reflect.String, reflect.Slice, reflect.Interface, reflect.Pointer,
		reflect.Map, reflect.Chan, reflect.Func, reflect.UnsafePointer:
		return true
	}
	return false
}

// reset clears the record at, keeping its slices' arrays.
func (p *resetProgram) reset(at unsafe.Pointer) {
	for index := range p.steps {
		step := &p.steps[index]
		field := unsafe.Add(at, step.offset)
		switch step.kind {
		case resetBytes:
			clear(unsafe.Slice((*byte)(field), step.size))
		case resetString:
			*(*string)(field) = ""
		case resetPointer:
			*(*unsafe.Pointer)(field) = nil
		case resetInterface:
			*(*any)(field) = nil
		case resetSlice:
			slice := (*sliceHeader)(field)
			slice.len = 0
			if slice.cap == 0 {
				// An empty slice that is not nil has nothing to lend, and would
				// otherwise outlive a decode that does not carry the field.
				slice.data = nil
			}
		}
	}
}

// holds is whether the record at has a slice with an array to lend. A root
// that has none, as every new value, is cleared whole and needs no settle:
// on a flat record of a few fields, the program's step per field is most of
// what a decode into a new value would otherwise pay for reuse.
func (p *resetProgram) holds(at unsafe.Pointer) bool {
	if p == nil {
		return false
	}
	for _, offset := range p.slices {
		if (*sliceHeader)(unsafe.Add(at, offset)).cap != 0 {
			return true
		}
	}
	return false
}

// settleRows is settle over the rows of a table newSlice reused. A row type has
// no slice among its columns, but may hold one the plan does not carry.
func (p *resetProgram) settleRows(reused bool, data unsafe.Pointer, count int, stride uintptr) {
	if !reused || !p.keeps() {
		return
	}
	for index := range count {
		p.settle(unsafe.Add(data, uintptr(index)*stride))
	}
}

// settle sets back to nil every kept slice the decode did not fill.
func (p *resetProgram) settle(at unsafe.Pointer) {
	for _, offset := range p.slices {
		if slice := (*sliceHeader)(unsafe.Add(at, offset)); slice.len == 0 && slice.cap != 0 {
			*slice = sliceHeader{}
		}
	}
}
