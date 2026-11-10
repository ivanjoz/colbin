package colbin_test

import (
	"fmt"

	"github.com/ivanjoz/colbin"
)

// A slice of ids: sorted or clustered ones cost a few bits each.
func ExampleEncodeSlice() {
	ids := make([]int32, 200)
	for index := range ids {
		ids[index] = int32(500_000 + index*3)
	}
	encoded := colbin.EncodeSlice(ids)
	back, err := colbin.DecodeSlice[int32](encoded)

	fmt.Println(len(encoded), "bytes for", len(ids), "ids; 800 raw")
	fmt.Println(err, back[0], back[199])
	// Output:
	// 88 bytes for 200 ids; 800 raw
	// <nil> 500000 500597
}

// Prices are floats that are really decimals: stored as integers, and read
// back bit for bit.
func ExampleEncodeSlice_prices() {
	prices := []float32{12.5, 9.99, 0.1, 100, 3.25, 7.75, 12.5, 49.9}
	encoded := colbin.EncodeSlice(prices)
	back, _ := colbin.DecodeSlice[float32](encoded)

	fmt.Println(len(encoded), "bytes for", len(prices), "prices; 32 raw")
	fmt.Println(back)
	// Output:
	// 23 bytes for 8 prices; 32 raw
	// [12.5 9.99 0.1 100 3.25 7.75 12.5 49.9]
}

// A value that is not a decimal costs its bits and nothing else's: NaN, -0,
// infinities and 1/3 come back exactly, next to values that are decimals.
func ExampleEncodeSlice_exceptions() {
	values := []float64{1.25, 2.5, 1.0 / 3, 4.75}
	encoded := colbin.EncodeSlice(values)
	back, _ := colbin.DecodeSlice[float64](encoded)

	fmt.Println(back[2] == 1.0/3, back)
	// Output:
	// true [1.25 2.5 0.3333333333333333 4.75]
}

// What the slice holds decides the size, and the type the code declares it as
// does not: an []int16 reads back as an []int32 when a field is widened.
func ExampleDecodeSlice() {
	stored := colbin.EncodeSlice([]int16{-5, 300, 32000})

	wide, err := colbin.DecodeSlice[int32](stored)
	fmt.Println(wide, err)

	// A value that does not fit is an error, never a truncation.
	_, err = colbin.DecodeSlice[int8](stored)
	fmt.Println(err)
	// Output:
	// [-5 300 32000] <nil>
	// column: value out of range for the element type
}

// A named type works as its underlying one, and AppendSlice writes into a
// buffer the caller already has.
func ExampleAppendSlice() {
	type storeID int32
	buffer := []byte("prefix:")
	buffer = colbin.AppendSlice(buffer, []storeID{3, 4, 5, 6})

	back, err := colbin.DecodeSlice[storeID](buffer[len("prefix:"):])
	fmt.Println(back, err)
	// Output:
	// [3 4 5 6] <nil>
}

// The bytes carry no type tag and no header: an empty slice is the single
// byte of its count, and a reader must know what the field holds.
func ExampleDecodeSlice_empty() {
	encoded := colbin.EncodeSlice([]int64(nil))
	back, err := colbin.DecodeSlice[int64](encoded)

	fmt.Println(encoded, back == nil, len(back), err)
	// Output:
	// [0] false 0 <nil>
}
