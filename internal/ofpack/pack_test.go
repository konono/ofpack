package ofpack

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeSource builds an installs root that looks like mise's, with one tool per
// name, and returns the root.
func fakeSource(t *testing.T, tools ...string) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "installs")
	for _, rel := range tools {
		dir := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil {
			t.Fatal(err)
		}
		name := strings.Split(rel, "/")[0]
		if err := os.WriteFile(filepath.Join(dir, "bin", name), []byte("#!/bin/sh\necho "+name+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "README"), []byte(rel), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// fakePlan assembles a Plan without talking to mise, so the packing logic can
// be exercised on a synthetic tree.
func fakePlan(t *testing.T, root string, allowUnresolved bool, rels ...string) (*Plan, error) {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "mise")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	tools := make([]Tool, 0, len(rels))
	for _, rel := range rels {
		parts := strings.SplitN(rel, "/", 2)
		tools = append(tools, Tool{Name: parts[0], Version: parts[1], RequestedVersion: parts[1], RelPath: rel})
	}
	p := &Plan{
		Env:          Env{Bin: bin, Version: "test"},
		InstallsRoot: root,
		Units:        append([]string(nil), rels...),
		Manifest: Manifest{
			Schema: SchemaVersion, OfpackVersion: "test",
			OS: runtime.GOOS, Arch: runtime.GOARCH, MiseVersion: "test",
			Tools: tools, HomeRelativeProjectPath: "work/myapp",
		},
	}
	return p, p.scan(PackOptions{AllowUnresolved: allowUnresolved})
}

func TestClassifyLinkRelativeTargetIsKeptVerbatim(t *testing.T) {
	root := fakeSource(t, "node/22.1.0")
	link := filepath.Join(root, "node/22.1.0/bin/current")
	if err := os.Symlink("node", link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	plan, err := fakePlan(t, root, false, "node/22.1.0")
	if err != nil {
		t.Fatal(err)
	}
	if n := len(plan.Manifest.RewrittenLinks); n != 0 {
		t.Fatalf("a relative in-tree link should not be rewritten, got %d rewrites", n)
	}
	if plan.Manifest.Stats.Symlinks != 1 {
		t.Fatalf("symlink not archived: %+v", plan.Manifest.Stats)
	}
}

func TestClassifyLinkAbsoluteInTreeTargetIsRewritten(t *testing.T) {
	root := fakeSource(t, "prettier/3.9.3")
	target := filepath.Join(root, "prettier/3.9.3/README")
	link := filepath.Join(root, "prettier/3.9.3/bin/prettier")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	plan, err := fakePlan(t, root, false, "prettier/3.9.3")
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Manifest.RewrittenLinks) != 1 {
		t.Fatalf("absolute in-tree link should be recorded as rewritten: %+v", plan.Manifest.RewrittenLinks)
	}
	var stored string
	for _, e := range plan.entries {
		if strings.HasSuffix(e.name, "bin/prettier") {
			stored = e.linkTarget
		}
	}
	if want := RootSentinel + "/prettier/3.9.3/README"; stored != want {
		t.Fatalf("stored target = %q, want %q", stored, want)
	}
}

func TestClassifyLinkOutsideInstallsRootIsFatal(t *testing.T) {
	root := fakeSource(t, "rust/1.0.0")
	if err := os.Symlink("/opt/elsewhere/bin", filepath.Join(root, "rust/1.0.0/bin/cargo")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	_, err := fakePlan(t, root, false, "rust/1.0.0")
	if err == nil {
		t.Fatal("packing should fail rather than produce a silently broken archive")
	}
	if !strings.Contains(err.Error(), "outside the mise installs root") {
		t.Fatalf("unhelpful error: %v", err)
	}

	// With the explicit opt-in it packs, and the breakage is on the record.
	plan, err := fakePlan(t, root, true, "rust/1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Manifest.UnresolvedLinks) != 1 {
		t.Fatalf("unresolved link not recorded: %+v", plan.Manifest.UnresolvedLinks)
	}
}

// A link can stay inside the installs root and still break after relocation,
// if it points at a tool this archive does not carry.
func TestClassifyLinkToUnpackedToolIsFatal(t *testing.T) {
	root := fakeSource(t, "node/22.1.0", "python/3.12.0")
	if err := os.Symlink(filepath.Join(root, "python/3.12.0/bin/python"),
		filepath.Join(root, "node/22.1.0/bin/python")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	// Pack node only: the link now leaves the packed set.
	_, err := fakePlan(t, root, false, "node/22.1.0")
	if err == nil {
		t.Fatal("a link to a tool outside the archive must be detected")
	}
	if !strings.Contains(err.Error(), "not part of this archive") {
		t.Fatalf("unhelpful error: %v", err)
	}
	// Packing both tools makes the same link legitimate.
	if _, err := fakePlan(t, root, false, "node/22.1.0", "python/3.12.0"); err != nil {
		t.Fatalf("link within the packed set should be fine: %v", err)
	}
}

func TestScanRecordsDanglingLink(t *testing.T) {
	root := fakeSource(t, "node/22.1.0")
	if err := os.Symlink("missing", filepath.Join(root, "node/22.1.0/bin/gone")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	plan, err := fakePlan(t, root, false, "node/22.1.0")
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Manifest.DanglingLinks) != 1 {
		t.Fatalf("dangling link not reported: %+v", plan.Manifest.DanglingLinks)
	}
}

func TestScanRejectsUnsupportedFileType(t *testing.T) {
	root := fakeSource(t, "node/22.1.0")
	fifo := filepath.Join(root, "node/22.1.0/pipe")
	if err := mkfifo(fifo); err != nil {
		t.Skipf("cannot create a fifo: %v", err)
	}
	if _, err := fakePlan(t, root, false, "node/22.1.0"); err == nil ||
		!strings.Contains(err.Error(), "unsupported file type") {
		t.Fatalf("fifo should be rejected, got %v", err)
	}
}

func TestRenderConfigPinsExactVersions(t *testing.T) {
	got := string(RenderConfig([]Tool{
		{Name: "node", Version: "22.1.0", RequestedVersion: "latest"},
		{Name: "cargo:pwkit", Version: "0.1.0"},
	}))
	for _, want := range []string{"[tools]", `"node" = "22.1.0"`, `"cargo:pwkit" = "0.1.0"`} {
		if !strings.Contains(got, want) {
			t.Errorf("config missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "latest") {
		t.Errorf("the generated config must not contain a floating version:\n%s", got)
	}
}

func TestSplitInstallsRoot(t *testing.T) {
	root, rel, err := splitInstallsRoot("/home/u/.local/share/mise/installs/node/22.1.0")
	if err != nil {
		t.Fatal(err)
	}
	if root != filepath.FromSlash("/home/u/.local/share/mise/installs") || rel != "node/22.1.0" {
		t.Fatalf("got %q, %q", root, rel)
	}
	if _, _, err := splitInstallsRoot("/opt/tools/node/22.1.0"); err == nil {
		t.Fatal("a path with no installs component should fail")
	}
}

// A tool can be active at several versions at once. Emitting one key per
// version would be a duplicate TOML key, which mise refuses to parse.
func TestRenderConfigGroupsMultipleVersionsIntoAnArray(t *testing.T) {
	got := string(RenderConfig([]Tool{
		{Name: "node", Version: "22.22.2"},
		{Name: "node", Version: "24.16.0"},
		{Name: "jq", Version: "1.8.1"},
	}))
	if want := `"node" = ["22.22.2", "24.16.0"]`; !strings.Contains(got, want) {
		t.Errorf("missing %s:\n%s", want, got)
	}
	if !strings.Contains(got, `"jq" = "1.8.1"`) {
		t.Errorf("a single version should stay scalar:\n%s", got)
	}
	if n := strings.Count(got, `"node" =`); n != 1 {
		t.Errorf(`"node" appears as a key %d times; duplicate keys break mise:\n%s`, n, got)
	}
}

// The manifest is shipped to other people and machines: it must not carry the
// packer's home directory or account name.
func TestManifestDoesNotLeakAbsoluteSourcePaths(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	if got := scrubHome(filepath.Join(home, "work", "myapp", "mise.toml")); got != "~/work/myapp/mise.toml" {
		t.Errorf("scrubHome = %q, want ~/work/myapp/mise.toml", got)
	}
	if got := scrubHome("/opt/elsewhere/bin"); got != "" {
		t.Errorf("a path outside the home directory should be omitted, got %q", got)
	}

	root := fakeSource(t, "rust/1.0.0")
	if err := os.Symlink(filepath.Join(home, ".cargo", "bin"), filepath.Join(root, "rust/1.0.0/bin/cargo")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	plan, err := fakePlan(t, root, true, "rust/1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	plan.Manifest.Tools[0].ConfigSource = scrubHome(filepath.Join(home, ".config", "mise", "config.toml"))

	encoded, err := json.Marshal(plan.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), home) {
		t.Fatalf("the manifest contains the absolute home path:\n%s", encoded)
	}
	if got := plan.Manifest.UnresolvedLinks[0].Target; got != "~/.cargo/bin" {
		t.Errorf("link target = %q, want it relative to ~", got)
	}
	if got := plan.Manifest.Tools[0].ConfigSource; got != "~/.config/mise/config.toml" {
		t.Errorf("config source = %q, want it relative to ~", got)
	}
}

// mise lists versions in sorted order, but the config order is what decides
// which version wins on PATH. The generated config has to keep that order or
// the target resolves a different version than the source.
func TestToolOrderFollowsPathPrecedenceNotVersionOrder(t *testing.T) {
	root := filepath.Join(string(filepath.Separator), "data", "installs")
	pathValue := strings.Join([]string{
		filepath.Join(root, "node", "24.16.0", "bin"),
		filepath.Join(root, "node", "22.22.2", "bin"),
		"/usr/bin",
	}, string(filepath.ListSeparator))

	ranks := ranksFromPath(pathValue, root)
	if ranks["node/24.16.0"] != 0 || ranks["node/22.22.2"] != 1 {
		t.Fatalf("ranks = %v; want the config order 24 before 22", ranks)
	}
	if _, ok := ranks["usr/bin"]; ok {
		t.Error("entries outside the installs root must be ignored")
	}

	// Sorting by version alone would emit 22 first and flip the default.
	tools := []Tool{
		{Name: "node", Version: "22.22.2", RelPath: "node/22.22.2"},
		{Name: "node", Version: "24.16.0", RelPath: "node/24.16.0"},
	}
	sortToolsByPrecedence(tools, ranks)
	if tools[0].Version != "24.16.0" {
		t.Fatalf("first version = %s, want 24.16.0 (highest precedence)", tools[0].Version)
	}
	if got := string(RenderConfig(tools)); !strings.Contains(got, `["24.16.0", "22.22.2"]`) {
		t.Fatalf("generated config lost the precedence order:\n%s", got)
	}
}

// With no PATH information at all, the order must still be deterministic.
func TestToolOrderFallsBackToVersionWithoutPathInfo(t *testing.T) {
	tools := []Tool{
		{Name: "node", Version: "24.16.0", RelPath: "node/24.16.0"},
		{Name: "node", Version: "22.22.2", RelPath: "node/22.22.2"},
		{Name: "jq", Version: "1.8.1", RelPath: "jq/1.8.1"},
	}
	sortToolsByPrecedence(tools, nil)
	got := []string{tools[0].Name + tools[0].Version, tools[1].Version, tools[2].Version}
	if got[0] != "jq1.8.1" || got[1] != "22.22.2" || got[2] != "24.16.0" {
		t.Fatalf("unexpected fallback order: %v", got)
	}
}

// fakeMise installs a stub mise on OFPACK_MISE_BIN that reports the given
// tools and whose `env --json` behaviour is controllable, so the production
// pack path can be driven without a real mise.
func fakeMise(t *testing.T, installsRoot string, envJSON string, envFails bool, tools ...string) {
	t.Helper()
	entries := map[string][]map[string]any{}
	for _, rel := range tools {
		name, version, _ := strings.Cut(rel, "/")
		entries[name] = append(entries[name], map[string]any{
			"version":           version,
			"requested_version": version,
			"install_path":      filepath.Join(installsRoot, filepath.FromSlash(rel)),
			"installed":         true,
			"active":            true,
			"source":            map[string]string{"path": "/nowhere/mise.toml"},
		})
	}
	listJSON, err := json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	envBody := "printf '%s' " + shellQuote(envJSON)
	if envFails {
		envBody = "echo 'mise: boom' >&2; exit 1"
	}
	script := fmt.Sprintf(`#!/bin/sh
case "$1 $2" in
  "--version ") echo "9.9.9 test" ;;
  "config ls") echo '[]' ;;
  "ls --current") printf '%%s' %s ;;
  "env --json") %s ;;
  *) exit 1 ;;
esac
`, shellQuote(string(listJSON)), envBody)

	bin := filepath.Join(t.TempDir(), "mise")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OFPACK_MISE_BIN", bin)
}

// If mise cannot tell us the precedence, a tool with several active versions
// must stop the pack rather than be written in the wrong order.
func TestPackFailsWhenMultiVersionPrecedenceIsUnknown(t *testing.T) {
	root := fakeSource(t, "node/22.22.2", "node/24.16.0")
	project := t.TempDir()

	// `mise env --json` fails outright.
	fakeMise(t, root, "", true, "node/22.22.2", "node/24.16.0")
	_, err := BuildPlan(PackOptions{WorkDir: project, Version: "test"})
	if err == nil {
		t.Fatal("pack should fail when the version order cannot be determined")
	}
	for _, want := range []string{"node", "22.22.2", "24.16.0", "mise env --json", "--exclude"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q:\n%v", want, err)
		}
	}

	// `mise env --json` works but PATH says nothing about node.
	fakeMise(t, root, `{"PATH":"/usr/bin:/bin"}`, false, "node/22.22.2", "node/24.16.0")
	_, err = BuildPlan(PackOptions{WorkDir: project, Version: "test"})
	if err == nil {
		t.Fatal("pack should fail when PATH does not rank the versions")
	}
	if !strings.Contains(err.Error(), "PATH mise exports") {
		t.Errorf("unhelpful error: %v", err)
	}
}

// A single active version needs no ordering, so a missing PATH entry is fine.
func TestPackSucceedsWithoutPrecedenceForSingleVersionTools(t *testing.T) {
	root := fakeSource(t, "jq/1.8.1")
	fakeMise(t, root, "", true, "jq/1.8.1")
	plan, err := BuildPlan(PackOptions{WorkDir: t.TempDir(), Version: "test"})
	if err != nil {
		t.Fatalf("a single-version tool must still pack: %v", err)
	}
	if len(plan.Manifest.Tools) != 1 {
		t.Fatalf("got %d tools", len(plan.Manifest.Tools))
	}
}

// With PATH ranking both versions the pack proceeds and keeps that order.
func TestPackKeepsPathOrderForMultiVersionTools(t *testing.T) {
	root := fakeSource(t, "node/22.22.2", "node/24.16.0")
	pathValue := strings.Join([]string{
		filepath.Join(root, "node", "24.16.0", "bin"),
		filepath.Join(root, "node", "22.22.2", "bin"),
	}, string(filepath.ListSeparator))
	envJSON, err := json.Marshal(map[string]string{"PATH": pathValue})
	if err != nil {
		t.Fatal(err)
	}
	fakeMise(t, root, string(envJSON), false, "node/22.22.2", "node/24.16.0")

	plan, err := BuildPlan(PackOptions{WorkDir: t.TempDir(), Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if got := plan.Manifest.Scopes[0].Tools[0].Version; got != "24.16.0" {
		t.Fatalf("first version = %s, want 24.16.0", got)
	}
	if got := string(RenderConfig(plan.Manifest.Scopes[0].Tools)); !strings.Contains(got, `["24.16.0", "22.22.2"]`) {
		t.Fatalf("generated config lost the PATH order:\n%s", got)
	}
}
