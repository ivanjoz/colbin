package codec

import "testing"

// A document is structs inside structs, with a pointer here and there, and a
// codec handle encoding one onto a reused buffer, or decoding one into a reused
// record, has nothing to allocate for it. Each allocation this pins came back
// once: a writer handed to a func value, and a decode's scratch taken to the
// heap by a pointer to its arena.
type allocsLeaf struct {
	On   bool    `cb:"1"`
	Name *string `cb:"2"`
	Size uint32  `cb:"3"`
}

type allocsMiddle struct {
	Leaf  allocsLeaf `cb:"1"`
	Count int64      `cb:"2"`
}

type allocsDocument struct {
	Middle allocsMiddle `cb:"1"`
	Level  *uint32      `cb:"2"`
}

func TestANestedDocumentDoesNotAllocate(t *testing.T) {
	level := uint32(2)
	value := allocsDocument{Middle: allocsMiddle{Leaf: allocsLeaf{On: true, Size: 300}, Count: -7}, Level: &level}
	codec := MustCodec[allocsDocument]()
	buffer, err := codec.Append(nil, &value)
	if err != nil {
		t.Fatal(err)
	}
	if allocs := testing.AllocsPerRun(100, func() { buffer, _ = codec.Append(buffer[:0], &value) }); allocs != 0 {
		t.Errorf("encode: %v allocations", allocs)
	}
	var into allocsDocument
	if err := codec.Unmarshal(buffer, &into); err != nil {
		t.Fatal(err)
	}
	if into.Middle != value.Middle || into.Level == nil || *into.Level != level {
		t.Errorf("decoded %+v", into)
	}
	// A pointer the message carries is a value of its own, allocated: the
	// decode has nothing else to allocate.
	value.Level = nil
	buffer, _ = codec.Append(buffer[:0], &value)
	if allocs := testing.AllocsPerRun(100, func() { _ = codec.Unmarshal(buffer, &into) }); allocs != 0 {
		t.Errorf("decode: %v allocations", allocs)
	}
}
