// Package comparison contains the shared Colbin/Protocol Buffers test corpus.
package comparison

// Install protoc and the pinned Go plugin before regenerating:
//
//   go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.12
//
//go:generate protoc --go_out=. --go_opt=paths=source_relative examples.proto
