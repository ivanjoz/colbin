package codec

// String dictionaries: a table's low-cardinality string column carried as ids.
//
// A column of statuses, countries or currencies repeats a handful of strings
// row after row. With dictionaries on, the writer gives each such column a
// dictionary — one per field, so `Address.City` shares one wherever an address
// sits in the message — writes the strings once in a section ahead of the body,
// and writes the column as ids through the column codec: ⌈log₂ n⌉ bits a row for
// n distinct values.
//
//	message := [root] [dictionary section] [body]
//	section := [byteLength] [dictCount] dict*
//	dict    := [entryCount] ( [size] [bytes] )*
//
// Every length and count is one byte up to 254, or 0xFF and a u32. The root
// byte's 0x02 bit says the section is there.
//
// # Off costs nothing
//
// Turning dictionaries on is a separate path, not a mode of the ordinary one.
// The encoder takes it from appendRecord, on a plan that nests, before any of
// the walk; a message that ends up using no dictionary is byte for byte what
// the ordinary path writes, root included. The decoder takes it from the branch
// that refuses an unknown root byte, so the ordinary roots are dispatched by
// exactly the switch they were. Inside the walks, the one thing either path
// adds is a nil check per string column of a table — once per column, never
// per row or per field.
//
// # What it does not cover yet
//
// Only table columns. A self-describing message, the JSON walks and the Rust
// and JS readers do not take a dictionary yet, and refuse its root byte.

import (
	"errors"
	"math/bits"
	"reflect"
	"sync"
	"sync/atomic"
	"unsafe"

	"github.com/ivanjoz/colbin/wire"
)

// rootDictionary says a dictionary section sits between the root byte and the
// body.
const rootDictionary byte = 0x02

// maxDictEntries bounds a dictionary, so that an id is at most two bytes'
// worth: past it a column is not low-cardinality and goes out as strings.
const maxDictEntries = 1 << 16

// dictSampleRows is how far into a column the writer looks before giving up on
// one whose values are nearly all new. 1024 rows drawn from a thousand values
// are about 63% new, so a column of a thousand clerks is kept; drawn from a
// column of comments, nearly all of them are.
const dictSampleRows = 1024

// dictScanChunk is how many rows tableIDs scans of each column before moving
// to the next. A column at a time was 4% slower on TPC-H's lineitem; it is not
// gatherChunk, which is sized for the gather's cache and not the kernel's call.
const dictScanChunk = 256

var dictionaryOn atomic.Bool

// SetDictionary turns string dictionaries on or off for every encoder in the
// process. Off by default. Like SetPacked5 it is a writer setting: a reader that
// knows dictionaries reads a message with or without them.
func SetDictionary(on bool) { dictionaryOn.Store(on) }

// Dictionary reports whether string dictionaries are on.
func Dictionary() bool { return dictionaryOn.Load() }

// dictionaries is a message's dictionaries, while it is written or read. A
// scratch holds one only on the dictionary path.
type dictionaries struct {
	// byField is the writer's dictionary per column field, and written the ones
	// a column has used, in the order their numbers were given.
	byField map[dictField]*dictEntries
	written []*dictEntries
	// ids holds the ids being written or read: every string column of the
	// table being written, or the one column being read. columns is the
	// writer's per-column state. Both are reused across tables.
	ids     []int64
	columns []dictColumn
	// usedIDs is the most of ids the message has needed, which release judges
	// a large buffer by, as scratch.release does the column buffers.
	usedIDs int
	// read is the reader's: the section's dictionaries, by number.
	read [][]string
	// spare are an earlier message's dictionaries, emptied, whose index tables
	// entriesFor hands out again: allocating and regrowing them per message was
	// most of what a dictionary encode allocated, and the collections it set
	// off cost a third of a Public BI encode.
	spare []*dictEntries
}

var dictPool sync.Pool // *dictionaries

