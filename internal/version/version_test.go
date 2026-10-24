package version

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The version is a constant, not an -ldflags variable, because the
// bin/ofpack inside an archive is compiled from embedded source that cannot
// receive linker flags. If this ever became a var again, the bundled binary
// would silently report a different version than the one that packed it.
func TestVersionIsACompiledInConstant(t *testing.T) {
	src, err := os.ReadFile("version.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	if !strings.Contains(body, "const Version =") {
		t.Error("Version must be a const; a var could be overwritten at link time in one build and not the other")
	}
	if strings.Contains(body, "var Version") {
		t.Error("Version is declared as a var")
	}
	// release-please rewrites the line between these markers.
	if !strings.Contains(body, "x-release-please-start-version") ||
		!strings.Contains(body, "x-release-please-end") {
		t.Error("the release-please markers are missing, so releases would not update the version")
	}
	if !regexp.MustCompile(`^\d+\.\d+\.\d+$`).MatchString(Version) {
		t.Errorf("Version = %q, want a bare semantic version such as 1.2.3", Version)
	}
}

// The manifest release-please reads has to agree with the constant, or a
// release would renumber from the wrong place.
func TestReleasePleaseManifestMatchesTheConstant(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", ".release-please-manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"`+Version+`"`) {
		t.Errorf(".release-please-manifest.json does not record %s:\n%s", Version, data)
	}
}
