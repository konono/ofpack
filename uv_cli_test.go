package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A lower bound is not the Python the project actually uses. This must fail
// before Podman or mise tries to install the oldest allowed interpreter.
func TestUVPackUsesPythonLowerBoundWithoutMise(t *testing.T) {
	dir := t.TempDir()
	for name, data := range map[string]string{
		"pyproject.toml": "[project]\nname = \"demo\"\nversion = \"0.1\"\nrequires-python = \">= 3.8\"\ndependencies = [\"ansible>=9.5.1\"]\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	output := filepath.Join(dir, "out.tar.gz")
	err := cmdPack([]string{"--uv-project", "--target", "linux/amd64", "--print-command", "-C", dir, "-o", output})
	if err != nil {
		t.Fatalf("requires-python lower bound should select Python: %v", err)
	}
	if _, statErr := os.Stat(output); !os.IsNotExist(statErr) {
		t.Fatalf("pack wrote an archive despite an ambiguous Python choice: %v", statErr)
	}
}

func TestUVPackUsesProjectMisePythonWithoutRequiresPython(t *testing.T) {
	dir := t.TempDir()
	for name, data := range map[string]string{
		"pyproject.toml": "[project]\nname = \"demo\"\nversion = \"0.1\"\ndependencies = [\"ansible>=9.5.1\"]\n",
		"mise.toml":      "[tools]\npython = \"latest\"\nother = \"1\"\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	output := filepath.Join(dir, "out.tar.gz")
	if err := cmdPack([]string{"--uv-project", "--target", "linux/amd64", "--print-command", "-C", dir, "-o", output}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatalf("--print-command unexpectedly wrote an archive: %v", err)
	}
}

func TestUVPackUsesRequiresPythonFloorWhenMiseSaysLatest(t *testing.T) {
	dir := t.TempDir()
	for name, data := range map[string]string{
		"pyproject.toml": "[project]\nname = \"demo\"\nversion = \"0.1\"\nrequires-python = \">=3.11\"\ndependencies = [\"ansible>=9.5.1\"]\n",
		"mise.toml":      "[tools]\npython = \"latest\"\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := cmdPack([]string{"--uv-project", "--target", "linux/amd64", "--print-command", "-C", dir, "-o", filepath.Join(dir, "out.tar.gz")}); err != nil {
		t.Fatal(err)
	}
}

func TestUVPackFindsNestedProjectAndRejectsTwo(t *testing.T) {
	root := t.TempDir()
	app := filepath.Join(root, "app")
	if err := os.MkdirAll(app, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "mise.toml"), []byte("[tools]\npython=\"3.12\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	py := "[project]\nname=\"demo\"\nversion=\"0.1\"\nrequires-python=\">=3.11\"\ndependencies=[\"dep\"]\n"
	if err := os.WriteFile(filepath.Join(app, "pyproject.toml"), []byte(py), 0o644); err != nil {
		t.Fatal(err)
	}
	args := []string{"--uv-project", "--target", "linux/amd64", "--print-command", "-C", root, "-o", filepath.Join(root, "out.tar.gz")}
	if err := cmdPack(args); err != nil {
		t.Fatalf("nested project pack: %v", err)
	}
	other := filepath.Join(root, "other")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, "pyproject.toml"), []byte(py), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := cmdPack(args); err == nil || !strings.Contains(err.Error(), "multiple pyproject.toml") {
		t.Fatalf("ambiguous projects: %v", err)
	}
}

func TestUVPackDoesNotReadMiseAbovePackRoot(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "ansible_materials", "ansible_lab")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "mise.toml"), []byte("[tools]\npython = \"latest\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "pyproject.toml"), []byte("[project]\nname = \"demo\"\nversion = \"0.1\"\nrequires-python = \">=3.11\"\ndependencies = [\"ansible>=9.5.1\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := cmdPack([]string{"--uv-project", "--target", "linux/amd64", "--print-command", "-C", project, "-o", filepath.Join(root, "out.tar.gz")})
	if err != nil {
		t.Fatalf("pyproject lower bound should be usable while parent mise is ignored: %v", err)
	}
}

func TestUVPackRejectsProjectMisePythonThatConflicts(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "ansible_lab")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	for path, data := range map[string]string{
		filepath.Join(root, "mise.toml"):         "[tools]\npython = \"3.12\"\n",
		filepath.Join(project, "mise.toml"):      "[tools]\npython = \"3.10\"\n",
		filepath.Join(project, "pyproject.toml"): "[project]\nname = \"demo\"\nversion = \"0.1\"\nrequires-python = \">=3.11\"\ndependencies = [\"ansible>=9.5.1\"]\n",
	} {
		if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	err := cmdPack([]string{"--uv-project", "--target", "linux/amd64", "-C", project, "-o", filepath.Join(root, "out.tar.gz")})
	if err == nil || !strings.Contains(err.Error(), "3.10") || !strings.Contains(err.Error(), "requires-python") {
		t.Fatalf("nearest conflicting Python should fail before Podman: %v", err)
	}
}

func TestUVPackRejectsConflictingRangeWithMiseLatest(t *testing.T) {
	dir := t.TempDir()
	for name, data := range map[string]string{
		"pyproject.toml": "[project]\nname = \"demo\"\nversion = \"0.1\"\nrequires-python = \">=3.12,<3.12\"\ndependencies = [\"demo-dep\"]\n",
		"mise.toml":      "[tools]\npython = \"latest\"\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	err := cmdPack([]string{"--uv-project", "--target", "linux/amd64", "-C", dir, "-o", filepath.Join(dir, "out.tar.gz")})
	if err == nil || !strings.Contains(err.Error(), "conflicting constraints") {
		t.Fatalf("expected an early range conflict: %v", err)
	}
}

func TestExplicitPythonDoesNotReadProjectMiseToml(t *testing.T) {
	dir := t.TempDir()
	for name, data := range map[string]string{
		"pyproject.toml": "[project]\nname = \"demo\"\nversion = \"0.1\"\ndependencies = [\"ansible>=9.5.1\"]\n",
		"mise.toml":      "[tools]\npython = [\"3.12\", \"3.13\"]\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := cmdPack([]string{"--uv-project", "--python", "3.12", "--target", "linux/amd64", "--print-command", "-C", dir, "-o", filepath.Join(dir, "out.tar.gz")}); err != nil {
		t.Fatal(err)
	}
}

func TestLatestMisePythonFailureSuggestsAPinAndKeepsExistingOutput(t *testing.T) {
	dir := t.TempDir()
	for name, data := range map[string]string{
		"pyproject.toml": "[project]\nname = \"demo\"\nversion = \"0.1\"\ndependencies = [\"ansible>=9.5.1\"]\n",
		"mise.toml":      "[tools]\npython = \"latest\"\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	engine := filepath.Join(dir, "failing-engine")
	if err := os.WriteFile(engine, []byte("#!/bin/sh\nexit 17\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(dir, "existing.tar.gz")
	if err := os.WriteFile(output, []byte("previous archive"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := uvPack("linux/amd64", output, dir, "", "", "", engine, "", false, false, false)
	if err == nil || !strings.Contains(err.Error(), "--python X.Y") || !strings.Contains(err.Error(), "was not replaced") {
		t.Fatalf("latest failure should suggest a pin and account for the old archive: %v", err)
	}
	contents, readErr := os.ReadFile(output)
	if readErr != nil || string(contents) != "previous archive" {
		t.Fatalf("old archive changed on failure: %q, %v", contents, readErr)
	}
}

// captureStderr runs fn with os.Stderr redirected and returns what it wrote.
// The Python selection is reported there, and these tests are about which
// configuration file that choice came from.
func captureStderr(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stderr
	os.Stderr = w
	done := make(chan string, 1)
	go func() {
		var sb strings.Builder
		buf := make([]byte, 4096)
		for {
			n, readErr := r.Read(buf)
			sb.Write(buf[:n])
			if readErr != nil {
				break
			}
		}
		done <- sb.String()
	}()
	runErr := fn()
	os.Stderr = saved
	w.Close()
	out := <-done
	r.Close()
	return out, runErr
}

// uvProjectAt writes a project and runs a --print-command pack over it,
// returning what was reported.
func uvProjectAt(t *testing.T, root string, files map[string]string, args ...string) (string, error) {
	t.Helper()
	for name, data := range files {
		full := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	base := []string{"--uv-project", "--target", "linux/amd64", "--print-command",
		"-C", root, "-o", filepath.Join(root, "out.tar.gz")}
	return captureStderr(t, func() error { return cmdPack(append(base, args...)) })
}

const uvTestPyproject = "[project]\nname = \"demo\"\nversion = \"0.1\"\nrequires-python = \">=3.10\"\ndependencies = [\"ansible>=9.5.1\"]\n"

// TOML allows a quoted key and mise honours it - and mpack writes that form
// itself. Reading only the bare form meant the configuration was skipped and
// a different Python was chosen with no warning.
func TestUVPackReadsQuotedPythonKeyInMiseConfig(t *testing.T) {
	for _, quoted := range []string{`"python" = "3.13"`, `'python' = '3.13'`} {
		root := t.TempDir()
		out, err := uvProjectAt(t, root, map[string]string{
			"pyproject.toml": uvTestPyproject,
			"mise.toml":      "[tools]\n" + quoted + "\n",
		})
		if err != nil {
			t.Fatalf("%s: %v\n%s", quoted, err, out)
		}
		if !strings.Contains(out, "python 3.13") {
			t.Errorf("%s was not honoured; mpack reported:\n%s", quoted, out)
		}
		if strings.Contains(out, "requires-python lower bound") {
			t.Errorf("%s was skipped and the choice fell through:\n%s", quoted, out)
		}
	}
}

// DiscoverMiseScopes treats nine filenames as mise configuration. The Python
// selection has to read the same ones, or a scope mpack packs is a scope it
// cannot interpret.
func TestUVPackReadsEveryMiseConfigFilename(t *testing.T) {
	for _, name := range []string{
		".mise.local.toml", "mise.local.toml", ".mise.toml", "mise.toml",
		"mise/config.toml", ".mise/config.toml",
		".config/mise.toml", ".config/mise/config.toml",
	} {
		root := t.TempDir()
		out, err := uvProjectAt(t, root, map[string]string{
			"pyproject.toml": uvTestPyproject,
			name:             "[tools]\npython = \"3.13\"\n",
		})
		if err != nil {
			t.Fatalf("%s: %v\n%s", name, err, out)
		}
		if !strings.Contains(out, "python 3.13") {
			t.Errorf("%s was not read; mpack reported:\n%s", name, out)
		}
	}
	// .tool-versions is not TOML and needs its own reader.
	root := t.TempDir()
	out, err := uvProjectAt(t, root, map[string]string{
		"pyproject.toml": uvTestPyproject,
		".tool-versions": "nodejs 20.0.0\npython 3.13\n",
	})
	if err != nil {
		t.Fatalf(".tool-versions: %v\n%s", err, out)
	}
	if !strings.Contains(out, "python 3.13") {
		t.Errorf(".tool-versions was not read:\n%s", out)
	}
}

// A nested project's own configuration wins over the pack root's, and the
// nested file may be any of the recognised names.
func TestUVPackPrefersNestedMiseConfigOverRoot(t *testing.T) {
	root := t.TempDir()
	out, err := uvProjectAt(t, root, map[string]string{
		"mise.toml":          "[tools]\npython = \"3.11\"\n",
		"sub/pyproject.toml": uvTestPyproject,
		"sub/.mise.toml":     "[tools]\n\"python\" = \"3.13\"\n",
	})
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, "python 3.13") {
		t.Errorf("the nested .mise.toml did not win over the root mise.toml:\n%s", out)
	}
}

// Within one directory the files are read in mise's own order, measured with
// `mise config ls`; the wrong order would pick a version the directory does
// not use.
func TestUVPackFollowsMiseConfigPrecedence(t *testing.T) {
	root := t.TempDir()
	out, err := uvProjectAt(t, root, map[string]string{
		"pyproject.toml":   uvTestPyproject,
		"mise.toml":        "[tools]\npython = \"3.10\"\n",
		".mise.toml":       "[tools]\npython = \"3.11\"\n",
		"mise.local.toml":  "[tools]\npython = \"3.12\"\n",
		".mise.local.toml": "[tools]\npython = \"3.13\"\n",
		".tool-versions":   "python 3.9\n",
	})
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, "python 3.13") {
		t.Errorf(".mise.local.toml should win; mpack reported:\n%s", out)
	}
}

// A configuration that names a Python mpack cannot interpret must stop the
// pack. Falling through to the next source would build the environment on a
// different interpreter than the directory asks for, and that only shows up
// on the disconnected machine.
func TestUVPackRefusesUninterpretablePythonInsteadOfFallingThrough(t *testing.T) {
	cases := map[string]string{
		"several versions": "[tools]\npython = [\"3.12\", \"3.11\"]\n",
		"table form":       "[tools]\npython = { version = \"3.12\" }\n",
		"empty value":      "[tools]\npython = \"\"\n",
	}
	for name, body := range cases {
		root := t.TempDir()
		out, err := uvProjectAt(t, root, map[string]string{
			"pyproject.toml": uvTestPyproject,
			"mise.toml":      body,
		})
		if err == nil {
			t.Errorf("%s: pack continued instead of stopping:\n%s", name, out)
			continue
		}
		if !strings.Contains(err.Error(), "cannot interpret") {
			t.Errorf("%s: unhelpful error: %v", name, err)
		}
		// It must not have quietly used the requires-python floor instead.
		if strings.Contains(out, "requires-python lower bound") {
			t.Errorf("%s: the choice fell through to another source:\n%s", name, out)
		}
	}
	// The same applies to .tool-versions.
	root := t.TempDir()
	out, err := uvProjectAt(t, root, map[string]string{
		"pyproject.toml": uvTestPyproject,
		".tool-versions": "python 3.12 3.11\n",
	})
	if err == nil || !strings.Contains(err.Error(), "cannot interpret") {
		t.Errorf("an ambiguous .tool-versions should stop the pack, got %v\n%s", err, out)
	}
}

// A file with no Python at all is not an answer: the next file down is tried,
// which is what mise does.
func TestUVPackFallsThroughAFileWithNoPython(t *testing.T) {
	root := t.TempDir()
	out, err := uvProjectAt(t, root, map[string]string{
		"pyproject.toml":   uvTestPyproject,
		".mise.local.toml": "[tools]\njq = \"1.8.1\"\n",
		"mise.toml":        "[tools]\npython = \"3.13\"\n",
	})
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, "python 3.13") {
		t.Errorf("a config without a python entry should not stop the search:\n%s", out)
	}
}
