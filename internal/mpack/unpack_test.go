package mpack

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func packToBytes(t *testing.T, p *Plan) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := p.Write(&buf); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestRoundTripPreservesContentPermissionsAndLinks(t *testing.T) {
	root := fakeSource(t, "node/22.1.0", "jq/1.8.1")
	// A relative link, and an absolute one that must survive relocation.
	if err := os.Symlink("../README", filepath.Join(root, "node/22.1.0/bin/readme")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink(filepath.Join(root, "jq/1.8.1/bin/jq"),
		filepath.Join(root, "node/22.1.0/bin/jq")); err != nil {
		t.Fatal(err)
	}

	plan, err := fakePlan(t, root, false, "node/22.1.0", "jq/1.8.1")
	if err != nil {
		t.Fatal(err)
	}
	archive := packToBytes(t, plan)

	dest := filepath.Join(t.TempDir(), "out")
	m, err := Unpack(bytes.NewReader(archive), UnpackOptions{Dest: dest})
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Tools) != 2 {
		t.Fatalf("got %d tools, want 2", len(m.Tools))
	}

	// Regular file content and the executable bit.
	binNode := filepath.Join(dest, "installs/node/22.1.0/bin/node")
	data, err := os.ReadFile(binNode)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "echo node") {
		t.Fatalf("unexpected content %q", data)
	}
	fi, err := os.Stat(binNode)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm()&0o111 == 0 {
		t.Fatalf("executable bit lost: %v", fi.Mode())
	}

	// The relative link is unchanged.
	got, err := os.Readlink(filepath.Join(dest, "installs/node/22.1.0/bin/readme"))
	if err != nil {
		t.Fatal(err)
	}
	if got != "../README" {
		t.Fatalf("relative link = %q, want ../README", got)
	}

	// The absolute link now points into the new destination and resolves.
	linked := filepath.Join(dest, "installs/node/22.1.0/bin/jq")
	got, err = os.Readlink(linked)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dest, "installs/jq/1.8.1/bin/jq")
	if got != want {
		t.Fatalf("relocated link = %q, want %q", got, want)
	}
	if _, err := os.Stat(linked); err != nil {
		t.Fatalf("relocated link does not resolve: %v", err)
	}

	// The bundled mise binary, generated config and sidecars are in place.
	for _, rel := range []string{"bin/mise", "config/config.toml", "activate.sh", "mpack-manifest.json"} {
		if _, err := os.Stat(filepath.Join(dest, rel)); err != nil {
			t.Errorf("missing %s: %v", rel, err)
		}
	}
	if fi, err := os.Stat(filepath.Join(dest, "bin/mise")); err == nil && fi.Mode().Perm()&0o111 == 0 {
		t.Error("bundled mise is not executable")
	}
	script, err := os.ReadFile(filepath.Join(dest, "activate.sh"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"MISE_DATA_DIR", "MISE_CONFIG_DIR", "MISE_OFFLINE=1", "env -s bash"} {
		if !strings.Contains(string(script), want) {
			t.Errorf("activate.sh missing %q", want)
		}
	}
	if strings.Contains(string(script), `"$MPACK_ROOT/bin/mise" activate `) {
		t.Fatal("activate.sh must not install a dynamic mise hook")
	}
}

func TestUnpackRejectsPlatformMismatch(t *testing.T) {
	root := fakeSource(t, "node/22.1.0")
	plan, err := fakePlan(t, root, false, "node/22.1.0")
	if err != nil {
		t.Fatal(err)
	}
	plan.Manifest.Arch = "some-other-arch"
	archive := packToBytes(t, plan)
	dest := filepath.Join(t.TempDir(), "out")

	if _, err := Unpack(bytes.NewReader(archive), UnpackOptions{Dest: dest}); err == nil {
		t.Fatal("platform mismatch should abort the unpack")
	} else if !strings.Contains(err.Error(), "no-arch-check") {
		t.Fatalf("error should mention the override: %v", err)
	}
	if _, err := Unpack(bytes.NewReader(archive), UnpackOptions{Dest: dest, SkipArch: true}); err != nil {
		t.Fatalf("--no-arch-check should allow it: %v", err)
	}
}

