package main

// The message-level oracle (PLAN.md §7, level 2).
//
// It applies the inference rules of PLAN.md §3 to arbitrary JSON, builds a Go
// type that expresses the result, fills it, and hands it to the real
// colbin.MarshalJSON. The bytes that come back are what the AssemblyScript
// encoder must produce for the same input.
//
// The rules are implemented twice on purpose — here and in assembly/infer.ts —
// and the vectors are what keeps the two honest. Deriving one from the other
// would cost more than the drift it prevents (§12).

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"strconv"
	"strings"

	"github.com/ivanjoz/colbin"
)

// A value category, matching the five in assembly/infer.ts.
type category int

const (
	catNone category = iota
	catBool
	catNum
	catStr
	catArr
	catObj
)

var categoryNames = map[category]string{
	catBool: "bool", catNum: "number", catStr: "string", catArr: "array", catObj: "object",
}

// obs is what one position in the shape was observed to hold. Same two-pass
// split as the module: observe everything, then resolve.
type obs struct {
	cats    map[category]bool
	sawNull bool
	sawInt  bool
	sawNeg  bool
	sawUint bool
	sawFlt  bool

	elem    *obs
	fields  []*fieldObs
	byName  map[string]*fieldObs
	objects int
}

type fieldObs struct {
	name    string
	obs     *obs
	present int
}

func newObs() *obs {
	return &obs{cats: map[category]bool{}, byName: map[string]*fieldObs{}}
}

// jsonValue is a parsed JSON value that keeps integers exact. encoding/json
// into `any` would turn every number into a float64, which is the corruption
// the whole exercise is about.
type jsonValue struct {
	kind  string // null bool int uint float string array object
	b     bool
	i     int64
	u     uint64
	f     float64
	s     string
	arr   []jsonValue
	keys  []string
	items map[string]jsonValue
}

func parseValue(raw json.RawMessage) (jsonValue, error) {
	trimmed := strings.TrimSpace(string(raw))
	switch {
	case trimmed == "null":
		return jsonValue{kind: "null"}, nil
	case trimmed == "true":
		return jsonValue{kind: "bool", b: true}, nil
	case trimmed == "false":
		return jsonValue{kind: "bool"}, nil
	case strings.HasPrefix(trimmed, `"`):
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return jsonValue{}, err
		}
		return jsonValue{kind: "string", s: s}, nil
	case strings.HasPrefix(trimmed, "["):
		var items []json.RawMessage
		if err := json.Unmarshal(raw, &items); err != nil {
			return jsonValue{}, err
		}
		out := jsonValue{kind: "array"}
		for _, it := range items {
			v, err := parseValue(it)
			if err != nil {
				return jsonValue{}, err
			}
			out.arr = append(out.arr, v)
		}
		return out, nil
	case strings.HasPrefix(trimmed, "{"):
		// Key order is the document's, which the inference rules depend on, so
		// the object is walked with a token decoder rather than a map.
		dec := json.NewDecoder(strings.NewReader(trimmed))
		if _, err := dec.Token(); err != nil { // {
			return jsonValue{}, err
		}
		out := jsonValue{kind: "object", items: map[string]jsonValue{}}
		for dec.More() {
			keyTok, err := dec.Token()
			if err != nil {
				return jsonValue{}, err
			}
			key := keyTok.(string)
			var rawVal json.RawMessage
			if err := dec.Decode(&rawVal); err != nil {
				return jsonValue{}, err
			}
			v, err := parseValue(rawVal)
			if err != nil {
				return jsonValue{}, err
			}
			if _, seen := out.items[key]; !seen {
				out.keys = append(out.keys, key)
			}
			out.items[key] = v // a duplicate key overwrites, as JSON.parse does
		}
		return out, nil
	default:
		// A literal with no '.' and no exponent is an integer (PLAN.md §3.2).
		if !strings.ContainsAny(trimmed, ".eE") {
			if v, err := strconv.ParseInt(trimmed, 10, 64); err == nil {
				return jsonValue{kind: "int", i: v}, nil
			}
			if v, err := strconv.ParseUint(trimmed, 10, 64); err == nil {
				return jsonValue{kind: "uint", u: v}, nil
			}
			return jsonValue{}, fmt.Errorf("integer out of range: %s", trimmed)
		}
		v, err := strconv.ParseFloat(trimmed, 64)
		if err != nil || math.IsInf(v, 0) || math.IsNaN(v) {
			return jsonValue{}, fmt.Errorf("bad number: %s", trimmed)
		}
		return jsonValue{kind: "float", f: v}, nil
	}
}

func observe(o *obs, v jsonValue) {
	switch v.kind {
	case "null":
		o.sawNull = true
	case "bool":
		o.cats[catBool] = true
	case "int":
		o.cats[catNum] = true
		o.sawInt = true
		if v.i < 0 {
			o.sawNeg = true
		}
	case "uint":
		o.cats[catNum] = true
		o.sawInt = true
		o.sawUint = true
	case "float":
		o.cats[catNum] = true
		o.sawFlt = true
	case "string":
		o.cats[catStr] = true
	case "array":
		o.cats[catArr] = true
		if len(v.arr) > 0 && o.elem == nil {
			o.elem = newObs()
		}
		for _, e := range v.arr {
			observe(o.elem, e)
		}
	case "object":
		o.cats[catObj] = true
		o.objects++
		for _, k := range v.keys {
			f, ok := o.byName[k]
			if !ok {
				f = &fieldObs{name: k, obs: newObs()}
				o.byName[k] = f
				o.fields = append(o.fields, f) // first-seen order
			}
			if f.present < o.objects {
				f.present = o.objects
			}
			observe(f.obs, v.items[k])
		}
	}
}

