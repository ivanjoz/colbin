//go:build race

package codec

// raceEnabled says the race detector is on. Under it the compiler leaves
// slices.Grow's append(s, make([]E, n)...) unfused, so every grow allocates a
// temporary of the room asked for, and a byte count doubles though nothing in
// the codec changed.
const raceEnabled = true
