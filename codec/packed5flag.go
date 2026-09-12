package codec

import "sync/atomic"

// packed5On is the process-wide switch colbin.SetPacked5 drives. It is an
// atomic so that reading it in an encoder is a plain load rather than a lock,
// and so that setting it at startup is visible to every goroutine after.
//
// It is a *writer* setting only. The format carries a string's encoding in the
// field's own descriptor, so a decoder reads either form regardless of how this
// is set — which is what makes turning it on a size decision rather than a wire
// version.
var packed5On atomic.Bool

// SetPacked5 turns the packed5 string encoding on or off for every encoder in
// the process. Off by default; see colbin.SetPacked5 for what it trades.
func SetPacked5(on bool) {
	packed5On.Store(on)
	// A plan caches the key width it resolved, and packed5 is one of the two
	// things that decides it. Dropping the cache is what makes the setting take
	// effect on types already in use — and why it has to be set at startup
	// rather than mid-flight.
	planCache.Clear()
	// A schema section states that same key width, for the one run that cannot
	// say it on the wire, so it goes stale with the plans it was built from.
	schemaCache.Clear()
}

// Packed5 reports whether the packed5 string encoding is on.
func Packed5() bool { return packed5On.Load() }
