package codec

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	"github.com/ivanjoz/colbin/internal/wirefmt"
)

// wideLine is a row type with an id past sixteen, so that a narrow parent's
// table of it is keyed at eight bits.
type wideLine struct {
	ProductID uint32 `cb:"2"`
	Note      string `cb:"5"`
	Price     int64  `cb:"20"`
}

type wideLineBasket struct {
	Lines []wideLine `cb:"2"`
	ID    uint64     `cb:"3"`
}

func wideLines(count int) []wideLine {
	if count == 0 {
		return nil
	}
	built := make([]wideLine, count)
	for index := range built {
		built[index] = wideLine{ProductID: uint32(index * 3), Note: fmt.Sprint("n", index%4),
			Price: int64(index%5) * -1999}
	}
	return built
}

// withGroupRows runs fn with tables grouped past rows rows.
func withGroupRows(rows int, fn func()) {
	defer func(was int) { wirefmt.TableGroupRows = was }(wirefmt.TableGroupRows)
	wirefmt.TableGroupRows = rows
	fn()
}

// A table past the group size is written in groups, at every key width a table
// takes, and every reader gives back what a table written whole gives: the
// typed decoder fresh and into a used value, the JSON walk, DecodeAny and a
// self-describing message. Cut short anywhere, it fails.
func TestGroupedTablesReadAsTables(t *testing.T) {
	for _, dictionary := range []bool{false, true} {
		SetDictionary(dictionary)
		for _, groupRows := range []int{1, 7, 8, 64} {
			for _, count := range []int{8, 9, 63, 64, 65, 200} {
				name := fmt.Sprintf("dictionary %v, %d rows in groups of %d", dictionary, count, groupRows)
				withGroupRows(groupRows, func() {
					checkGrouped(t, name, &basket{ID: 9, Lines: lines(count)}, &basket{ID: 1, Lines: lines(count + 5)}, count > groupRows)
					checkGrouped(t, name+", wide parent", &wideBasket{ID: 9, Lines: lines(count)}, &wideBasket{Lines: lines(3)}, count > groupRows)
					checkGrouped(t, name+", wide rows", &wideLineBasket{ID: 9, Lines: wideLines(count)}, &wideLineBasket{Lines: wideLines(count * 2)}, count > groupRows)
				})
			}
		}
	}
	SetDictionary(false)
}

func checkGrouped[T any](t *testing.T, name string, value, used *T, grouped bool) {
	t.Helper()
	message := must(Marshal(value))
	var fresh T
	if err := Unmarshal(message, &fresh); err != nil || !reflect.DeepEqual(&fresh, value) {
		t.Fatalf("%s: decoded as %+v, %v", name, fresh, err)
	}
	if err := MustCodec[T]().Unmarshal(message, used); err != nil || !reflect.DeepEqual(used, value) {
		t.Fatalf("%s: into a used value: %v", name, err)
	}
	// Without its ID the table is the message's last field, so a cut anywhere
	// is a cut inside it.
	tableOnly := *value
	reflect.ValueOf(&tableOnly).Elem().FieldByName("ID").SetZero()
	whole := must(Marshal(&tableOnly))
	// A cut at a dictionary section's end is an empty body, which is a record.
	start := 2
	if isDictRoot(whole) {
		_, body, err := parseDictSection(whole[1:])
		if err != nil {
			t.Fatal(err)
		}
		start = len(whole) - len(body) + 1
	}
	for cut := start; cut < len(whole); cut++ {
		var back T
		if Unmarshal(whole[:cut], &back) == nil {
			t.Fatalf("%s: a message cut at %d of %d decoded", name, cut, len(whole))
		}
	}
	if !isDictRoot(message) {
		matchesEncodingJSON(t, value)
		decoded, err := DecodeAny(mustSchema(t, value), message)
		if err != nil {
			t.Fatalf("%s: DecodeAny: %v", name, err)
		}
		if rows := decoded.(map[string]any)["Lines"].([]any); len(rows) != reflect.ValueOf(value).Elem().Field(0).Len() {
			t.Fatalf("%s: DecodeAny gave %d rows", name, len(rows))
		}
	}
	described := must(MarshalSelfDescribing(value))
	var back T
	if err := Unmarshal(described, &back); err != nil || !reflect.DeepEqual(&back, value) {
		t.Fatalf("%s: a self-describing message decoded as %+v, %v", name, back, err)
	}
	out, err := ToJSON(nil, described)
	if err != nil {
		t.Fatalf("%s: a self-describing message's JSON: %v", name, err)
	}
	sameJSON(t, out, must(json.Marshal(value)))
}
