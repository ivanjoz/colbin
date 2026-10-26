//go:build !(amd64 || arm64) || purego

package column

// NarrowColumns: see narrow_columns.go. Without kernels a column is scanned in
// Go at whatever width it is held, and int64 is the one that path is tuned for.
const NarrowColumns = false
