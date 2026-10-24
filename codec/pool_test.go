package codec

import "testing"

// A large transposition buffer goes back to the pool only while the message
// that held it used a fair share of it: a run of large tables keeps it, the
// first small table lets it go, and past maxPooledValues it is never kept.
// sync.Pool's own behaviour is not asserted on, since it may drop anything at
// any time; what release hands it is.
func TestALargeBufferIsKeptOnlyWhileItIsUsed(t *testing.T) {
	large := maxPooledRows * 4

	buf := scratch{}
	columns := buf.reserveTable(large, large)
	buf.release()
	if cap(columns.ints) < large || cap(columns.strings) < large {
		t.Fatal("a large table's buffer was dropped after a large table")
	}

	buf = scratch{columns: columns}
	buf.reserveTable(large/2, 10)
	strings := columns.strings[:10]
	for index := range strings {
		strings[index] = "kept alive"
	}
	buf.release()
	if columns.ints == nil {
		t.Fatal("ints used at half their size were dropped")
	}
	if columns.strings != nil {
		t.Fatal("strings used at 10 of their size were kept")
	}
	if columns.usedInts != 0 || columns.usedStrings != 0 {
		t.Fatal("the usage was not reset for the next message")
	}

	buf = scratch{columns: columns}
	buf.reserveTable(10, 3)
	buf.release()
	if columns.ints != nil {
		t.Fatal("a large buffer was kept after a small table")
	}

	if keepPooled(maxPooledValues+1, maxPooledValues+1) {
		t.Fatal("a buffer past maxPooledValues was kept")
	}
	if !keepPooled(maxPooledRows, 0) {
		t.Fatal("a small buffer was dropped")
	}
}

// release clears only the strings the message reserved, which must still be
// every string it could have written: none of the caller's may outlive it.
func TestReleaseClearsTheStringsItReserved(t *testing.T) {
	buf := scratch{}
	columns := buf.reserveTable(0, 100)
	strings := columns.strings[:100]
	for index := range strings {
		strings[index] = "caller's"
	}
	buf.release()
	for index, value := range strings {
		if value != "" {
			t.Fatalf("string %d survived release: %q", index, value)
		}
	}
}
