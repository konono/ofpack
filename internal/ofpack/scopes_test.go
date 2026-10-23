package ofpack

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// This uses the production planner with a controlled mise executable. The
// two scopes select different versions, but each install tree is stored once.
func TestBuildPlanRecordsDirectorySelectionsAndPhysicalUnion(t *testing.T) {
	installs := fakeSource(t, "jq/1.8.1", "jq/1.8.2")
	base := t.TempDir()
	root := filepath.Join(base, "project")
	child := filepath.Join(root, "app")
	for _, dir := range []string{root, child} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, dir := range []string{base, root, child} {
		if err := os.WriteFile(filepath.Join(dir, "mise.toml"), []byte("[tools]\njq = \"1.8.1\"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	entry := func(v string) string {
		b, _ := json.Marshal(map[string]any{"jq": []map[string]any{{"version": v, "requested_version": v, "install_path": filepath.Join(installs, "jq", v), "installed": true, "active": true}}})
		return string(b)
	}
	bin := filepath.Join(t.TempDir(), "mise")
	script := fmt.Sprintf(`#!/bin/sh
case "$1 $2" in
  "--version ") echo 'test' ;;
  "config ls") echo '[]' ;;
  "ls --current")
    case "$(pwd -P)" in
      %s) printf '%%s' %s ;;
      *) printf '%%s' %s ;;
    esac ;;
  "bin-paths ")
    case "$(pwd -P)" in
      %s) echo %s ;;
      *) echo %s ;;
    esac ;;
  "env --json") echo '{}' ;;
  *) exit 1 ;;
esac
`, shellQuote(canonical(child)), shellQuote(entry("1.8.2")), shellQuote(entry("1.8.1")), shellQuote(canonical(child)), shellQuote(filepath.Join(installs, "jq/1.8.2/bin")), shellQuote(filepath.Join(installs, "jq/1.8.1/bin")))
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OFPACK_MISE_BIN", bin)
	plan, err := BuildPlan(PackOptions{WorkDir: root, Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := BuildPlan(PackOptions{WorkDir: root, Version: "test", Exclude: []string{"jq"}}); err == nil || !strings.Contains(err.Error(), "nothing to pack") {
		t.Fatalf("--exclude jq must remove both scoped versions, got %v", err)
	}
	if plan.Manifest.Schema != SchemaVersionScoped || len(plan.Manifest.Scopes) != 2 || len(plan.Manifest.Tools) != 2 || len(plan.Units) != 2 {
		t.Fatalf("wrong scope/union: %+v, units=%v", plan.Manifest, plan.Units)
	}
	if plan.Manifest.Scopes[0].Tools[0].Version != "1.8.1" || plan.Manifest.Scopes[1].Tools[0].Version != "1.8.2" {
		t.Fatalf("scope selections: %+v", plan.Manifest.Scopes)
	}
	var archive bytes.Buffer
	if err := plan.Write(&archive); err != nil {
		t.Fatal(err)
	}
	gz, err := gzip.NewReader(&archive)
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			t.Fatal("missing root config")
		}
		if err != nil {
			t.Fatal(err)
		}
		if h.Name != ConfigName {
			continue
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(body), "1.8.1") || strings.Contains(string(body), "1.8.2") {
			t.Fatalf("root config uses union: %s", body)
		}
		break
	}
}

func TestDiscoverMiseScopesSkipsGeneratedTrees(t *testing.T) {
	root := t.TempDir()
	for _, rel := range []string{"app", ".venv/lib", ".ofpack/config", "node_modules/pkg"} {
		dir := filepath.Join(root, rel)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "mise.toml"), []byte("[tools]\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	scopes, err := DiscoverMiseScopes(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(scopes) != 2 || scopes[1] != filepath.Join(root, "app") {
		t.Fatalf("unexpected scopes: %v", scopes)
	}
}
