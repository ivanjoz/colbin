//go:build linux

package codec

import (
	"bytes"
	"reflect"
	"syscall"
	"testing"
	"unsafe"
)

// dictKernel loads sixteen bytes from a string's start. Strings that start in
// a page's last sixteen bytes, with nothing mapped after the page, must be
// left to Go: a load past the string there would fault, and this test with it.
func TestDictionaryKernelPageEnd(t *testing.T) {
	pageSize := syscall.Getpagesize()
	memory, err := syscall.Mmap(-1, 0, 2*pageSize,
		syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_ANON|syscall.MAP_PRIVATE)
	if err != nil {
		t.Skip(err)
	}
	defer syscall.Munmap(memory)
	if err := syscall.Mprotect(memory[pageSize:], syscall.PROT_NONE); err != nil {
		t.Skip(err)
	}
	page := memory[:pageSize]
	for at := range page {
		page[at] = 'A' + byte(at%26)
	}
	// Every string that ends exactly at the page's end, from one byte to
	// sixteen, and one-byte strings at each of the last sixteen bytes.
	var values []string
	for length := 1; length <= 16; length++ {
		values = append(values, unsafe.String(&page[pageSize-length], length))
	}
	for at := pageSize - 16; at < pageSize; at++ {
		values = append(values, unsafe.String(&page[at], 1))
	}
	rows := make([]dictRow, 4000)
	for index := range rows {
		rows[index] = dictRow{ID: int32(index), Status: values[index%len(values)],
			Country: values[(index/7)%len(values)]}
	}
	value := dictHolder{Rows: rows}
	var withKernel, withoutKernel []byte
	withDictionary(t, func() {
		withKernel = must(Marshal(&value))
		was := setDictKernel(false)
		withoutKernel = must(Marshal(&value))
		setDictKernel(was)
	})
	if !bytes.Equal(withKernel, withoutKernel) {
		t.Fatal("the kernel's message differs from the Go loop's")
	}
	if withKernel[0]&rootDictionary == 0 {
		t.Fatal("no dictionary was used")
	}
	var back dictHolder
	if err := Unmarshal(withKernel, &back); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back.Rows, rows) {
		t.Fatal("round-tripped wrong")
	}
}