// newDictionaries is a message's dictionaries, with the buffers of an earlier
// message's when the pool has one: a table of TPC-H's lineitem needs 19 MB of
// ids, which allocating per message cost a tenth of its encode.
func newDictionaries() *dictionaries {
	if d, _ := dictPool.Get().(*dictionaries); d != nil {
		return d
	}
	return &dictionaries{}
}

// release hands d back holding nothing of the message: its dictionaries and
// the strings in them go, and only the buffers stay.
func (d *dictionaries) release() {
	for _, entries := range d.byField {
		if entries.empty() {
			d.spare = append(d.spare, entries)
		}
	}
	clear(d.byField)
	clear(d.written)
	d.written = d.written[:0]
	clear(d.columns[:cap(d.columns)])
	d.columns = d.columns[:0]
	d.read = nil
	if !keepPooled(cap(d.ids), d.usedIDs) {
		d.ids = nil
	}
	d.ids, d.usedIDs = d.ids[:0], 0
	dictPool.Put(d)
}

// dictField names a column field: its row type's plan and its key.
//
// The plan is held by address rather than by pointer. Plans are cached for the
// life of the process and the collector does not move them, so the address is
// as good a name; and a pointer stored in the map would make every table
// writer's plan parameter leak, which is a change to the ordinary path's
// escape analysis that this path has no business making.
type dictField struct {
	plan uintptr
	key  uint8
}

// appendDictRecord is appendRecord on the dictionary path.
func appendDictRecord(dst []byte, root byte, plan *typePlan, record unsafe.Pointer) ([]byte, error) {
	dict := newDictionaries()
	defer dict.release()
	buf := scratch{dict: dict}
	start := len(dst)
	out, err := appendRunInto(append(dst, root), plan, record, &buf)
	if err != nil {
		return dst, err
	}
	if buf.grouped {
		out[start] |= rootGrouped
	}
	if len(dict.written) == 0 {
		return out, nil
	}
	out[start] |= rootDictionary
	return dict.insertSection(out, start+1), nil
}

// dictColumn is one string column of a table on its way through tableIDs.
// A column not refused goes out as ids into dictionary number.
type dictColumn struct {
	entries *dictEntries
	// values are the column's strings, one per row or, for a pointer column,
	// one per row present; ids has one for each.
	values  []string
	ids     []int64
	number  int
	refused bool
	// before is how many entries the dictionary had when the column began,
	// which a refusal rolls back to; newLimit is where it stops taking new ones.
	before, newLimit int
	// raw is what the strings would cost, and added what the new entries do.
	raw, added int
	nonEmpty   bool
}

// tableIDs decides, for each of a table's string columns, whether it goes out
// as ids, and gives them if it does: when its strings repeat enough that the
// new dictionary entries and the ids cost less than the strings.
//
// strings is the table's string columns back to back, rows values each, in
// field order, as gatherTable leaves them, and nulls its pointer columns as
// gatherPointers does; the result has one entry per string column, pointer or
// not, in field order, and is good until the next call. The columns are
// walked dictScanChunk rows at a time.
//
// Reading the strings straight from the rows, with a stride, instead of from
// the gathered headers saved their copy and measured no faster on TPC-H.
func (d *dictionaries) tableIDs(plan *typePlan, strings []string, nulls []nullColumn, rows int) []dictColumn {
	columns := d.columns[:0]
	need := 0
	for index := range plan.fields {
		field := &plan.fields[index]
		var values []string
		switch field.op {
		case opString:
			values, strings = strings[:rows:rows], strings[rows:]
		case opPointer:
			null := &nulls[0]
			nulls = nulls[1:]
			if field.elemOp != opString {
				continue
			}
			// The rows present only, which is what a nullable column's ids
			// stand for (wire.NullableDictColumn).
			values = null.strings
		default:
			continue
		}
		columns = append(columns, dictColumn{entries: d.entriesFor(plan, field.key), values: values})
		need += len(values)
	}
	if cap(d.ids) < need {
		d.ids = make([]int64, need)
	}
	d.usedIDs = max(d.usedIDs, need)
	ids := d.ids[:need]
	for at := range columns {
		column := &columns[at]
		n := len(column.values)
		column.ids, ids = ids[:n:n], ids[n:]
		column.before = len(column.entries.values)
		// A column whose rows are more than half new values has nothing to
		// sell, and finding that out early saves hashing the rest of it.
		column.newLimit = column.before + n/2
		column.refused = n == 0
	}
	for start := 0; start < rows; start += dictScanChunk {
		for at := range columns {
			if column := &columns[at]; !column.refused && start < len(column.values) {
				column.scan(column.values[start:min(start+dictScanChunk, len(column.values))], start)
			}
		}
	}
	for at := range columns {
		d.decide(&columns[at])
	}
	d.columns = columns
	return columns
}

