package mpack

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestEndToEndWithRealMise packs a real (small) mise tool and then checks that
// the bundled mise, running under an isolated HOME and MISE_DATA_DIR, reads
// the generated config and finds the tool in its new location. It is skipped
// when mise is not installed, or when MPACK_E2E_TOOL is not set to an
// installed tool.
//
//	MPACK_E2E_TOOL='jq@1.8.1' go test ./... -run EndToEnd
func TestEndToEndWithRealMise(t *testing.T) {
	spec := os.Getenv("MPACK_E2E_TOOL")
	if spec == "" {
		t.Skip("set MPACK_E2E_TOOL=<tool>@<version> (an already installed mise tool) to run the end to end test")
	}
	name, version, ok := strings.Cut(spec, "@")
	if !ok {
		t.Fatalf("MPACK_E2E_TOOL=%q, want <tool>@<version>", spec)
	}
	if _, err := exec.LookPath("mise"); err != nil {
		t.Skip("mise is not installed")
	}

	// An empty config dir keeps the real global config out of the way, so the
	// pack contains only the one tool the project asks for.
	t.Setenv("MISE_CONFIG_DIR", t.TempDir())
	project := t.TempDir()
	cfg := "[tools]\n" + tomlQuote(name) + " = " + tomlQuote(version) + "\n"
	if err := os.WriteFile(filepath.Join(project, "mise.toml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	plan, err := BuildPlan(PackOptions{WorkDir: project, Version: "e2e"})
	if err != nil {
		t.Fatalf("pack: %v", err)
	}
	if len(plan.Manifest.Tools) != 1 || plan.Manifest.Tools[0].Name != name {
		t.Fatalf("expected to pack only %s, got %+v", name, plan.Manifest.Tools)
	}
	var archive bytes.Buffer
	if err := plan.Write(&archive); err != nil {
		t.Fatal(err)
	}

	// Unpack somewhere entirely unrelated to the source installs root.
	home := t.TempDir()
	dest := filepath.Join(home, "work", "myapp", MpackSubdir)
	if _, err := Unpack(bytes.NewReader(archive.Bytes()), UnpackOptions{Dest: dest}); err != nil {
		t.Fatalf("unpack: %v", err)
	}

	// Run the bundled mise exactly the way activate.sh sets it up, with a
	// scrubbed environment so nothing from the developer's shell leaks in.
	miseBin := filepath.Join(dest, "bin", "mise")
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command(miseBin, args...)
		cmd.Dir = home // not the project dir: the generated global config must suffice
		cmd.Env = []string{
			"HOME=" + home,
			"PATH=/usr/bin:/bin",
			"MISE_DATA_DIR=" + dest,
			"MISE_CONFIG_DIR=" + filepath.Join(dest, "config"),
			"MISE_OFFLINE=1",
		}
		var out, errb bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errb
		if err := cmd.Run(); err != nil {
			t.Fatalf("mise %s: %v\n%s", strings.Join(args, " "), err, errb.String())
		}
		return out.String()
	}

	var listed map[string][]miseToolEntry
	if err := json.Unmarshal([]byte(run("ls", "--current", "--json")), &listed); err != nil {
		t.Fatal(err)
	}
	entries, found := listed[name]
	if !found || len(entries) == 0 {
		t.Fatalf("the unpacked mise does not see %s: %v", name, listed)
	}
	got := entries[0]
	if !got.Installed || !got.Active {
		t.Fatalf("%s is not usable: installed=%v active=%v", name, got.Installed, got.Active)
	}
	if got.Version != version {
		t.Fatalf("version = %q, want %q", got.Version, version)
	}
	if !isUnder(dest, filepath.Clean(got.InstallPath)) {
		t.Fatalf("install path %q is not inside the unpacked tree %q", got.InstallPath, dest)
	}
	if _, err := os.Stat(got.InstallPath); err != nil {
		t.Fatalf("the reported install path does not exist: %v", err)
	}

	// And the tool actually resolves to a runnable binary in the new tree.
	which := strings.TrimSpace(run("which", name))
	if !isUnder(dest, filepath.Clean(which)) {
		t.Fatalf("mise which %s = %q, want a path inside %q", name, which, dest)
	}
	fi, err := os.Stat(which)
	if err != nil || fi.Mode().Perm()&0o111 == 0 {
		t.Fatalf("%s is not executable: %v", which, err)
	}
}

// TestEndToEndPreservesVersionPrecedence packs a tool pinned to several
// versions and checks that the unpacked environment resolves the *same*
// version as the source. mise lists versions in sorted order while the config
// order decides precedence, so a naive pack flips the default.
//
//	MPACK_E2E_MULTI='node@24.16.0,22.22.2' go test ./... -run EndToEnd
func TestEndToEndPreservesVersionPrecedence(t *testing.T) {
	spec := os.Getenv("MPACK_E2E_MULTI")
	if spec == "" {
		t.Skip("set MPACK_E2E_MULTI=<tool>@<v1>,<v2> (both already installed) to run the precedence test")
	}
	name, versions, ok := strings.Cut(spec, "@")
	if !ok || !strings.Contains(versions, ",") {
		t.Fatalf("MPACK_E2E_MULTI=%q, want <tool>@<v1>,<v2>", spec)
	}
	if _, err := exec.LookPath("mise"); err != nil {
		t.Skip("mise is not installed")
	}

	quoted := make([]string, 0, 2)
	for _, v := range strings.Split(versions, ",") {
		quoted = append(quoted, tomlQuote(strings.TrimSpace(v)))
	}

	t.Setenv("MISE_CONFIG_DIR", t.TempDir())
	project := t.TempDir()
	cfg := "[tools]\n" + tomlQuote(name) + " = [" + strings.Join(quoted, ", ") + "]\n"
	if err := os.WriteFile(filepath.Join(project, "mise.toml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	// What the source machine resolves, before packing.
	srcCmd := exec.Command("mise", "which", name)
	srcCmd.Dir = project
	srcOut, err := srcCmd.Output()
	if err != nil {
		t.Fatalf("mise which %s on the source: %v", name, err)
	}
	srcVersion := versionFromToolPath(strings.TrimSpace(string(srcOut)))

	plan, err := BuildPlan(PackOptions{WorkDir: project, Version: "e2e"})
	if err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	if err := plan.Write(&archive); err != nil {
		t.Fatal(err)
	}

	home := t.TempDir()
	dest := filepath.Join(home, "work", "myapp", MpackSubdir)
	if _, err := Unpack(bytes.NewReader(archive.Bytes()), UnpackOptions{Dest: dest}); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(filepath.Join(dest, "bin", "mise"), "which", name)
	cmd.Dir = home
	cmd.Env = []string{
		"HOME=" + home,
		"PATH=/usr/bin:/bin",
		"MISE_DATA_DIR=" + dest,
		"MISE_CONFIG_DIR=" + filepath.Join(dest, "config"),
		"MISE_OFFLINE=1",
	}
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("mise which %s on the target: %v", name, err)
	}
	gotVersion := versionFromToolPath(strings.TrimSpace(string(out)))

	if gotVersion != srcVersion {
		t.Fatalf("the target resolves %s@%s but the source resolves %s@%s; the generated config is\n%s",
			name, gotVersion, name, srcVersion, RenderConfig(plan.Manifest.Tools))
	}
}

// versionFromToolPath pulls the version out of .../installs/<tool>/<version>/...
func versionFromToolPath(p string) string {
	parts := strings.Split(filepath.ToSlash(p), "/")
	for i, part := range parts {
		if part == InstallsPrefix && i+2 < len(parts) {
			return parts[i+2]
		}
	}
	return p
}
