package codec

import "sync"

// Per-column scratch buffers are reused across columns via pools: the encoder
// gathers one column's values into a scratch slice, packs it, then returns the
// scratch. This replaces one N-sized allocation per column with a reused buffer.

var (
	i8Pool   = sync.Pool{New: func() any { s := make([]int8, 0, 256); return &s }}
	i16Pool  = sync.Pool{New: func() any { s := make([]int16, 0, 256); return &s }}
	i32Pool  = sync.Pool{New: func() any { s := make([]int32, 0, 256); return &s }}
	i64Pool  = sync.Pool{New: func() any { s := make([]int64, 0, 256); return &s }}
	f64Pool  = sync.Pool{New: func() any { s := make([]float64, 0, 256); return &s }}
	blobPool = sync.Pool{New: func() any { s := make([][]byte, 0, 256); return &s }}
)

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
func putBlobs(p *[][]byte) { blobPool.Put(p) }
