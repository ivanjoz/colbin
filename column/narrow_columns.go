//go:build (amd64 || arm64) && !purego

package column

// NarrowColumns is whether a table keeps an integer column that fits an int32
// at that width: half the scratch to gather it into. amd64 has kernels for
// such a column (narrowKernels); arm64 widens it, one column at a time, for
// its int64 ones (appendWidened), which costs a pass over the column and keeps
// a whole table's scratch at half the size.
const NarrowColumns = true