func TestUnpackOverwritePolicies(t *testing.T) {
	root := fakeSource(t, "node/22.1.0")
	plan, err := fakePlan(t, root, false, "node/22.1.0")
	if err != nil {
		t.Fatal(err)
	}
	archive := packToBytes(t, plan)
	dest := filepath.Join(t.TempDir(), "out")
	existing := filepath.Join(dest, "installs/node/22.1.0")
	if err := os.MkdirAll(existing, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(existing, "PRE-EXISTING")
	if err := os.WriteFile(marker, []byte("keep me"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Default: refuse, and change nothing.
	_, err = Unpack(bytes.NewReader(archive), UnpackOptions{Dest: dest})
	if err == nil || !strings.Contains(err.Error(), "already exist") {
		t.Fatalf("default policy should refuse, got %v", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("the existing tree was touched by a refused unpack")
	}
	if _, err := os.Stat(filepath.Join(dest, "bin/mise")); err == nil {
		t.Fatal("a refused unpack must not install anything")
	}

	// Skip: the existing tool is left alone, the rest is installed.
	if _, err := Unpack(bytes.NewReader(archive), UnpackOptions{Dest: dest, Policy: OverwriteSkip}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("--skip-existing removed an existing file")
	}
	if _, err := os.Stat(filepath.Join(dest, "bin/mise")); err != nil {
		t.Fatal("--skip-existing should still install missing units")
	}

	// Force: the tool is replaced.
	if _, err := Unpack(bytes.NewReader(archive), UnpackOptions{Dest: dest, Policy: OverwriteForce}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("--force should have replaced the tool directory")
	}
	if _, err := os.Stat(filepath.Join(dest, "installs/node/22.1.0/README")); err != nil {
		t.Fatalf("replacement content missing: %v", err)
	}
	// No staging or backup directories are left behind.
	entries, err := os.ReadDir(dest)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".mpack-") {
			t.Errorf("leftover temporary directory %s", e.Name())
		}
	}
}

// A --force unpack that fails partway must put the original tree back.
func TestUnpackForceRollsBackOnFailure(t *testing.T) {
	root := fakeSource(t, "node/22.1.0")
	plan, err := fakePlan(t, root, false, "node/22.1.0")
	if err != nil {
		t.Fatal(err)
	}
	// Claim a second tool in the manifest that the payload does not contain,
	// so the move phase fails after bin/mise and the config are in place.
	plan.Manifest.Tools = append(plan.Manifest.Tools, Tool{Name: "ghost", Version: "1.0.0", RelPath: "ghost/1.0.0"})
	archive := packToBytes(t, plan)

	dest := filepath.Join(t.TempDir(), "out")
	if err := os.MkdirAll(filepath.Join(dest, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	original := filepath.Join(dest, "bin/mise")
	if err := os.WriteFile(original, []byte("original mise"), 0o755); err != nil {
		t.Fatal(err)
	}

	if _, err := Unpack(bytes.NewReader(archive), UnpackOptions{Dest: dest, Policy: OverwriteForce}); err == nil {
		t.Fatal("unpack should have failed on the missing tool")
	}
	data, err := os.ReadFile(original)
	if err != nil {
		t.Fatalf("the replaced file was not restored: %v", err)
	}
	if string(data) != "original mise" {
		t.Fatalf("restored content = %q, want the original", data)
	}
	if _, err := os.Stat(filepath.Join(dest, "config/config.toml")); !os.IsNotExist(err) {
		t.Error("a rolled back unpack left a newly installed file behind")
	}
	entries, _ := os.ReadDir(dest)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".mpack-") {
			t.Errorf("leftover temporary directory %s", e.Name())
		}
	}
}

// buildHostileArchive writes a minimal archive with a caller supplied payload
// entry, to test the extraction defences against a tampered archive.
func buildHostileArchive(t *testing.T, write func(tw *tar.Writer)) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	m := Manifest{
		Schema: SchemaVersion, OS: runtime.GOOS, Arch: runtime.GOARCH,
		Tools: []Tool{{Name: "node", Version: "1", RelPath: "node/1"}},
	}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeBytes(tw, ManifestName, 0o644, data, time.Unix(0, 0).UTC()); err != nil {
		t.Fatal(err)
	}
	write(tw)
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestUnpackRejectsTamperedArchives(t *testing.T) {
	file := func(name string, body string) func(*tar.Writer) {
		return func(tw *tar.Writer) {
			_ = writeBytes(tw, name, 0o644, []byte(body), time.Unix(0, 0).UTC())
		}
	}
	link := func(name, target string) func(*tar.Writer) {
		return func(tw *tar.Writer) {
			_ = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeSymlink, Name: name, Linkname: target, Mode: 0o777})
		}
	}
	cases := []struct {
		name  string
		write func(*tar.Writer)
		want  string
	}{
		{"parent traversal", file("../escaped", "x"), "component"},
		{"absolute path", file("/tmp/escaped", "x"), "absolute"},
		{"dot dot inside", file("installs/node/../../escaped", "x"), "canonical"},
		{"absolute symlink", link("installs/node/1/evil", "/etc/passwd"), "not rewritten"},
		{"escaping symlink", link("installs/node/1/evil", "../../../../etc/passwd"), "escapes"},
		{"sentinel traversal", link("installs/node/1/evil", RootSentinel+"/../../etc"), "component"},
		{"hard link", func(tw *tar.Writer) {
			_ = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeLink, Name: "installs/node/1/hard", Linkname: "/etc/passwd"})
		}, "unsupported type"},
		{"device node", func(tw *tar.Writer) {
			_ = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeChar, Name: "installs/node/1/dev", Mode: 0o644})
		}, "unsupported type"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			archive := buildHostileArchive(t, tc.write)
			dest := filepath.Join(t.TempDir(), "out")
			_, err := Unpack(bytes.NewReader(archive), UnpackOptions{Dest: dest})
			if err == nil {
				t.Fatal("hostile archive was accepted")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
			// Nothing may be installed when extraction fails.
			if _, err := os.Stat(filepath.Join(dest, "bin")); err == nil {
				t.Fatal("a rejected archive left files behind")
			}
		})
	}
}