// resolve turns an observation into a Go type, refusing the contradictions
// PLAN.md §4.1 refuses.
func resolve(o *obs, path string) (reflect.Type, error) {
	if len(o.cats) > 1 {
		names := []string{}
		for c := range o.cats {
			names = append(names, categoryNames[c])
		}
		return nil, fmt.Errorf("%s: type conflict between %s", path, strings.Join(names, " and "))
	}

	var base reflect.Type
	switch {
	case len(o.cats) == 0:
		base = reflect.TypeOf("") // only ever null
	case o.cats[catBool]:
		base = reflect.TypeOf(false)
	case o.cats[catNum]:
		switch {
		case o.sawFlt:
			base = reflect.TypeOf(float64(0))
		case o.sawUint && o.sawNeg:
			return nil, fmt.Errorf("%s: values span below zero and above the int64 maximum", path)
		case o.sawUint:
			base = reflect.TypeOf(uint64(0))
		default:
			base = reflect.TypeOf(int64(0))
		}
	case o.cats[catStr]:
		base = reflect.TypeOf("")
	case o.cats[catArr]:
		var elem reflect.Type
		if o.elem == nil {
			elem = reflect.TypeOf("") // always an empty array
		} else {
			var err error
			if elem, err = resolve(o.elem, path+"[]"); err != nil {
				return nil, err
			}
		}
		base = reflect.SliceOf(elem)
	case o.cats[catObj]:
		if len(o.fields) > 254 {
			return nil, fmt.Errorf("%s: more than 254 fields", path)
		}
		fields := make([]reflect.StructField, 0, len(o.fields))
		for i, f := range o.fields {
			ft, err := resolve(f.obs, path+"."+f.name)
			if err != nil {
				return nil, err
			}
			// A key missing from some record is nullable, exactly as an explicit
			// null is: the wire cannot tell them apart (PLAN.md §6).
			if f.present < o.objects && ft.Kind() != reflect.Pointer {
				ft = reflect.PointerTo(ft)
			}
			fields = append(fields, reflect.StructField{
				Name: fmt.Sprintf("F%d", i),
				Type: ft,
				Tag:  reflect.StructTag(fmt.Sprintf("cb:%q json:%q", f.name, f.name)),
			})
		}
		base = reflect.StructOf(fields)
	}

	if o.sawNull && base.Kind() != reflect.Pointer {
		base = reflect.PointerTo(base)
	}
	return base, nil
}

// fill writes v into dst, which resolve() produced for v's position.
func fill(dst reflect.Value, v jsonValue) error {
	t := dst.Type()
	if t.Kind() == reflect.Pointer {
		if v.kind == "null" {
			return nil // a nil pointer
		}
		p := reflect.New(t.Elem())
		if err := fill(p.Elem(), v); err != nil {
			return err
		}
		dst.Set(p)
		return nil
	}
	switch v.kind {
	case "null":
		return nil // the zero value; only reachable for a non-nullable position
	case "bool":
		dst.SetBool(v.b)
	case "int":
		if t.Kind() == reflect.Float64 {
			dst.SetFloat(float64(v.i))
		} else if t.Kind() == reflect.Uint64 {
			dst.SetUint(uint64(v.i))
		} else {
			dst.SetInt(v.i)
		}
	case "uint":
		if t.Kind() == reflect.Float64 {
			dst.SetFloat(float64(v.u))
		} else {
			dst.SetUint(v.u)
		}
	case "float":
		dst.SetFloat(v.f)
	case "string":
		dst.SetString(v.s)
	case "array":
		s := reflect.MakeSlice(t, len(v.arr), len(v.arr))
		for i, e := range v.arr {
			if err := fill(s.Index(i), e); err != nil {
				return err
			}
		}
		dst.Set(s)
	case "object":
		for i := 0; i < t.NumField(); i++ {
			name := t.Field(i).Tag.Get("json")
			item, ok := v.items[name]
			if !ok {
				continue // absent: leave the nil pointer resolve() gave it
			}
			if err := fill(dst.Field(i), item); err != nil {
				return err
			}
		}
	}
	return nil
}

// encodeJSON is the whole oracle: JSON text in, a colbin JSON-mode message out.
func encodeJSON(text string) ([]byte, error) {
	root, err := parseValue(json.RawMessage(text))
	if err != nil {
		return nil, err
	}

	records := []jsonValue{}
	single := false
	valueMode := false
	switch root.kind {
	case "array":
		if len(root.arr) == 0 {
			return nil, fmt.Errorf("an empty array has no shape to infer")
		}
		allObjects := true
		for _, e := range root.arr {
			if e.kind != "object" {
				allObjects = false
				break
			}
		}
		if !allObjects {
			// An array of anything else is one value, not a batch of records.
			valueMode = true
			records = []jsonValue{root}
		} else {
			records = root.arr
		}
	case "object":
		records = []jsonValue{root}
		single = true
	case "null":
		return nil, fmt.Errorf("null has no shape to infer")
	default:
		valueMode = true
		records = []jsonValue{root}
	}

	o := newObs()
	for _, r := range records {
		observe(o, r)
	}
	recordType, err := resolve(o, "")
	if err != nil {
		return nil, err
	}

	if single || valueMode {
		v := reflect.New(recordType).Elem()
		if err := fill(v, records[0]); err != nil {
			return nil, err
		}
		return colbin.MarshalJSON(v.Interface())
	}
	rows := reflect.MakeSlice(reflect.SliceOf(recordType), len(records), len(records))
	for i, r := range records {
		if err := fill(rows.Index(i), r); err != nil {
			return nil, err
		}
	}
	return colbin.MarshalJSON(rows.Interface())
}
