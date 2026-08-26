package codec

func allZeroFloat64sScalar(vals []float64) bool {
	for _, v := range vals {
		if v != 0 {
			return false
		}
	}
	return true
}
