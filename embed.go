package main

import (
	"embed"
	"io/fs"
)

// sourceFS carries this program's own Go source, so that `pack --target` can
// build an mpack for the target platform without a source checkout. The test
// files are embedded too (the pattern cannot exclude them) but are skipped
// when the source is written out; they add a few dozen kilobytes.
//
//go:embed go.mod main.go embed.go internal/mpack
var sourceFS embed.FS

// embeddedSource is sourceFS as a plain fs.FS.
func embeddedSource() fs.FS { return sourceFS }
