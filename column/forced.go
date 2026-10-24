//go:build !colbin_ablation

package column

// forcedTransform: see ablation.go. A constant here, so AppendArray's branch on
// it is dead code in every ordinary build.
const forcedTransform int8 = -1
