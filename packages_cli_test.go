package main

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ofpack/internal/ofpack"
)

// readArchiveManifest returns an archive's manifest and entry names.
func readArchiveManifest(t *testing.T, path string) (ofpack.Manifest, map[string]bool) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	var m ofpack.Manifest
	names := map[string]bool{}
	first := true
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names[strings.TrimSuffix(hdr.Name, "/")] = true
		if first {
			if err := json.NewDecoder(tr).Decode(&m); err != nil {
				t.Fatal(err)
			}
			first = false
		}
	}
	return m, names
}

// --package-base says where a closure is resolved, not which architecture
// it is for. Without --target a Mac would record darwin and bundle a macOS
// unpacker into an archive full of Linux packages, so the combination is
// refused before anything is downloaded.
func TestPackageBaseRequiresATarget(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ofpack.PackagesListName), []byte("jq\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "out.tar.gz")
	err := cmdPack([]string{"--packages",
		"--package-base", "docker.io/library/debian:bookworm-slim",
		"-C", dir, "-o", out})
	if err == nil || !strings.Contains(err.Error(), "--target") {
		t.Fatalf("--package-base without --target should be refused, got %v", err)
	}
	if _, statErr := os.Stat(out); !os.IsNotExist(statErr) {
		t.Error("an archive was written despite the refusal")
	}
	// And it is not silently accepted without --packages either.
	if err := cmdPack([]string{"--package-base", "x", "--target", "linux/amd64", "-C", dir, "-o", out}); err == nil ||
		!strings.Contains(err.Error(), "--packages") {
		t.Fatalf("--package-base without --packages should be refused, got %v", err)
	}
}

// A package-only archive is described by the platform it is for, not the
// one that built it, and the bundled unpacker has to run there.
func TestPackageOnlyPlanUsesTheTarget(t *testing.T) {
	stage := t.TempDir()
	dir := filepath.Join(stage, ofpack.PackagesPrefix, ofpack.ManagerAPT)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "jq_1.6_amd64.deb"), []byte("deb"), 0o644); err != nil {
		t.Fatal(err)
	}
	files, err := ofpack.CollectPackageFiles(dir, ".deb")
	if err != nil {
		t.Fatal(err)
	}
	plan, err := ofpack.BuildPackagesOnlyPlan(ofpack.PackagesOnlyOptions{
		WorkDir: t.TempDir(), StageDir: stage, Target: "linux/amd64", Version: "test",
		Packages: &ofpack.Packages{
			Manager: ofpack.ManagerAPT, OS: ofpack.OSRelease{ID: "debian", VersionID: "12"},
			Requested: []string{"jq"}, Closure: "test", Files: files,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Manifest.OS != "linux" || plan.Manifest.Arch != "amd64" {
		t.Fatalf("manifest records %s/%s; it must describe the target, not this machine",
			plan.Manifest.OS, plan.Manifest.Arch)
	}
}

// Tools in a child directory still count. Looking only at the root made a
// project with its tools one level down look toolless, and a package-only
// archive silently dropped them.
func TestPackagesDoNotHideChildScopeTools(t *testing.T) {
	if _, err := ofpack.LookupEnv(); err != nil {
		t.Skip("mise is not installed")
	}
	root := t.TempDir()
	child := filepath.Join(root, "app")
	if err := os.MkdirAll(child, 0o755); err != nil {
		t.Fatal(err)
	}
	// The root has packages but no tools; the child has a tool.
	if err := os.WriteFile(filepath.Join(root, ofpack.PackagesListName), []byte("jq\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(child, "mise.toml"), []byte("[tools]\njq = \"1.8.1\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// An empty global config, so only the fixture's own scopes apply.
	t.Setenv("MISE_CONFIG_DIR", t.TempDir())

	if !hasMiseProject(root) {
		t.Fatal("a tool in a child scope was not noticed; the pack would drop it as package-only")
	}
}
