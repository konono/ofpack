package mpack

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCheckArchiveNameRejectsHostilePaths(t *testing.T) {
	bad := []string{
		"",
		"/etc/passwd",
		"../escape",
		"installs/../../escape",
		"installs/./node",
		"installs//node",
		"a\\b",
		"a\x00b",
		".",
		"..",
	}
	for _, name := range bad {
		if _, err := CheckArchiveName(name); err == nil {
			t.Errorf("CheckArchiveName(%q) accepted a hostile path", name)
		}
	}
	// tar marks directories with a trailing slash; that is normalised away.
	if got, err := CheckArchiveName("installs/node/"); err != nil || got != "installs/node" {
		t.Errorf(`CheckArchiveName("installs/node/") = %q, %v; want "installs/node"`, got, err)
	}
	good := []string{"manifest.json", "installs/node/22.1.0/bin/node", "bin/mise"}
	for _, name := range good {
		if got, err := CheckArchiveName(name); err != nil || got != name {
			t.Errorf("CheckArchiveName(%q) = %q, %v; want it accepted unchanged", name, got, err)
		}
	}
}

func TestResolveUnderStaysInsideRoot(t *testing.T) {
	root := t.TempDir()
	got, err := ResolveUnder(root, "installs/node/bin/node")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(root, "installs/node/bin/node"); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if _, err := ResolveUnder(root, "../outside"); err == nil {
		t.Fatal("ResolveUnder accepted a path escaping the root")
	}
}

func TestRelUnder(t *testing.T) {
	root := filepath.Join(string(filepath.Separator), "a", "b")
	if rel, ok := RelUnder(root, filepath.Join(root, "c", "d")); !ok || rel != "c/d" {
		t.Fatalf("got %q, %v; want c/d, true", rel, ok)
	}
	// "/a/bc" must not count as being under "/a/b".
	if _, ok := RelUnder(root, filepath.Join(string(filepath.Separator), "a", "bc")); ok {
		t.Fatal("RelUnder treated a sibling with a shared prefix as a child")
	}
}

func TestMkdirAllNoSymlinkRefusesToDescendThroughSymlink(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := MkdirAllNoSymlink(root, "link/sub", 0o755); err == nil {
		t.Fatal("MkdirAllNoSymlink walked through a symlink")
	}
	if _, err := os.Stat(filepath.Join(outside, "sub")); err == nil {
		t.Fatal("a directory was created outside the root")
	}
	if err := MkdirAllNoSymlink(root, "real/sub", 0o755); err != nil {
		t.Fatalf("MkdirAllNoSymlink on a clean path: %v", err)
	}
}
