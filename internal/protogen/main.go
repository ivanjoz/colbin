// Command protogen generates the protobuf twins of the corpus tables without
// protoc.
//
// # Why this exists
//
// The corpus needs a protobuf comparison, and `protoc` is not installed here.
// It is also not needed: protoc's only job in the pipeline is turning `.proto`
// text into a FileDescriptorProto, and a descriptor is an ordinary protobuf
// message that can be built directly. `protoc-gen-go` is a plain program that
// reads a CodeGeneratorRequest on stdin and writes a CodeGeneratorResponse on
// stdout, and it ships inside the `google.golang.org/protobuf` module this repo
// already depends on.
//
// So: build the descriptor here, pipe it through the plugin from the module
// cache, write the result. No protoc, no network, no new dependency.
//
// # The catch
//
// This file is the source of truth for corpus.proto rather than the other way
// round, because nothing here can parse `.proto` text. corpus.proto is written
// out alongside the Go code as documentation of what was built, and is not read
// back. If the two ever disagree, this file is right.
//
//	go run ./internal/protogen
package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/pluginpb"
)

const (
	protoPackage = "colbin.corpus"
	goPackage    = "github.com/ivanjoz/colbin/bench"
	protoFile    = "corpus.proto"
	outputDir    = "bench"
)

// field kinds, shortened so the message definitions below read like a .proto.
const (
	u32 = descriptorpb.FieldDescriptorProto_TYPE_UINT32
	u64 = descriptorpb.FieldDescriptorProto_TYPE_UINT64
	i64 = descriptorpb.FieldDescriptorProto_TYPE_INT64
	str = descriptorpb.FieldDescriptorProto_TYPE_STRING
	bln = descriptorpb.FieldDescriptorProto_TYPE_BOOL
	f64 = descriptorpb.FieldDescriptorProto_TYPE_DOUBLE
	msg = descriptorpb.FieldDescriptorProto_TYPE_MESSAGE
)

type fieldSpec struct {
	name     string
	number   int32
	kind     descriptorpb.FieldDescriptorProto_Type
	repeated bool
	typeName string // for message fields, fully qualified
}

type messageSpec struct {
	name   string
	fields []fieldSpec
}

// The corpus tables, mirroring corpus/corpus.go field for field and number for
// number.
//
// Cents are `int64` and not `sint64` on purpose. sint64 zigzags, which costs a
// bit, and every amount in this corpus is non-negative — so int64 is the form
// protobuf encodes these smallest in, and the comparison should give protobuf
// its best rather than a handicap.
var messages = []messageSpec{
	{"CorpusUser", []fieldSpec{
		{name: "id", number: 1, kind: u32},
		{name: "name", number: 2, kind: str},
		{name: "email", number: 3, kind: str},
		{name: "country", number: 4, kind: str},
		{name: "age", number: 5, kind: u32}, // proto has no uint8
		{name: "active", number: 6, kind: bln},
		{name: "created_at", number: 7, kind: i64},
	}},
	{"CorpusProduct", []fieldSpec{
		{name: "id", number: 1, kind: u32},
		{name: "sku", number: 2, kind: str},
		{name: "name", number: 3, kind: str},
		{name: "price_cents", number: 4, kind: i64},
		{name: "stock", number: 5, kind: u32},
		{name: "category_id", number: 6, kind: u32},
		{name: "tags", number: 7, kind: str, repeated: true},
	}},
	{"CorpusCategory", []fieldSpec{
		{name: "id", number: 1, kind: u32},
		{name: "name", number: 2, kind: str},
		{name: "parent_id", number: 3, kind: u32},
	}},
	{"CorpusStore", []fieldSpec{
		{name: "id", number: 1, kind: u32},
		{name: "code", number: 2, kind: str},
		{name: "city", number: 3, kind: str},
		{name: "lat", number: 4, kind: f64},
		{name: "lon", number: 5, kind: f64},
	}},
	{"CorpusSaleLine", []fieldSpec{
		{name: "product_id", number: 1, kind: u32},
		{name: "quantity", number: 2, kind: u32},
		{name: "unit_cents", number: 3, kind: i64},
		{name: "discount_bp", number: 4, kind: u32},
		{name: "tax_bp", number: 5, kind: u32},
		{name: "total_cents", number: 6, kind: i64},
	}},
	{"CorpusSale", []fieldSpec{
		{name: "id", number: 1, kind: u64},
		{name: "user_id", number: 2, kind: u32},
		{name: "store_id", number: 3, kind: u32},
		{name: "created_at", number: 4, kind: i64},
		{name: "subtotal_cents", number: 5, kind: i64},
		{name: "tax_cents", number: 6, kind: i64},
		{name: "total_cents", number: 7, kind: i64},
		{name: "paid_cents", number: 8, kind: i64},
		{name: "detail", number: 9, kind: msg, repeated: true,
			typeName: "." + protoPackage + ".CorpusSaleLine"},
	}},
	{"CorpusMetric", []fieldSpec{
		{name: "series_id", number: 1, kind: u32},
		{name: "at", number: 2, kind: i64},
		{name: "value", number: 3, kind: i64},
	}},
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "protogen:", err)
		os.Exit(1)
	}
}