// A symlink entry followed by a file "through" it must not write outside.
func TestUnpackRefusesToWriteThroughSymlink(t *testing.T) {
	outside := t.TempDir()
	archive := buildHostileArchive(t, func(tw *tar.Writer) {
		_ = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeSymlink, Name: "installs/node/1/link", Linkname: RootSentinel + "/node/1", Mode: 0o777})
		_ = writeBytes(tw, "installs/node/1/link/pwned", 0o644, []byte("x"), time.Unix(0, 0).UTC())
	})
	dest := filepath.Join(t.TempDir(), "out")
	if _, err := Unpack(bytes.NewReader(archive), UnpackOptions{Dest: dest}); err == nil {
		t.Fatal("writing through a symlink was allowed")
	}
	if _, err := os.Stat(filepath.Join(outside, "pwned")); err == nil {
		t.Fatal("a file escaped the destination")
	}
}

func TestReadManifestRejectsNonMpackArchive(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	_ = writeBytes(tw, "something-else", 0o644, []byte("x"), time.Unix(0, 0).UTC())
	_ = tw.Close()
	_ = gz.Close()
	if _, err := ReadManifest(bytes.NewReader(buf.Bytes())); err == nil {
		t.Fatal("a foreign tarball should be rejected")
	}
	if _, err := ReadManifest(strings.NewReader("not gzip at all")); err == nil {
		t.Fatal("a non-gzip file should be rejected")
	}
}

func TestDefaultDestReproducesProjectPathUnderHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if runtime.GOOS == "windows" {
		t.Skip("HOME override does not apply on Windows")
	}
	got, err := DefaultDest(&Manifest{HomeRelativeProjectPath: "work/myapp"})
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, "work/myapp", MpackSubdir); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}

	// Packed from outside the home directory: no guessing, ask for --dest.
	if _, err := DefaultDest(&Manifest{}); err == nil || !strings.Contains(err.Error(), "--dest") {
		t.Fatalf("expected a --dest hint, got %v", err)
	}
	// A tampered manifest must not steer the destination out of the home dir.
	for _, bad := range []string{"../../etc", "/etc", "work/../../etc"} {
		if _, err := DefaultDest(&Manifest{HomeRelativeProjectPath: bad}); err == nil {
			t.Errorf("DefaultDest accepted hostile project path %q", bad)
		}
	}
}

// Unpacking into an existing project directory must not disturb its files.
func TestUnpackLeavesProjectFilesAlone(t *testing.T) {
	home := t.TempDir()
	project := filepath.Join(home, "work", "myapp")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(project, "main.go")
	if err := os.WriteFile(src, []byte("package main"), 0o644); err != nil {
		t.Fatal(err)
	}
	root := fakeSource(t, "node/22.1.0")
	plan, err := fakePlan(t, root, false, "node/22.1.0")
	if err != nil {
		t.Fatal(err)
	}
	archive := packToBytes(t, plan)

	dest := filepath.Join(project, MpackSubdir)
	if _, err := Unpack(bytes.NewReader(archive), UnpackOptions{Dest: dest}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(src)
	if err != nil || string(data) != "package main" {
		t.Fatalf("project source was modified: %q, %v", data, err)
	}
	entries, err := os.ReadDir(project)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if len(names) != 2 {
		t.Fatalf("unpack touched the project directory: %v", names)
	}
}

func TestDryRunWritesNothing(t *testing.T) {
	root := fakeSource(t, "node/22.1.0")
	plan, err := fakePlan(t, root, false, "node/22.1.0")
	if err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "out")
	if _, err := Unpack(bytes.NewReader(packToBytes(t, plan)), UnpackOptions{Dest: dest, DryRun: true, Log: io.Discard}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatal("--dry-run created the destination")
	}
}

