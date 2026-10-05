package codec

import "sync/atomic"

// packed5On is the process-wide switch colbin.SetPacked5 drives. It is an
// atomic so that reading it in an encoder is a plain load rather than a lock,
// and so that setting it is visible to every goroutine after.
//
// It is a *writer* setting only. The format carries a string's encoding in the
// field's own descriptor, so a decoder reads either form regardless of how this
// is set — which is what makes turning it on a size decision rather than a wire
// version. Nothing a plan or a schema section holds depends on it either, so it
// may change while encoders run: a message in flight at the switch holds either
// form, or both, and reads the same.
var packed5On atomic.Bool

// SetPacked5 turns the packed5 string encoding on or off for every encoder in
// the process. Off by default; see colbin.SetPacked5 for what it trades.
func SetPacked5(on bool) { packed5On.Store(on) }

// Packed5 reports whether the packed5 string encoding is on.
func Packed5() bool { return packed5On.Load() }
