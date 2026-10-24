//go:build colbin_ablation

package codec

// tableThreshold: see table.go. Under the colbin_ablation build tag, and only
// there, it is a variable, so that the threshold ablation in colbin-benchmarks
// can move it through go:linkname. A variable costs a load on a hot path — about
// 4% on a three-line nested record — which is why no ordinary build has one.
// Nothing in this module writes it.
var tableThreshold = 8
