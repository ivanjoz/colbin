//go:build !colbin_ablation

package codec

// tableThreshold: see table.go. A constant in every ordinary build, so the
// comparison against it costs nothing.
const tableThreshold = 8