// The generated sidecars must obey the overwrite policy like everything else,
// rather than being written unconditionally.
func TestSidecarsRespectOverwritePolicy(t *testing.T) {
	root := fakeSource(t, "node/22.1.0")
	plan, err := fakePlan(t, root, false, "node/22.1.0")
	if err != nil {
		t.Fatal(err)
	}
	archive := packToBytes(t, plan)

	dest := filepath.Join(t.TempDir(), "out")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	existing := filepath.Join(dest, ActivateName)
	if err := os.WriteFile(existing, []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err = Unpack(bytes.NewReader(archive), UnpackOptions{Dest: dest})
	if err == nil || !strings.Contains(err.Error(), ActivateName) {
		t.Fatalf("an existing %s should block the unpack, got %v", ActivateName, err)
	}
	if data, _ := os.ReadFile(existing); string(data) != "mine" {
		t.Fatalf("the existing sidecar was overwritten: %q", data)
	}

	if _, err := Unpack(bytes.NewReader(archive), UnpackOptions{Dest: dest, Policy: OverwriteSkip}); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(existing); string(data) != "mine" {
		t.Fatalf("--skip-existing replaced the sidecar: %q", data)
	}
	if _, err := Unpack(bytes.NewReader(archive), UnpackOptions{Dest: dest, Policy: OverwriteForce}); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(existing); !strings.Contains(string(data), "MISE_DATA_DIR") {
		t.Fatalf("--force did not replace the sidecar: %q", data)
	}
}

// A sidecar that exists as a symlink must not be followed: writing through it
// would modify a file outside the destination.
func TestSidecarSymlinkIsNotFollowed(t *testing.T) {
	root := fakeSource(t, "node/22.1.0")
	plan, err := fakePlan(t, root, false, "node/22.1.0")
	if err != nil {
		t.Fatal(err)
	}
	archive := packToBytes(t, plan)

	outside := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(outside, []byte("precious"), 0o644); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "out")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dest, ActivateName)); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	// Default policy: refused outright.
	if _, err := Unpack(bytes.NewReader(archive), UnpackOptions{Dest: dest}); err == nil {
		t.Fatal("a symlinked sidecar should be reported as a conflict")
	}
	if data, _ := os.ReadFile(outside); string(data) != "precious" {
		t.Fatalf("a file outside the destination was modified: %q", data)
	}
	// --force replaces the link itself, never the file it points at.
	if _, err := Unpack(bytes.NewReader(archive), UnpackOptions{Dest: dest, Policy: OverwriteForce}); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(outside); string(data) != "precious" {
		t.Fatalf("--force wrote through the symlink: %q", data)
	}
	fi, err := os.Lstat(filepath.Join(dest, ActivateName))
	if err != nil || fi.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("the symlink should have been replaced by a real file: %v %v", fi, err)
	}
}

// Rolling back must also undo the sidecars, not just the archived units.
func TestSidecarsAreRolledBack(t *testing.T) {
	root := fakeSource(t, "node/22.1.0")
	plan, err := fakePlan(t, root, false, "node/22.1.0")
	if err != nil {
		t.Fatal(err)
	}
	plan.Manifest.Tools = append(plan.Manifest.Tools, Tool{Name: "ghost", Version: "1", RelPath: "ghost/1"})
	archive := packToBytes(t, plan)

	dest := filepath.Join(t.TempDir(), "out")
	if _, err := Unpack(bytes.NewReader(archive), UnpackOptions{Dest: dest}); err == nil {
		t.Fatal("expected the unpack to fail")
	}
	for _, name := range []string{ActivateName, InstalledManifestName} {
		if _, err := os.Stat(filepath.Join(dest, name)); !os.IsNotExist(err) {
			t.Errorf("%s survived a rolled back unpack", name)
		}
	}
}

// The default destination comes from the archive, so no existing symlink
// between the home directory and it may be followed.
func TestUnpackRefusesSymlinkedPathToDefaultDest(t *testing.T) {
	home := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(home, "work")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	root := fakeSource(t, "node/22.1.0")
	plan, err := fakePlan(t, root, false, "node/22.1.0")
	if err != nil {
		t.Fatal(err)
	}
	archive := packToBytes(t, plan)

	dest := filepath.Join(home, "work", "myapp", MpackSubdir)
	_, err = Unpack(bytes.NewReader(archive), UnpackOptions{Dest: dest, SafeBase: home})
	if err == nil {
		t.Fatal("unpack followed a symlink on the way to the default destination")
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("unhelpful error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "myapp")); err == nil {
		t.Fatal("files were created outside the home directory")
	}
}

func TestUnpackRefusesSymlinkedDest(t *testing.T) {
	outside := t.TempDir()
	base := t.TempDir()
	dest := filepath.Join(base, "link")
	if err := os.Symlink(outside, dest); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	root := fakeSource(t, "node/22.1.0")
	plan, err := fakePlan(t, root, false, "node/22.1.0")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Unpack(bytes.NewReader(packToBytes(t, plan)), UnpackOptions{Dest: dest}); err == nil {
		t.Fatal("unpacking into a symlinked destination should be refused")
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatal("files were written through the destination symlink")
	}
}
