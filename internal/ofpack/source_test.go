package ofpack

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

// The embedded source is what builds the unpacker for another platform.
// Listing files by name meant a newly added one was silently left out, and
// the omission only surfaced as an undefined symbol during a cross pack.
func TestSourceCompleteSpotsAMissingFile(t *testing.T) {
	src := fstest.MapFS{
		"go.mod":                      {Data: []byte("module ofpack\n")},
		"main.go":                     {Data: []byte("package main\n")},
		"internal/ofpack/manifest.go": {Data: []byte("package ofpack\n")},
	}
	if missing := SourceComplete(src, []string{"go.mod", "main.go"}); len(missing) != 0 {
		t.Errorf("unexpected missing files: %v", missing)
	}
	missing := SourceComplete(src, []string{"go.mod", "uv.go", "embed.go"})
	if len(missing) != 2 || missing[0] != "uv.go" {
		t.Fatalf("missing = %v, want [uv.go embed.go]", missing)
	}
}

// And the real embed pattern must cover every Go file in the main package.
func TestEveryRootGoFileIsEmbedded(t *testing.T) {
	entries, err := os.ReadDir("../..")
	if err != nil {
		t.Fatal(err)
	}
	var want []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".go") && !strings.HasSuffix(e.Name(), "_test.go") {
			want = append(want, e.Name())
		}
	}
	if len(want) == 0 {
		t.Skip("no root Go files found")
	}
	embedLine, err := os.ReadFile(filepath.Join("..", "..", "embed.go"))
	if err != nil {
		t.Fatal(err)
	}
	// A glob covers files added later; a list of names does not.
	if !strings.Contains(string(embedLine), "*.go") {
		t.Errorf("embed.go names files individually, so a new one would be left out:\n%s", embedLine)
	}
}