// entriesFor is the dictionary of a column field, made the first time a
// table asks for it.
func (d *dictionaries) entriesFor(plan *typePlan, key uint8) *dictEntries {
	field := dictField{uintptr(unsafe.Pointer(plan)), key}
	entries := d.byField[field]
	if entries == nil {
		if d.byField == nil {
			d.byField = map[dictField]*dictEntries{}
		}
		if last := len(d.spare) - 1; last >= 0 {
			entries, d.spare = d.spare[last], d.spare[:last]
		} else {
			entries = newDictEntries()
		}
		d.byField[field] = entries
	}
	return entries
}

// scan looks up one chunk of a column, the rows from start on.
//
// Where there is a dictKernel it takes the run of rows it can — short strings
// the dictionary already holds, which is nearly every row of a column worth a
// dictionary — and this loop takes one row the kernel stopped at, and hands
// back. A kernel adds no entries, so the sample check it skipped over reads
// the same afterwards as it would have at its row.
func (c *dictColumn) scan(values []string, start int) {
	entries := c.entries
	for at := 0; at < len(values); {
		if useDictKernel && !entries.broken {
			done, raw, lengths := dictKernel(&values[at], len(values)-at, &entries.short[0],
				(len(entries.short)-1)<<5, uint(entries.shortShift), &c.ids[start+at])
			c.raw += raw
			c.nonEmpty = c.nonEmpty || lengths != 0
			from := start + at
			at += done
			if from < dictSampleRows && start+at >= dictSampleRows && c.sampleIsNew() {
				c.refuse()
				return
			}
			if at == len(values) {
				return
			}
		}
		if !c.add(values[at], start+at) {
			c.refuse()
			return
		}
		at++
		if start+at == dictSampleRows && c.sampleIsNew() {
			c.refuse()
			return
		}
	}
}

// sampleIsNew is whether nearly every row of the sample was a new value: a
// column of comments or names, whose half-way refusal would have hashed and
// then unhashed half of it.
func (c *dictColumn) sampleIsNew() bool {
	return len(c.entries.values)-c.before > dictSampleRows*7/8
}

// add looks up one value, adding it if it is new, and stores its id at row.
// It reports false when the column has to be refused.
func (c *dictColumn) add(value string, row int) bool {
	entries := c.entries
	c.raw += lengthBytes(len(value)) + len(value)
	c.nonEmpty = c.nonEmpty || len(value) != 0
	lo, hi := shortKey(value)
	id, found, ok := entries.find(value, lo, hi)
	if !ok {
		return false
	}
	if !found {
		if len(entries.values) >= c.newLimit || len(entries.values) >= maxDictEntries {
			return false
		}
		id = uint32(len(entries.values))
		if !entries.insert(value, lo, hi) {
			return false
		}
		c.added += lengthBytes(len(value)) + len(value)
	}
	c.ids[row] = int64(id)
	return true
}

func (c *dictColumn) refuse() {
	c.entries.rollback(c.before)
	c.refused = true
}

// decide weighs a scanned column's ids and new entries against its strings,
// refusing it when they do not pay, and numbers its dictionary the first time
// a column uses it.
func (d *dictionaries) decide(c *dictColumn) {
	if c.refused {
		return
	}
	entries := c.entries
	number := entries.number
	cost := c.added + idBytes(len(c.ids), len(entries.values))
	if number < 0 {
		number = len(d.written)
		cost += lengthBytes(len(entries.values)) // the dictionary's own count
	}
	cost += lengthBytes(number)
	if !c.nonEmpty || cost >= c.raw {
		c.refuse()
		return
	}
	if entries.number < 0 {
		entries.number = number
		d.written = append(d.written, entries)
	}
	c.number = number
}

