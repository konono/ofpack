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

func scopedFixture(t *testing.T) []byte {
	t.Helper()
	root := fakeSource(t, "go/1.24.0", "go/1.25.0")
	plan, err := fakePlan(t, root, false, "go/1.24.0", "go/1.25.0")
	if err != nil {
		t.Fatal(err)
	}
	plan.Manifest.Schema = SchemaVersionScoped
	plan.Manifest.Scopes = []Scope{
		{Path: ".", Tools: []Tool{plan.Manifest.Tools[0]}},
		{Path: "app", Tools: []Tool{plan.Manifest.Tools[1]}},
	}
	return packToBytes(t, plan)
}

func TestScopedUnpackGeneratesTransactionalChildFiles(t *testing.T) {
	archive := scopedFixture(t)
	dest := filepath.Join(t.TempDir(), ".mpack")
	if _, err := Unpack(bytes.NewReader(archive), UnpackOptions{Dest: dest}); err != nil {
		t.Fatal(err)
	}
	configName := filepath.Join(dest, "config/scopes/app/config.toml")
	config, err := os.ReadFile(configName)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(config), "1.25.0") || strings.Contains(string(config), "1.24.0") {
		t.Fatalf("child config selects the wrong tools: %s", config)
	}
	script, err := os.ReadFile(filepath.Join(dest, "config/scopes/app/direnv.envrc"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(script), "MISE_GLOBAL_CONFIG_FILE") || !strings.Contains(string(script), "config/scopes/app") {
		t.Fatalf("scope script does not pin its config: %s", script)
	}
	// A generated scope file must have the same conflict policy as packed units.
	if _, err := Unpack(bytes.NewReader(archive), UnpackOptions{Dest: dest}); err == nil || !strings.Contains(err.Error(), "already exist") {
		t.Fatalf("expected a conflict, got %v", err)
	}
	if err := os.WriteFile(configName, []byte("user override"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Unpack(bytes.NewReader(archive), UnpackOptions{Dest: dest, Policy: OverwriteSkip}); err != nil {
		t.Fatal(err)
	}
	if content, _ := os.ReadFile(configName); string(content) != "user override" {
		t.Fatalf("skip replaced child config: %s", content)
	}
	if _, err := Unpack(bytes.NewReader(archive), UnpackOptions{Dest: dest, Policy: OverwriteForce}); err != nil {
		t.Fatal(err)
	}
	if content, _ := os.ReadFile(configName); !strings.Contains(string(content), "1.25.0") {
		t.Fatalf("force failed to replace child config: %s", content)
	}
}

func TestScopedManifestRejectsHostilePathsAndUnknownTools(t *testing.T) {
	tool := Tool{Name: "go", Version: "1.25.0", RelPath: "go/1.25.0"}
	for name, scopes := range map[string][]Scope{
		"escape":       {{Path: ".", Tools: []Tool{tool}}, {Path: "../outside"}},
		"duplicate":    {{Path: "."}, {Path: "."}},
		"unknown":      {{Path: ".", Tools: []Tool{{Name: "go", Version: "1.26.0", RelPath: "go/1.26.0"}}}},
		"missing root": {{Path: "app", Tools: []Tool{tool}}},
	} {
		t.Run(name, func(t *testing.T) {
			m := &Manifest{Schema: SchemaVersionScoped, Tools: []Tool{tool}, Scopes: scopes}
			if err := validateScopedManifest(m); err == nil {
				t.Fatalf("accepted hostile scoped manifest: %+v", scopes)
			}
		})
	}
	m := &Manifest{Schema: SchemaVersionScoped, Tools: []Tool{tool, {Name: "other", Version: "2", RelPath: tool.RelPath}}, Scopes: []Scope{{Path: "."}}}
	if err := validateScopedManifest(m); err == nil {
		t.Fatal("accepted two tools claiming one install directory")
	}
}

func TestScopedUnpackReplacesLinkNotItsTarget(t *testing.T) {
	archive := scopedFixture(t)
	base := t.TempDir()
	dest := filepath.Join(base, ".mpack")
	configName := filepath.Join(dest, "config/scopes/app/config.toml")
	if err := os.MkdirAll(filepath.Dir(configName), 0o755); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(base, "victim")
	if err := os.WriteFile(victim, []byte("precious"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, configName); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := Unpack(bytes.NewReader(archive), UnpackOptions{Dest: dest, Policy: OverwriteForce}); err != nil {
		t.Fatal(err)
	}
	if content, _ := os.ReadFile(victim); string(content) != "precious" {
		t.Fatalf("external link target changed: %s", content)
	}
	if fi, err := os.Lstat(configName); err != nil || fi.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("scope config was not installed as a regular file: %v, %v", fi, err)
	}
}

func TestPlanDirenvSelectsDeepestScopedConfig(t *testing.T) {
	projectRoot := t.TempDir()
	dest := filepath.Join(projectRoot, MpackSubdir)
	go24 := Tool{Name: "go", Version: "1.24.0", RelPath: "go/1.24.0"}
	go25 := Tool{Name: "go", Version: "1.25.0", RelPath: "go/1.25.0"}
	if err := os.MkdirAll(filepath.Join(dest, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{MiseBinName, DirenvName, scopeDirenvName("app")} {
		full := filepath.Join(dest, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("ok"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	m := Manifest{Schema: SchemaVersionScoped, Tools: []Tool{go24, go25}, Scopes: []Scope{{Path: ".", Tools: []Tool{go24}}, {Path: "app", Tools: []Tool{go25}}}}
	data, _ := json.Marshal(m)
	if err := os.WriteFile(filepath.Join(dest, InstalledManifestName), data, 0o644); err != nil {
		t.Fatal(err)
	}
	child := filepath.Join(projectRoot, "app", "nested")
	if err := os.MkdirAll(child, 0o755); err != nil {
		t.Fatal(err)
	}
	plan, err := PlanDirenv(child, "")
	if err != nil {
		t.Fatal(err)
	}
	if plan.ScopePath != "app" || !strings.Contains(plan.Line, "config/scopes/app/direnv.envrc") || !strings.Contains(plan.Line, "../") {
		t.Fatalf("wrong child scope plan: %+v", plan)
	}
	other := filepath.Join(projectRoot, "other")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	rootPlan, err := PlanDirenv(other, "")
	if err != nil || rootPlan.ScopePath != "." {
		t.Fatalf("root scope plan: %+v, %v", rootPlan, err)
	}
}

func TestCheckSnapshotScopePinsChildConfigAndTools(t *testing.T) {
	archive := scopedFixture(t)
	dest := filepath.Join(t.TempDir(), ".mpack")
	if _, err := Unpack(bytes.NewReader(archive), UnpackOptions{Dest: dest}); err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(t.TempDir(), "config-used")
	stub := `#!/bin/sh
printf '%s\n' "$MISE_GLOBAL_CONFIG_FILE" >> ` + shellQuote(record) + `
case "$*" in
  *env*) printf 'export PATH=%s\n' "'/stub/bin:$PATH'" ;;
  *ls*)
    case "$MISE_GLOBAL_CONFIG_FILE" in
      */scopes/app/config.toml) printf '%s' '{"go":[{"version":"1.25.0","active":true,"installed":true}]}' ;;
      *) printf '%s' '{"go":[{"version":"1.24.0","active":true,"installed":true}]}' ;;
    esac ;;
esac
`
	if err := os.WriteFile(filepath.Join(dest, filepath.FromSlash(MiseBinName)), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	check, err := CheckSnapshotScope(dest, "app")
	if err != nil || !check.OK() {
		t.Fatalf("child check failed: %+v, %v", check, err)
	}
	used, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(used), filepath.Join(dest, "config/scopes/app/config.toml")) {
		t.Fatalf("child config was not pinned: %s", used)
	}
}

func TestScopedDirenvScriptFindsMovedTree(t *testing.T) {
	archive := scopedFixture(t)
	dest := filepath.Join(t.TempDir(), "original", ".mpack")
	if _, err := Unpack(bytes.NewReader(archive), UnpackOptions{Dest: dest}); err != nil {
		t.Fatal(err)
	}
	newBase := filepath.Join(filepath.Dir(filepath.Dir(dest)), "renamed")
	if err := os.Rename(filepath.Dir(dest), newBase); err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(newBase, ".mpack")
	stub := "#!/bin/sh\nprintf 'export PATH=%s\\n' \"'/stub/bin:$PATH'\"\n"
	if err := os.WriteFile(filepath.Join(moved, filepath.FromSlash(MiseBinName)), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	scopeDir := filepath.Join(moved, "config/scopes/app")
	cmd := exec.Command("bash", "-c", `PATH_add() { :; }; watch_file() { :; }; . ./direnv.envrc; printf '%s\n%s\n%s\n' "$MISE_DATA_DIR" "$MISE_CONFIG_DIR" "$MISE_GLOBAL_CONFIG_FILE"`)
	cmd.Dir = scopeDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("moved scope script failed: %v: %s", err, out)
	}
	for _, want := range []string{moved, filepath.Join(scopeDir, "config.toml")} {
		if !strings.Contains(string(out), want) {
			t.Fatalf("scope script did not derive moved path %s: %s", want, out)
		}
	}
}

// Manual activation is a fixed root snapshot even when sourced inside a child
// whose live mise.toml asks for another version. A dynamic mise hook would
// immediately replace the -C / environment and fail this production path.
func TestActivateScriptKeepsRootSelectionInsideChild(t *testing.T) {
	project := t.TempDir()
	child := filepath.Join(project, "app")
	if err := os.MkdirAll(child, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(child, "mise.toml"), []byte("[tools]\ngo = \"1.25.0\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(project, MpackSubdir)
	if _, err := Unpack(bytes.NewReader(scopedFixture(t)), UnpackOptions{Dest: dest}); err != nil {
		t.Fatal(err)
	}
	stub := `#!/bin/sh
case "$*" in
  "-C / env -s bash") printf 'export PATH="%s/installs/go/1.24.0/bin:$PATH"\n' "$MISE_DATA_DIR" ;;
  "activate bash")
    if [ -f ./mise.toml ]; then
      printf 'export PATH="%s/installs/go/1.25.0/bin:$PATH"\n' "$MISE_DATA_DIR"
    fi ;;
  *) exit 1 ;;
esac
`
	if err := os.WriteFile(filepath.Join(dest, MiseBinName), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", "-c", `PROMPT_COMMAND=; . ../.mpack/activate.sh; command -v go; cd ..; command -v go; printf 'prompt=%s\n' "$PROMPT_COMMAND"`)
	cmd.Dir = child
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("activate.sh failed: %v: %s", err, out)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	want := filepath.Join(dest, "installs/go/1.24.0/bin/go")
	if len(lines) != 3 || lines[0] != want || lines[1] != want || lines[2] != "prompt=" {
		t.Fatalf("manual activation did not keep the root snapshot: %q, want %q twice and no hook", out, want)
	}
}
