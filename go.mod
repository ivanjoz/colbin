module github.com/ivanjoz/colbin

go 1.27

// Pin the patch release CI builds with. `go-version-file: go.mod` otherwise
// resolves to the newest 1.27.x, and go1.27.2 writes export data that
// staticcheck v0.8.1 cannot read, which failed go-checks on a change that had
// nothing to do with it. A toolchain line binds this module only; a module
// that imports colbin never sees it.
toolchain go1.27.0

// No require block, and no go.sum beside it. That is a deliberate property of
// this module, not an accident of nothing being needed yet: the benchmarks that
// wanted protobuf live in github.com/ivanjoz/colbin-benchmarks precisely so that
// importing colbin adds nothing to anyone's module graph.
//
// Before adding a dependency here, check whether it belongs in that repository
// instead. A test-only dependency is still a dependency: it never gets
// downloaded by a consumer, but it does become a version floor in their build
// and a line in their go.sum and their SBOM.