// idBytes estimates a column of rows ids into n entries: the bits, a width byte
// per block of 128 and the column header.
func idBytes(rows, n int) int {
	width := bits.Len(uint(max(n, 1) - 1))
	return (rows*width+7)/8 + (rows+127)/128 + 1
}

// lengthBytes is what wire.AppendLength spends on n.
func lengthBytes(n int) int {
	if n >= 0 && n <= 0xFE {
		return 1
	}
	return 5
}

// insertSection writes the section into out at at, moving what follows up.
func (d *dictionaries) insertSection(out []byte, at int) []byte {
	size := lengthBytes(len(d.written))
	for _, entries := range d.written {
		size += lengthBytes(len(entries.values))
		for _, value := range entries.values {
			size += lengthBytes(len(value)) + len(value)
		}
	}
	total := lengthBytes(size) + size
	end := len(out)
	out = append(out, make([]byte, total)...)
	copy(out[at+total:], out[at:end])
	// The section is appended into the gap: it is exactly total bytes long, so
	// it ends where the body now starts.
	section := wire.AppendLength(out[at:at], size)
	section = wire.AppendLength(section, len(d.written))
	for _, entries := range d.written {
		section = wire.AppendLength(section, len(entries.values))
		for _, value := range entries.values {
			section = append(wire.AppendLength(section, len(value)), value...)
		}
	}
	if len(section) != total {
		panic("colbin: dictionary section size miscounted")
	}
	return out
}

var (
	errDictSection = errors.New("colbin: malformed dictionary section")
	errDictNumber  = errors.New("colbin: a column names a dictionary the message does not hold")
	errDictID      = errors.New("colbin: a column holds an id past its dictionary's end")
)

// isDictRoot reports whether a root byte is one the dictionary path reads:
// either width, with no schema section.
func isDictRoot(data []byte) bool {
	return len(data) > 0 && data[0]&^(rootWide|rootGrouped) == RootFirst|rootDictionary
}

// parseDictSection reads the section at the front of data and returns its
// dictionaries and the body after it. Every string is a substring of one copy
// of the section, so a section costs one allocation for its text however many
// entries it has, and every row holding an entry shares it.
func parseDictSection(data []byte) ([][]string, []byte, error) {
	size, width, ok := wire.ReadLength(data)
	if !ok || size > len(data)-width {
		return nil, nil, errDictSection
	}
	section, body := data[width:width+size], data[width+size:]
	text := string(section)
	count, at, ok := wire.ReadLength(section)
	// Every dictionary is at least its count byte.
	if !ok || count > len(section)-at {
		return nil, nil, errDictSection
	}
	dicts := make([][]string, count)
	for number := range dicts {
		entries, width, ok := wire.ReadLength(section[at:])
		at += width
		// And every entry at least its size byte.
		if !ok || entries > len(section)-at {
			return nil, nil, errDictSection
		}
		values := make([]string, entries)
		for index := range values {
			length, width, ok := wire.ReadLength(section[at:])
			at += width
			if !ok || length > len(section)-at {
				return nil, nil, errDictSection
			}
			values[index] = text[at : at+length]
			at += length
		}
		dicts[number] = values
	}
	if at != len(section) {
		return nil, nil, errDictSection
	}
	return dicts, body, nil
}

// unmarshalDictionaryAs is unmarshalDictionary with its failure named for the
// type being decoded, as unmarshalBody names one.
func unmarshalDictionaryAs(data []byte, plan *typePlan, record unsafe.Pointer, what reflect.Type) error {
	if err := unmarshalDictionary(data, plan, record); err != nil {
		return errDecoding(what, err)
	}
	return nil
}

