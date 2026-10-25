//go:build !amd64 || purego

package column

// scanSIMD is statsSIMD and then blockedCostsSIMD, with the costs scored as
// appendLong would. amd64's assembly does it in one read of the column (see
// asm_amd64.go); everywhere else it is the two scans in turn.
func scanSIMD(vals []int64) (minimum int64, constant, deltaOverflows bool, raw, frame, steps int) {
	minimum, constant, deltaOverflows = statsSIMD(vals)
	raw, frame, steps = blockedCostsSIMD(vals, minimum, minimum < 0, !deltaOverflows && len(vals) > 1)
	return minimum, constant, deltaOverflows, raw, frame, steps
}