func run() error {
	descriptor := buildFile()
	response, err := plugin(descriptor)
	if err != nil {
		return err
	}
	if response.Error != nil && *response.Error != "" {
		return fmt.Errorf("protoc-gen-go: %s", *response.Error)
	}
	for _, file := range response.File {
		path := outputDir + "/" + file.GetName()
		if err := os.WriteFile(path, []byte(file.GetContent()), 0o644); err != nil {
			return err
		}
		fmt.Println("wrote", path)
	}
	return writeProtoText()
}

func buildFile() *descriptorpb.FileDescriptorProto {
	file := &descriptorpb.FileDescriptorProto{
		Name:    proto.String(protoFile),
		Package: proto.String(protoPackage),
		Syntax:  proto.String("proto3"),
		Options: &descriptorpb.FileOptions{GoPackage: proto.String(goPackage)},
	}
	for _, spec := range messages {
		message := &descriptorpb.DescriptorProto{Name: proto.String(spec.name)}
		for _, field := range spec.fields {
			label := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL
			if field.repeated {
				label = descriptorpb.FieldDescriptorProto_LABEL_REPEATED
			}
			entry := &descriptorpb.FieldDescriptorProto{
				Name:     proto.String(field.name),
				Number:   proto.Int32(field.number),
				Label:    label.Enum(),
				Type:     field.kind.Enum(),
				JsonName: proto.String(jsonName(field.name)),
			}
			if field.typeName != "" {
				entry.TypeName = proto.String(field.typeName)
			}
			message.Field = append(message.Field, entry)
		}
		file.MessageType = append(file.MessageType, message)
	}
	return file
}

// plugin runs protoc-gen-go out of the module cache with the request on stdin.
func plugin(file *descriptorpb.FileDescriptorProto) (*pluginpb.CodeGeneratorResponse, error) {
	request := &pluginpb.CodeGeneratorRequest{
		FileToGenerate: []string{protoFile},
		Parameter:      proto.String("paths=source_relative"),
		ProtoFile:      []*descriptorpb.FileDescriptorProto{file},
	}
	payload, err := proto.Marshal(request)
	if err != nil {
		return nil, err
	}
	command := exec.Command("go", "run", "google.golang.org/protobuf/cmd/protoc-gen-go")
	command.Stdin = bytes.NewReader(payload)
	command.Stderr = os.Stderr
	out, err := command.Output()
	if err != nil {
		return nil, fmt.Errorf("running protoc-gen-go: %w", err)
	}
	response := &pluginpb.CodeGeneratorResponse{}
	if err := proto.Unmarshal(out, response); err != nil {
		return nil, err
	}
	return response, nil
}

// writeProtoText emits the .proto for a human to read. Nothing reads it back —
// see the note at the top of this file.
func writeProtoText() error {
	var text bytes.Buffer
	fmt.Fprintf(&text, "syntax = \"proto3\";\n\npackage %s;\n\n", protoPackage)
	fmt.Fprintf(&text, "option go_package = \"%s\";\n\n", goPackage)
	fmt.Fprintf(&text, "// Generated by internal/protogen. That program is the source of\n")
	fmt.Fprintf(&text, "// truth; this file is documentation and is never parsed.\n\n")
	for _, spec := range messages {
		fmt.Fprintf(&text, "message %s {\n", spec.name)
		for _, field := range spec.fields {
			label := ""
			if field.repeated {
				label = "repeated "
			}
			fmt.Fprintf(&text, "  %s%s %s = %d;\n",
				label, protoTypeName(field), field.name, field.number)
		}
		fmt.Fprintf(&text, "}\n\n")
	}
	return os.WriteFile(outputDir+"/"+protoFile, text.Bytes(), 0o644)
}

func protoTypeName(field fieldSpec) string {
	if field.kind == msg {
		return field.typeName[len(protoPackage)+2:]
	}
	switch field.kind {
	case u32:
		return "uint32"
	case u64:
		return "uint64"
	case i64:
		return "int64"
	case str:
		return "string"
	case bln:
		return "bool"
	case f64:
		return "double"
	}
	return "?"
}

// jsonName is protoc's lowerCamelCase of a snake_case field name.
func jsonName(name string) string {
	var out []byte
	upper := false
	for index := range len(name) {
		char := name[index]
		if char == '_' {
			upper = true
			continue
		}
		if upper && char >= 'a' && char <= 'z' {
			char -= 'a' - 'A'
		}
		upper = false
		out = append(out, char)
	}
	return string(out)
}
