//go:build colbin_ablation

package column

// forcedTransform: see ablation.go. Set only by the colbin-benchmarks ablation,
// through go:linkname; nothing in this module writes it.
var forcedTransform int8 = -1
