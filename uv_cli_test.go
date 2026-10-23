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
