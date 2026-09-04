package codec

import (
	"sync"
	"unsafe"
)

// Per-column scratch buffers are reused across columns via pools: the encoder
// gathers one column's values into a scratch slice, packs it, then returns the
// scratch. This replaces one N-sized allocation per column with a reused buffer.
//
// The decoder borrows the same pools. It used to allocate every column buffer
// fresh, which the encoder had long since stopped doing, and on the shape that
// makes it hurt — many small messages of one type decoded in a loop — those
// per-column allocations were most of the work. A decode buffer is read into the
// destination and dropped within the column that made it, so it has exactly the
// lifetime a pool wants.

var (
	i8Pool   = sync.Pool{New: func() any { s := make([]int8, 0, 256); return &s }}
	i16Pool  = sync.Pool{New: func() any { s := make([]int16, 0, 256); return &s }}
	i32Pool  = sync.Pool{New: func() any { s := make([]int32, 0, 256); return &s }}
	i64Pool  = sync.Pool{New: func() any { s := make([]int64, 0, 256); return &s }}
	f64Pool  = sync.Pool{New: func() any { s := make([]float64, 0, 256); return &s }}
	blobPool = sync.Pool{New: func() any { s := make([][]byte, 0, 256); return &s }}
	ptrPool  = sync.Pool{New: func() any { s := make([]unsafe.Pointer, 0, 256); return &s }}
)

// getPtrs is one pointer per record or per element. Both codecs walk a column by
// handing the next level down a slice of pointers, one per value, and rebuild
// that slice at every level of every column of every message.
//
// The pooled buffer holds pointers, so it is scanned by the GC while it sits in
// the pool. putPtrs clears it before handing it back: a stale pointer in a
// pooled buffer keeps a whole decoded message alive for as long as the pool
// holds the buffer, which on a decode loop is indefinitely.
func getPtrs(n int) *[]unsafe.Pointer {
	p := ptrPool.Get().(*[]unsafe.Pointer)
	if cap(*p) < n {
		*p = make([]unsafe.Pointer, n)
	} else {
		*p = (*p)[:n]
	}
	return p
}

func putPtrs(p *[]unsafe.Pointer) {
	clear(*p)
	ptrPool.Put(p)
}

func getI8(n int) *[]int8 {
	p := i8Pool.Get().(*[]int8)
	if cap(*p) < n {
		*p = make([]int8, n)
	} else {
		*p = (*p)[:n]
	}
	return p
}
func putI8(p *[]int8) { i8Pool.Put(p) }

func getI16(n int) *[]int16 {
	p := i16Pool.Get().(*[]int16)
	if cap(*p) < n {
		*p = make([]int16, n)
	} else {
		*p = (*p)[:n]
	}
	return p
}
func putI16(p *[]int16) { i16Pool.Put(p) }

func getI32(n int) *[]int32 {
	p := i32Pool.Get().(*[]int32)
	if cap(*p) < n {
		*p = make([]int32, n)
	} else {
		*p = (*p)[:n]
	}
	return p
}
func putI32(p *[]int32) { i32Pool.Put(p) }

func getI64(n int) *[]int64 {
	p := i64Pool.Get().(*[]int64)
	if cap(*p) < n {
		*p = make([]int64, n)
	} else {
		*p = (*p)[:n]
	}
	return p
}
func putI64(p *[]int64) { i64Pool.Put(p) }

func getF64(n int) *[]float64 {
	p := f64Pool.Get().(*[]float64)
	if cap(*p) < n {
		*p = make([]float64, n)
	} else {
		*p = (*p)[:n]
	}
	return p
}
func putF64(p *[]float64) { f64Pool.Put(p) }

func getBlobs(n int) *[][]byte {
	p := blobPool.Get().(*[][]byte)
	if cap(*p) < n {
		*p = make([][]byte, n)
	} else {
		*p = (*p)[:n]
	}
	return p
}

// putBlobs clears before returning: these entries alias either the caller's byte
// slices (encode) or the message being decoded, and a pooled buffer that keeps
// holding them pins that memory for as long as the pool holds the buffer.
func putBlobs(p *[][]byte) {
	clear(*p)
	blobPool.Put(p)
}
