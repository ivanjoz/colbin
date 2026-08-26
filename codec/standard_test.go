package codec

// marshalStandard encodes v with compact mode disabled, the way MarshalJSON
// does. Marshal is free to choose compact mode for a small message, so tests
// that pin the columnar wire format go through here instead.
func marshalStandard(v any) (out []byte, err error) {
	defer recoverEncode(&out, &err)
	rv, err := marshalRoot(v)
	if err != nil {
		return nil, err
	}
	return appendMessage([]byte{formatVersion}, rv, false)
}
