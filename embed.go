package main

import (
	"embed"
	"io/fs"
)

// sourceFS carries this program's own Go source, so that `pack --target` can
// build an mpack for the target platform without a source checkout. The
// pattern is a glob rather than a list of names: naming the files meant that
// adding one left it out of the embedded copy, and the unpacker build then
// failed on an undefined symbol. The test files are embedded too (the
// pattern cannot exclude them) but are skipped when the source is written
// out; they add a few dozen kilobytes.
//
//go:embed go.mod *.go internal/mpack
var sourceFS embed.FS

// embeddedSource is sourceFS as a plain fs.FS.
func embeddedSource() fs.FS { return sourceFS }
