package codec

import (
	"bytes"
	"math/rand/v2"
	"strings"
	"testing"
)

// The tree program's types: value structs inside value structs, around every
// leaf it writes itself, the runs it hands the kernel, and the fields it hands
// writeFields — a slice of structs, a pointer to one, a map, a slice behind a
// pointer, and a struct of a wide type.
type treeLeaf struct {
	On    bool    `cb:"1"`
	Count int16   `cb:"2"`
	Name  *string `cb:"3"`
	Ratio float32 `cb:"4"`
}

type treeStrings struct {
	A string `cb:"1"`
	B string `cb:"2"`
	C string `cb:"3"`
	D string `cb:"4"`
	E int64  `cb:"5"`
}

type treeWide struct {
	X uint32 `cb:"200"`
}

type treeMiddle struct {
	Leaf    treeLeaf          `cb:"1"`
	Strings treeStrings       `cb:"2"`
	Rows    []treeLeaf        `cb:"3"`
	Next    *treeLeaf         `cb:"4"`
	Tags    map[string]uint32 `cb:"5"`
	Many    *[]int64          `cb:"6"`
	Wide    treeWide          `cb:"7"`
	Size    uint64            `cb:"8"`
	Rules   []treePointers    `cb:"9"`
}

// treePointers is flat, but of pointers alone: no row program, and so a tree.
type treePointers struct {
	Level   *uint32   `cb:"1"`
	When    *string   `cb:"2"`
	Options *treeLeaf `cb:"3"`
}

type treeRoot struct {
	Version float64    `cb:"1"`
	Middle  treeMiddle `cb:"2"`
	Level   *uint32    `cb:"3"`
	Deep    treeDeep1  `cb:"4"`
}

// A chain deeper than maxTreeDepth, whose last levels are written by calls.
type treeDeep1 struct {
	Next treeDeep2 `cb:"1"`
}
type treeDeep2 struct {
	Next treeDeep3 `cb:"1"`
}
type treeDeep3 struct {
	Next treeDeep4 `cb:"1"`
}
type treeDeep4 struct {
	Next treeDeep5 `cb:"1"`
}
type treeDeep5 struct {
	Next treeDeep6 `cb:"1"`
}
type treeDeep6 struct {
	Next treeDeep7 `cb:"1"`
}
type treeDeep7 struct {
	Next treeDeep8 `cb:"1"`
}
type treeDeep8 struct {
	Next treeDeep9 `cb:"1"`
}
type treeDeep9 struct {
	Value int32 `cb:"1"`
}

// treePure is a tree with no call in it, which appendRecord writes on its own.
type treePure struct {
	Rules struct {
		Imports struct {
			Options struct {
				Grouped bool    `cb:"1"`
				Name    *string `cb:"2"`
			} `cb:"1"`
		} `cb:"1"`
		Text string `cb:"2"`
	} `cb:"1"`
}

func makeTreeRoot(random *rand.Rand) treeRoot {
	text := func() string {
		// Now and then long enough that a struct's body outgrows its length
		// byte, which is the tree's one call into wire's Close.
		return strings.Repeat("x", random.IntN(4)*random.IntN(90))
	}
	leaf := func() treeLeaf {
		value := treeLeaf{On: random.IntN(2) == 0, Count: int16(random.IntN(600) - 300), Ratio: random.Float32()}
		if random.IntN(2) == 0 {
			name := text()
			value.Name = &name
		}
		return value
	}
	root := treeRoot{Version: random.Float64()}
	root.Middle.Leaf = leaf()
	root.Middle.Strings = treeStrings{A: text(), B: text(), C: text(), D: text(), E: random.Int64()}
	for range random.IntN(5) {
		root.Middle.Rows = append(root.Middle.Rows, leaf())
	}
	if random.IntN(2) == 0 {
		next := leaf()
		root.Middle.Next = &next
	}
	if random.IntN(2) == 0 {
		root.Middle.Tags = map[string]uint32{"a": random.Uint32(), "b": 7}
	}
	if random.IntN(2) == 0 {
		many := []int64{random.Int64(), -3}
		root.Middle.Many = &many
	}
	root.Middle.Wide.X = uint32(random.IntN(3))
	root.Middle.Size = random.Uint64() >> random.IntN(64)
	for range random.IntN(4) {
		var rule treePointers
		if random.IntN(2) == 0 {
			level := uint32(random.IntN(3))
			rule.Level = &level
		}
		if random.IntN(2) == 0 {
			when := text()
			rule.When = &when
		}
		if random.IntN(2) == 0 {
			options := leaf()
			rule.Options = &options
		}
		root.Middle.Rules = append(root.Middle.Rules, rule)
	}
	if random.IntN(2) == 0 {
		level := uint32(random.IntN(1000))
		root.Level = &level
	}
	root.Deep.Next.Next.Next.Next.Next.Next.Next.Next.Value = int32(random.IntN(100) - 50)
	return root
}

func TestTreeProgramWritesWhatWriteFieldsDoes(t *testing.T) {
	if tree := MustCodec[treeRoot]().plan.tree; tree == nil || tree.pure {
		t.Fatal("treeRoot compiled to no tree, or to a pure one")
	}
	if tree := MustCodec[treePure]().plan.tree; tree == nil || !tree.pure {
		t.Fatal("treePure compiled to no pure tree")
	}
	if MustCodec[treePointers]().plan.tree == nil {
		t.Fatal("treePointers compiled to no tree")
	}
	random := rand.New(rand.NewPCG(31, 32))
	for trial := range 2000 {
		value := makeTreeRoot(random)
		useRowPrograms = false
		want, wantErr := Marshal(&value)
		useRowPrograms = true
		for _, kernel := range []bool{false, true} {
			was := setRowKernel(kernel)
			got, gotErr := Marshal(&value)
			setRowKernel(was)
			if (wantErr != nil) != (gotErr != nil) || !bytes.Equal(got, want) {
				t.Fatalf("trial %d, kernel %v: tree %d bytes (%v), writeFields %d bytes (%v)\n%+v",
					trial, kernel, len(got), gotErr, len(want), wantErr, value)
			}
		}
		var back treeRoot
		if err := Unmarshal(want, &back); err != nil {
			t.Fatalf("trial %d: %v", trial, err)
		}
		again, _ := Marshal(&back)
		if !bytes.Equal(again, want) {
			t.Fatalf("trial %d: decoded and encoded again to other bytes", trial)
		}

		var pure treePure
		pure.Rules.Imports.Options.Grouped = random.IntN(2) == 0
		pure.Rules.Text = strings.Repeat("y", random.IntN(300))
		if random.IntN(2) == 0 {
			name := strings.Repeat("n", random.IntN(300))
			pure.Rules.Imports.Options.Name = &name
		}
		useRowPrograms = false
		want, _ = Marshal(&pure)
		useRowPrograms = true
		if got, _ := Marshal(&pure); !bytes.Equal(got, want) {
			t.Fatalf("trial %d, pure: tree %x, writeFields %x", trial, got, want)
		}

		// A flat tree at the root, its rules taken from the record's.
		for _, rule := range value.Middle.Rules {
			useRowPrograms = false
			want, _ = Marshal(&rule)
			useRowPrograms = true
			if got, _ := Marshal(&rule); !bytes.Equal(got, want) {
				t.Fatalf("trial %d, pointers: tree %x, writeFields %x", trial, got, want)
			}
		}
	}
}
