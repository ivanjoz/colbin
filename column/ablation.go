package column

import "encoding/binary"

// forcedTransform is an ablation hook, and nothing else. In every ordinary
// build it is the constant -1 (forced.go), the branch on it compiles away, and
// AppendArray scores the transforms and keeps the smallest. Under the
// colbin_ablation build tag it is a variable (forced_ablation.go) that the
// column-codec ablation in colbin-benchmarks sets through go:linkname to trRaw,
// trDelta or trFOR, to measure what the choice is worth on real data.
//
// A forced column is still an ordinary column: the transform is recorded in the
// header byte as always, so DecodeArray reads it with no knowledge of the hook.

// appendForced writes vals under the transform given instead of the scored
// choice. Constant is never forced, because it is not a transform a column can
// be put through: it only describes one that already is constant. A delta that
// would overflow, or a one-value column, which delta cannot carry, falls back to
// raw.
func appendForced[T Signed](out []byte, vals []T, transform uint8, minimum int64, zz, deltaOverflows bool) []byte {
	zigzagged := false
	switch {
	case transform == trDelta && !deltaOverflows && len(vals) > 1:
		zigzagged = true
	case transform == trFOR:
	default:
		transform, zigzagged = trRaw, zz
	}

	header := transform
	if zigzagged {
		header |= zigzagFlag
	}
	out = append(out, header)
	switch transform {
	case trFOR:
		out = binary.LittleEndian.AppendUint64(out, zigzag(minimum))
	case trDelta:
		out = binary.LittleEndian.AppendUint64(out, zigzag(int64(vals[0])))
	}
	return appendBlocks(out, vals, transform, minimum, zigzagged)
}