// unmarshalDictionary decodes a message with a dictionary section into an
// already-zeroed record. It is unmarshalBody with the section read first.
func unmarshalDictionary(data []byte, plan *typePlan, record unsafe.Pointer) error {
	wide := data[0]&rootWide != 0
	dicts, body, err := parseDictSection(data[1:])
	if err != nil {
		return err
	}
	if plan.simple {
		// A flat type has no table to hold ids: the body reads as any other,
		// stepping over a column its type no longer declares.
		if wide {
			return unmarshalWide(body, nil, plan, record)
		}
		return unmarshalNarrow(body, plan, record)
	}
	dict := newDictionaries()
	defer dict.release()
	dict.read = dicts
	buf := scratch{rowsLeft: rowBudget(len(body)), dict: dict}
	buf.strings.Reset(len(body))
	defer buf.release()
	if readsByProgram(plan, wide) {
		return readByProgram(body, plan, record, &buf)
	}
	if wide {
		reader := wire.NewReader8(body)
		readRun(&reader, plan, record, &buf)
		return reader.Err()
	}
	reader := wire.NewReader(body)
	readNarrowRun(&reader, plan, record, &buf)
	return reader.Err()
}

// readDictColumn8 reads a dictionary column at eight key bits into slot,
// reporting whether it holds the table's rows.
func (d *dictionaries) readDictColumn8(columns *wire.Reader8, rows int, slot []string) bool {
	number, ids, ok := columns.DictColumn(rows, d.ids)
	if !ok {
		return false
	}
	d.ids, d.usedIDs = ids[:0], max(d.usedIDs, len(ids))
	return d.resolve(columns.Fail, number, ids, rows, slot)
}

// readDictColumn is readDictColumn8 at four key bits.
func (d *dictionaries) readDictColumn(columns *wire.Reader, rows int, slot []string) bool {
	number, ids, ok := columns.DictColumn(rows, d.ids)
	if !ok {
		return false
	}
	d.ids, d.usedIDs = ids[:0], max(d.usedIDs, len(ids))
	return d.resolve(columns.Fail, number, ids, rows, slot)
}

// dictColumnReader is what a pointer column written as ids is read through, at
// either key width.
type dictColumnReader interface {
	IsDictColumn() bool
	IsNullableDictColumn() bool
	DictColumn(rows int, dst []int64) (int, []int64, bool)
	NullableDictColumn(rows int, dst []int64) (int, []byte, []int64, bool)
	Fail(err error)
}

// readDictPointer reads a pointer string column written as ids, in either
// form, and points each row present at its string. It reads nothing and
// reports false when the column at the cursor is not one.
func (d *dictionaries) readDictPointer(
	columns dictColumnReader, field *planField, data unsafe.Pointer, stride uintptr, rows int,
) bool {
	var number int
	var presence []byte
	var ids []int64
	var ok bool
	switch {
	case columns.IsDictColumn():
		number, ids, ok = columns.DictColumn(rows, d.ids)
	case columns.IsNullableDictColumn():
		number, presence, ids, ok = columns.NullableDictColumn(rows, d.ids)
	default:
		return false
	}
	if !ok {
		return true
	}
	d.ids, d.usedIDs = ids[:0], max(d.usedIDs, len(ids))
	// The array every row of the column points into, as readPointerColumn's.
	values := make([]string, len(ids))
	want := rows
	if presence != nil {
		want = len(ids) // the presence's count, which the column was decoded to
	}
	if d.resolve(columns.Fail, number, ids, want, values) {
		pointRows(values, presence, data, stride, field.offset, rows)
	}
	return true
}

// resolve turns ids into their dictionary's strings, in slot.
func (d *dictionaries) resolve(fail func(error), number int, ids []int64, rows int, slot []string) bool {
	if number >= len(d.read) {
		fail(errDictNumber)
		return false
	}
	if len(ids) != rows {
		fail(errColumnLength)
		return false
	}
	values := d.read[number]
	out := slot[:rows]
	for row, id := range ids {
		if uint64(id) >= uint64(len(values)) {
			fail(errDictID)
			return false
		}
		out[row] = values[id]
	}
	return true
}
