package codec

// The walk for a plan with nothing nested in it.
//
// `writePlan` and `readField` handle every field a type can hold, composites
// included. That generality is not free even for a type that has none: the
// composite cases call out of line and take a scratch buffer, so the switch
// keeps a frame and spills registers the scalar cases would otherwise hold, and
// the cost lands on every record whether or not it has a nested field in it.
//
// Most records do not. A plan with no struct, slice-of-struct, map, pointer or
// dynamic field is flagged `simple` when it is built, and the entry points send
// it to appendScalars and readScalars instead — the value arms alone, inline, so
// they need no scratch buffer and no frame. Those walks are generated from
// valueOps, with every other switch over a value op; see ops.go.

// simplePlan reports whether every field is a value op, which is what makes the
// flat walks sufficient.
func (plan *typePlan) simplePlan() bool {
	for _, field := range plan.fields {
		if _, ok := valueOpOf(field.op); !ok {
			return false
		}
	}
	return true
}
