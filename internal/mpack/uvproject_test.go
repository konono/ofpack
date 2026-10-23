package mpack

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// Fixtures here are written from scratch. The project this feature was built
// for holds credentials, and none of its content belongs in a test.
const samplePyproject = `[project]
name = "example-installer"
version = "0.1.0"
requires-python = ">=3.12,<3.13"
dependencies = [
    "pkg-core>=2.17,<3.0",
    "pkg-extra>=10.0",
    "other-dep>=29.0",
]
`

const sampleLock = `version = 1
requires-python = "==3.12.*"

[[package]]
name = "pkg-core"
version = "2.17.4"
source = { registry = "https://example.invalid/simple" }

[[package]]
name = "transitive-dep"
version = "1.2.3"
source = { registry = "https://example.invalid/simple" }

[[package]]
name = "example-installer"
version = "0.1.0"
source = { virtual = "." }
`

func writeProject(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// A requirement's own comma is not an item separator. Splitting on every
// comma turns "pkg-core>=2.17,<3.0" into two broken requirements and quietly
// changes which version gets installed.
func TestDependenciesKeepCommasInsideVersionRanges(t *testing.T) {
	dir := writeProject(t, map[string]string{projectFiles.Pyproject: samplePyproject})
	s, err := ReadUVProject(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"pkg-core>=2.17,<3.0", "pkg-extra>=10.0", "other-dep>=29.0"}
	if len(s.Requirements) != len(want) {
		t.Fatalf("got %d requirements %v, want %d", len(s.Requirements), s.Requirements, len(want))
	}
	for i, w := range want {
		if s.Requirements[i] != w {
			t.Errorf("requirement %d = %q, want %q", i, s.Requirements[i], w)
		}
	}
}

// requirements.txt may list fewer packages than pyproject declares. Driving
// the install from it would leave the missing ones out of the environment
// altogether, so pyproject is the source of truth and the gap is reported.
func TestRequirementsTxtIsNotTheSourceOfTruth(t *testing.T) {
	dir := writeProject(t, map[string]string{
		projectFiles.Pyproject: samplePyproject,
		// Only two of the three, as a thinned-out requirements.txt tends to be.
		projectFiles.Requirements: "other-dep>=29.0\npkg_extra>=10.0\n# a comment\n",
	})
	s, err := ReadUVProject(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Requirements) != 3 {
		t.Fatalf("the install set must come from pyproject: %v", s.Requirements)
	}
	missing := s.MissingFromRequirementsTxt()
	if len(missing) != 1 || missing[0] != "pkg-core" {
		t.Fatalf("missing = %v, want [pkg-core]", missing)
	}
	// Underscores and dashes are the same distribution.
	if got := normalizeDist("Pkg_Extra"); got != "pkg-extra" {
		t.Errorf("normalizeDist = %q", got)
	}
}

// The lock pins versions, but it is optional and it constrains rather than
// requires: it covers every platform, and the project itself is not a wheel.
func TestLockPinsAreConstraintsAndExcludeTheProject(t *testing.T) {
	dir := writeProject(t, map[string]string{
		projectFiles.Pyproject: samplePyproject,
		projectFiles.Lock:      sampleLock,
	})
	s, err := ReadUVProject(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !s.HasLock {
		t.Fatal("the lock was not picked up")
	}
	want := []string{"pkg-core==2.17.4", "transitive-dep==1.2.3"}
	if strings.Join(s.Constraints, ",") != strings.Join(want, ",") {
		t.Fatalf("constraints = %v, want %v (the project itself must not be pinned)", s.Constraints, want)
	}

	// Without a lock the project still packs; pinning is a bonus, not a
	// requirement.
	noLock := writeProject(t, map[string]string{projectFiles.Pyproject: samplePyproject})
	s2, err := ReadUVProject(noLock)
	if err != nil {
		t.Fatalf("a project without a lock must still be packable: %v", err)
	}
	if s2.HasLock || len(s2.Constraints) != 0 {
		t.Errorf("unexpected lock state: %+v", s2)
	}
}

// Only three files are ever opened. A project directory can hold
// credentials, and mpack has no business reading them.
func TestOnlyDependencyFilesAreRead(t *testing.T) {
	dir := writeProject(t, map[string]string{
		projectFiles.Pyproject:     samplePyproject,
		projectFiles.Lock:          sampleLock,
		projectFiles.Requirements:  "other-dep\n",
		".credentials":             "SECRET_TOKEN=must-not-be-read\n",
		".generated-passwords.yml": "admin: must-not-be-read\n",
		".envrc":                   "source .venv/bin/activate\n",
		"inventory":                "[all]\nhost1\n",
	})
	// Make the secrets unreadable: if anything tried to open them, it would
	// fail rather than quietly succeed on a developer's machine.
	for _, secret := range []string{".credentials", ".generated-passwords.yml"} {
		if err := os.Chmod(filepath.Join(dir, secret), 0o000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(filepath.Join(dir, secret), 0o644) })
	}
	s, err := ReadUVProject(dir)
	if err != nil {
		t.Fatalf("reading the project must not touch the secrets: %v", err)
	}

	// Nothing from those files may appear in what gets packed or recorded.
	plan := &Plan{Manifest: Manifest{Tools: []Tool{
		{Name: "python", Version: "3.12.15"}, {Name: "uv", Version: "0.13.5"},
	}}}
	rec := &UVProject{
		Requirements: s.Requirements, Constraints: s.Constraints,
		PyprojectSHA256: s.PyprojectSHA, LockSHA256: s.LockSHA,
	}
	plan.Manifest.UVProject = rec
	encoded, err := json.Marshal(plan.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"must-not-be-read", "SECRET_TOKEN", "host1", "activate"} {
		if strings.Contains(string(encoded), leak) {
			t.Errorf("the manifest contains %q from a file that should not have been read:\n%s", leak, encoded)
		}
	}
}

func TestPythonSpecComesFromTheProject(t *testing.T) {
	dir := writeProject(t, map[string]string{projectFiles.Pyproject: samplePyproject})
	s, err := ReadUVProject(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, err := PythonSpecFor(s)
	if err != nil || got != "3.12" {
		t.Errorf("PythonSpecFor = %q, %v; want 3.12 from requires-python", got, err)
	}
	for _, c := range []struct{ in, want string }{
		{">=3.12,<3.13", "3.12"}, {"==3.12.*", "3.12"}, {">=3.11", "3.11"},
		{"~=3.13.0", "3.13"},
		// An upper bound names versions that are excluded. Reading "<3.12"
		// as "3.12" picks the first version the project has ruled out.
		{"<3.12", ""}, {"<=3.13", ""}, {"!=3.11", ""}, {"", ""},
	} {
		if got := pythonSpecFromRequires(c.in); got != c.want {
			t.Errorf("pythonSpecFromRequires(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// A Python chosen by hand, or derived, must be one the project accepts.
func TestPythonSatisfiesRequiresPython(t *testing.T) {
	cases := []struct {
		spec, requires string
		ok             bool
	}{
		{"3.12", ">=3.12,<3.13", true},
		{"3.13", ">=3.12,<3.13", false},   // the upper bound excludes it
		{"3.14.0", ">=3.12,<3.13", false}, // a target-resolved latest must be checked too
		{"3.11", ">=3.12,<3.13", false},
		{"3.12", "==3.12.*", true},
		{"3.13", "==3.12.*", false},
		{"3.14", ">=3.11", true},
		{"3.11", "!=3.11", false},
		{"3.12", "", true}, // nothing stated, nothing to violate
		{"3.12", "<=3.13", true},
		{"3.13", ">3.13", false},
	}
	for _, c := range cases {
		got, why := PythonSatisfies(c.spec, c.requires)
		if got != c.ok {
			t.Errorf("PythonSatisfies(%q, %q) = %v (%s), want %v", c.spec, c.requires, got, why, c.ok)
		}
	}
}

func TestPythonFloorForLatest(t *testing.T) {
	for _, tc := range []struct{ requires, want string }{
		{">= 3.11", "3.11"},
		{">=3.8,>=3.11,<3.13", "3.11"},
		{">3.11,<3.13", "3.12"},
		{"", ""},
		{"<3.13", ""},
	} {
		got, err := PythonFloorForLatest(tc.requires)
		if err != nil || got != tc.want {
			t.Errorf("PythonFloorForLatest(%q) = %q, %v; want %q", tc.requires, got, err, tc.want)
		}
	}
	if _, err := PythonFloorForLatest(">=3.12,<3.12"); err == nil || !strings.Contains(err.Error(), "conflicting") {
		t.Fatalf("contradictory range should fail before installation: %v", err)
	}
}

func TestProjectMisePythonReadsLocalSelector(t *testing.T) {
	dir := writeProject(t, map[string]string{
		"mise.toml": "[tools]\npython = \"latest\" # target resolves it\nnode = \"22\"\n[env]\nTOKEN = \"synthetic-secret\"\n",
	})
	selector, read, err := ProjectMisePython(dir)
	if err != nil || !read || selector != "latest" {
		t.Fatalf("project mise Python = %q, read %v, error %v; want latest", selector, read, err)
	}
	if strings.Contains(selector, "synthetic-secret") {
		t.Fatal("an unrelated mise setting leaked into the Python selector")
	}
	if selector, read, err := ProjectMisePython(t.TempDir()); err != nil || read || selector != "" {
		t.Fatalf("missing mise.toml = %q, read %v, error %v", selector, read, err)
	}
}

func TestProjectMisePythonIgnoresParent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	project := filepath.Join(home, "course", "materials", "ansible_lab")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	parentConfig := filepath.Join(home, "course", "mise.toml")
	if err := os.WriteFile(parentConfig, []byte("[tools]\npython = \"latest\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, read, err := ProjectMisePython(project); err != nil || read || got != "" {
		t.Fatalf("parent selection should be ignored: %q, read=%v, err=%v", got, read, err)
	}
	localConfig := filepath.Join(project, "mise.toml")
	if err := os.WriteFile(localConfig, []byte("[tools]\nnode = \"22\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, read, err := ProjectMisePython(project); err != nil || !read || got != "" {
		t.Fatalf("local mise.toml without Python should not inherit the parent: %q, read=%v, err=%v", got, read, err)
	}
	if err := os.WriteFile(localConfig, []byte("[tools]\npython = \"3.12\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, read, err := ProjectMisePython(project); err != nil || !read || got != "3.12" {
		t.Fatalf("local selection = %q, read=%v, err=%v; want 3.12", got, read, err)
	}
}

func TestProjectMisePythonRefusesAnAmbiguousToolValue(t *testing.T) {
	dir := writeProject(t, map[string]string{"mise.toml": "[tools]\npython = [\"3.12\", \"3.13\"]\n"})
	_, _, err := ProjectMisePython(dir)
	if err == nil || !strings.Contains(err.Error(), "--python") {
		t.Fatalf("ambiguous [tools].python should require an explicit choice: %v", err)
	}
}

// A project whose requires-python names no installable version must say so
// rather than guessing a default.
func TestPythonSpecRefusesToGuess(t *testing.T) {
	for _, requires := range []string{"<3.12", "<=3.13", ""} {
		body := "[project]\nname = \"x\"\nversion = \"1\"\n"
		if requires != "" {
			body += "requires-python = \"" + requires + "\"\n"
		}
		body += "dependencies = [\"dep\"]\n"
		dir := writeProject(t, map[string]string{projectFiles.Pyproject: body})
		s, err := ReadUVProject(dir)
		if err != nil {
			t.Fatal(err)
		}
		spec, err := PythonSpecFor(s)
		if err == nil {
			t.Errorf("requires-python = %q yielded %q instead of an error", requires, spec)
		}
		if err != nil && !strings.Contains(err.Error(), "--python") {
			t.Errorf("the error should point at --python: %v", err)
		}
	}
}

func TestPythonSpecUsesBroadLowerBound(t *testing.T) {
	for _, tc := range []struct{ requires, want string }{
		{">=3.8", "3.8"}, {">=3.12,<3.14", "3.12"}, {">=3.8,!=3.9,<3.11", "3.8"},
	} {
		dir := writeProject(t, map[string]string{projectFiles.Pyproject: "[project]\nname=\"x\"\nversion=\"1\"\nrequires-python=\"" + tc.requires + "\"\ndependencies=[\"dep\"]\n"})
		s, err := ReadUVProject(dir)
		if err != nil {
			t.Fatal(err)
		}
		got, err := PythonSpecFor(s)
		if err != nil || got != tc.want {
			t.Errorf("%q: got %q, %v; want %q", tc.requires, got, err, tc.want)
		}
	}
}

func TestFindUVProjectSelectsOneAndRejectsAmbiguity(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "app")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := FindUVProject(root); err == nil {
		t.Fatal("missing pyproject was accepted")
	}
	if err := os.WriteFile(filepath.Join(project, "pyproject.toml"), []byte("[project]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, skip := range []string{".venv", ".git", "node_modules", ".mpack"} {
		dir := filepath.Join(root, skip)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "pyproject.toml"), []byte("[project]\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(project, filepath.Join(root, "linked-app")); err != nil {
		t.Fatal(err)
	}
	if got, err := FindUVProject(root); err != nil || got != project {
		t.Fatalf("got %s, %v; want %s", got, err, project)
	}
	other := filepath.Join(root, "other")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, "pyproject.toml"), []byte("[project]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := FindUVProject(root); err == nil || !strings.Contains(err.Error(), "multiple pyproject.toml") {
		t.Fatalf("multiple projects were accepted: %v", err)
	}
}

func TestScopedMisePythonStopsAtRootAndChildOverrides(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "scope")
	child := filepath.Join(root, "app")
	if err := os.MkdirAll(child, 0o755); err != nil {
		t.Fatal(err)
	}
	for file, body := range map[string]string{
		filepath.Join(parent, "mise.toml"): "[tools]\npython=\"3.10\"\n",
		filepath.Join(root, "mise.toml"):   "[tools]\npython=\"3.11\"\n",
		filepath.Join(child, "mise.toml"):  "[tools]\npython=\"3.12\"\n",
	} {
		if err := os.WriteFile(file, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if got, _, _, err := ScopedMisePython(root, child); err != nil || got != "3.12" {
		t.Fatalf("got %q, %v", got, err)
	}
	if err := os.Remove(filepath.Join(child, "mise.toml")); err != nil {
		t.Fatal(err)
	}
	if got, _, _, err := ScopedMisePython(root, child); err != nil || got != "3.11" {
		t.Fatalf("got %q, %v", got, err)
	}
	if err := os.Remove(filepath.Join(root, "mise.toml")); err != nil {
		t.Fatal(err)
	}
	if got, _, _, err := ScopedMisePython(root, child); err != nil || got != "" {
		t.Fatalf("parent above root leaked: %q, %v", got, err)
	}
}

func TestGlobalMisePythonUsesConfiguredFileAndOnlyPython(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MISE_CONFIG_DIR", dir)
	t.Setenv("MISE_GLOBAL_CONFIG_FILE", "")
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte("[tools]\nnode=\"22\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "mise.toml"), []byte("[tools]\npython=\"3.12\"\n[env]\nTOKEN=\"synthetic-secret\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, source, err := GlobalMisePython()
	if err != nil || got != "3.12" || !strings.HasSuffix(source, "mise.toml") {
		t.Fatalf("got %q from %q, %v", got, source, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "explicit.toml"), []byte("[tools]\npython=\"3.11\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MISE_GLOBAL_CONFIG_FILE", filepath.Join(dir, "explicit.toml"))
	got, source, err = GlobalMisePython()
	if err != nil || got != "3.11" || !strings.HasSuffix(source, "explicit.toml") {
		t.Fatalf("explicit override: got %q from %q, %v", got, source, err)
	}
}

func TestPythonSpecAcceptsOnlyOneMinorRanges(t *testing.T) {
	for _, requires := range []string{">=3.12,<3.13", "==3.12.*", "~=3.12.0", ">=3.12,<=3.12"} {
		body := "[project]\nname = \"x\"\nversion = \"1\"\nrequires-python = \"" + requires + "\"\ndependencies = [\"dep\"]\n"
		project := writeProject(t, map[string]string{projectFiles.Pyproject: body})
		source, err := ReadUVProject(project)
		if err != nil {
			t.Fatal(err)
		}
		if got, err := PythonSpecFor(source); err != nil || got != "3.12" {
			t.Errorf("requires-python = %q chose %q, %v; want 3.12", requires, got, err)
		}
	}
}

func TestReadUVProjectRejectsSomethingThatIsNotAUVProject(t *testing.T) {
	if _, err := ReadUVProject(t.TempDir()); err == nil ||
		!strings.Contains(err.Error(), projectFiles.Pyproject) {
		t.Fatalf("a directory with no pyproject.toml should say so, got %v", err)
	}
	noDeps := writeProject(t, map[string]string{
		projectFiles.Pyproject: "[project]\nname = \"x\"\nversion = \"1\"\n",
	})
	if _, err := ReadUVProject(noDeps); err == nil || !strings.Contains(err.Error(), "dependencies") {
		t.Fatalf("a project with no dependencies should say so, got %v", err)
	}
}

// The generated build script must never install from requirements.txt, must
// not swallow a download failure, and must build its own mise configuration
// rather than using the packing machine's.
func TestUVBuildScriptIsSelfContainedAndFailsLoudly(t *testing.T) {
	dir := writeProject(t, map[string]string{
		projectFiles.Pyproject: samplePyproject,
		projectFiles.Lock:      sampleLock,
	})
	s, err := ReadUVProject(dir)
	if err != nil {
		t.Fatal(err)
	}
	script := UVBuildScript(s, "3.12")

	for _, want := range []string{
		`"python" = "3.12"`, `"uv" = "latest"`, // its own configuration
		"pip download", "--only-binary=:all:",
		"-c /mpack-out/" + ConstraintsName, // the lock, as constraints
		"pkg-core>=2.17,<3.0",              // the real requirement, comma intact
	} {
		if !strings.Contains(script, want) {
			t.Errorf("the build script is missing %q:\n%s", want, script)
		}
	}
	// A wheel that cannot be fetched has to stop the pack.
	if strings.Contains(script, "|| true") {
		t.Errorf("the build script swallows a failure somewhere:\n%s", script)
	}
	if !strings.Contains(script, `if [ "$wheels" -eq 0 ]`) {
		t.Errorf("the build script does not check that any wheels arrived:\n%s", script)
	}
	// The host's own mise configuration must not be consulted.
	for _, forbidden := range []string{"/host/mise-config", "/host/global-config"} {
		if strings.Contains(script, forbidden) {
			t.Errorf("the build script reads the host's mise configuration (%s):\n%s", forbidden, script)
		}
	}
	// Nothing of the project but its declaration is present.
	if strings.Contains(script, dir) {
		t.Errorf("the build script references the project directory:\n%s", script)
	}
}

// Packing without proving the closure installs would push the failure onto
// the disconnected machine, where there is no way to fix it.
func TestUVVerifyScriptProvesThereIsNoNetwork(t *testing.T) {
	script := UVVerifyScript()
	if !strings.Contains(script, "pypi.org") || !strings.Contains(script, "must run without network") {
		t.Errorf("the check does not confirm the network is really absent:\n%s", script)
	}
	for _, want := range []string{"--no-index", "--find-links /payload/" + WheelhouseDir, "pip check"} {
		if !strings.Contains(script, want) {
			t.Errorf("the check is missing %q:\n%s", want, script)
		}
	}
	if strings.Contains(script, "|| true") {
		t.Errorf("the check swallows a failure:\n%s", script)
	}
}

// Restore must refuse when the project no longer matches the archive: the
// wheels are the ones the old declaration needed.
func TestRestoreRefusesWhenTheProjectHasChanged(t *testing.T) {
	dir := writeProject(t, map[string]string{
		projectFiles.Pyproject: samplePyproject,
		projectFiles.Lock:      sampleLock,
	})
	s, err := ReadUVProject(dir)
	if err != nil {
		t.Fatal(err)
	}
	rec := &UVProject{PyprojectSHA256: s.PyprojectSHA, LockSHA256: s.LockSHA}
	if err := checkProjectMatches(dir, rec); err != nil {
		t.Fatalf("an unchanged project should match: %v", err)
	}

	if err := os.WriteFile(filepath.Join(dir, projectFiles.Pyproject), []byte(samplePyproject+"\n# edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err = checkProjectMatches(dir, rec)
	if err == nil || !strings.Contains(err.Error(), "has changed") {
		t.Fatalf("an edited pyproject.toml should be refused, got %v", err)
	}

	// A changed lock is refused too, and so is a lock that has gone missing.
	if err := os.WriteFile(filepath.Join(dir, projectFiles.Pyproject), []byte(samplePyproject), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, projectFiles.Lock), []byte(sampleLock+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := checkProjectMatches(dir, rec); err == nil || !strings.Contains(err.Error(), projectFiles.Lock) {
		t.Fatalf("an edited uv.lock should be refused, got %v", err)
	}
	if err := os.Remove(filepath.Join(dir, projectFiles.Lock)); err != nil {
		t.Fatal(err)
	}
	if err := checkProjectMatches(dir, rec); err == nil || !strings.Contains(err.Error(), "has none") {
		t.Fatalf("a missing uv.lock should be refused, got %v", err)
	}
}

// An existing .venv is the user's; it is never replaced without being asked,
// and --skip-existing does not touch it at all.
func TestRestoreRespectsAnExistingVenv(t *testing.T) {
	project := writeProject(t, map[string]string{projectFiles.Pyproject: samplePyproject})
	dest := filepath.Join(project, MpackSubdir)
	s, err := ReadUVProject(project)
	if err != nil {
		t.Fatal(err)
	}
	unpackedTree(t, dest)
	m := Manifest{Schema: SchemaVersion, UVProject: &UVProject{PyprojectSHA256: s.PyprojectSHA}}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dest, InstalledManifestName), data, 0o644); err != nil {
		t.Fatal(err)
	}

	venv := filepath.Join(project, VenvDir)
	if err := os.MkdirAll(venv, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(venv, "PRE-EXISTING")
	if err := os.WriteFile(marker, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, built, err := RestoreVenv(RestoreOptions{Project: project, Policy: OverwriteFail})
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("the default must refuse, got %v", err)
	}
	if built {
		t.Error("something was built despite the refusal")
	}

	_, built, err = RestoreVenv(RestoreOptions{Project: project, Policy: OverwriteSkip})
	if err != nil {
		t.Fatal(err)
	}
	if built {
		t.Error("--skip-existing reported building something")
	}
	if body, _ := os.ReadFile(marker); string(body) != "keep" {
		t.Fatalf("--skip-existing touched the existing .venv: %q", body)
	}
	if entries, _ := filepath.Glob(filepath.Join(project, ".venv.mpack-*")); len(entries) != 0 {
		t.Errorf("staging directories were left behind: %v", entries)
	}
}

// A failed build must leave the previous .venv exactly as it was.
func TestRestoreKeepsThePreviousVenvWhenTheBuildFails(t *testing.T) {
	project := writeProject(t, map[string]string{projectFiles.Pyproject: samplePyproject})
	dest := filepath.Join(project, MpackSubdir)
	s, err := ReadUVProject(project)
	if err != nil {
		t.Fatal(err)
	}
	unpackedTree(t, dest)
	// A mise that cannot find uv, so building fails.
	if err := os.WriteFile(filepath.Join(dest, filepath.FromSlash(MiseBinName)), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	m := Manifest{Schema: SchemaVersion, UVProject: &UVProject{PyprojectSHA256: s.PyprojectSHA}}
	data, _ := json.Marshal(m)
	if err := os.WriteFile(filepath.Join(dest, InstalledManifestName), data, 0o644); err != nil {
		t.Fatal(err)
	}

	venv := filepath.Join(project, VenvDir)
	if err := os.MkdirAll(venv, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(venv, "PRE-EXISTING"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, _, err := RestoreVenv(RestoreOptions{Project: project, Policy: OverwriteForce}); err == nil {
		t.Fatal("the build should have failed")
	}
	if body, _ := os.ReadFile(filepath.Join(venv, "PRE-EXISTING")); string(body) != "keep" {
		t.Fatalf("the previous .venv was lost: %q", body)
	}
	if entries, _ := filepath.Glob(filepath.Join(project, ".venv.mpack-*")); len(entries) != 0 {
		t.Errorf("staging directories were left behind: %v", entries)
	}
}

// An archive with no Python environment in it cannot restore one.
func TestRestoreRefusesAnArchiveWithoutAUVProject(t *testing.T) {
	project := writeProject(t, map[string]string{projectFiles.Pyproject: samplePyproject})
	dest := filepath.Join(project, MpackSubdir)
	unpackedTree(t, dest)
	if _, _, err := RestoreVenv(RestoreOptions{Project: project}); err == nil ||
		!strings.Contains(err.Error(), "--uv-project") {
		t.Fatalf("expected a clear error about the archive, got %v", err)
	}
}

// A venv built somewhere and moved into place must stay usable. Without
// --relocatable uv writes the build path into every console script, so the
// environment has a working python and broken entry points.
func TestVenvIsBuiltRelocatable(t *testing.T) {
	// buildVenv is exercised end to end elsewhere; here the flag itself is
	// pinned, because its absence fails only when a script is run.
	src, err := os.ReadFile("uvproject.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), `"venv", venv, "--relocatable"`) {
		t.Error("uv venv is not asked for a relocatable environment; moving it into place would break every console script")
	}
}

// An mpack that predates the uv payload drops the uv/ directory while
// reporting a successful unpack, leaving a tree that looks fine but cannot
// restore. The archive declares a schema such an mpack refuses, and this one
// must handle both.
func TestUVArchiveDeclaresANewerSchema(t *testing.T) {
	root := fakeSource(t, "python/3.12.15")
	plan, err := fakePlan(t, root, false, "python/3.12.15")
	if err != nil {
		t.Fatal(err)
	}
	if plan.Manifest.Schema != SchemaVersion {
		t.Fatalf("a plain archive should stay at schema %d", SchemaVersion)
	}
	plan.Manifest.Tools = []Tool{
		{Name: "python", Version: "3.12.15", RelPath: "python/3.12.15"},
		{Name: "uv", Version: "0.13.5", RelPath: "uv/0.13.5"},
	}
	// AttachUVProject needs a payload to count wheels in; the schema bump is
	// what matters here, so set the record directly as it would be.
	plan.Manifest.UVProject = &UVProject{PythonVersion: "3.12.15", UvVersion: "0.13.5", Wheels: 1}
	plan.Manifest.Schema = SchemaVersionUV

	var buf bytes.Buffer
	if err := plan.Write(&buf); err != nil {
		t.Fatal(err)
	}
	m, err := ReadManifest(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("this mpack must read a uv archive: %v", err)
	}
	if m.Schema != SchemaVersionUV {
		t.Errorf("schema = %d, want %d", m.Schema, SchemaVersionUV)
	}
	// Both layouts are supported.
	if err := checkSchema(SchemaVersion); err != nil {
		t.Errorf("schema %d should still be readable: %v", SchemaVersion, err)
	}
	// A future layout is refused rather than partially unpacked.
	err = checkSchema(SchemaVersionScoped + 1)
	if err == nil || !strings.Contains(err.Error(), "silently leave parts out") {
		t.Errorf("an unknown schema should be refused with an explanation, got %v", err)
	}
}

// The wheel closure has to survive the unpack transaction and land in the
// destination. It did not, and the failure only appeared when restore could
// not find uv/requirements.txt on the target.
func TestUVPayloadIsInstalledByUnpack(t *testing.T) {
	const root = "/root/.local/share/mise/installs"
	payload := buildPayloadWithUV(t, root)

	plan, err := BuildPlanFromPayload(payload, "linux/amd64", PackOptions{WorkDir: t.TempDir(), Version: "t"})
	if err != nil {
		t.Fatal(err)
	}
	plan.Manifest.UVProject = &UVProject{PythonVersion: "3.12.15", UvVersion: "0.13.5", Wheels: 1}
	plan.Manifest.Schema = SchemaVersionUV

	var archive bytes.Buffer
	if err := plan.Write(&archive); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "out")
	if _, err := Unpack(bytes.NewReader(archive.Bytes()), UnpackOptions{Dest: dest, SkipArch: true}); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{
		filepath.FromSlash(RequirementsName),
		filepath.FromSlash(ConstraintsName),
		filepath.FromSlash(WheelhouseDir + "/example-1.0-py3-none-any.whl"),
	} {
		if _, err := os.Stat(filepath.Join(dest, rel)); err != nil {
			t.Errorf("unpack did not install %s: %v", rel, err)
		}
	}
	body, err := os.ReadFile(filepath.Join(dest, filepath.FromSlash(RequirementsName)))
	if err != nil || !strings.Contains(string(body), "example") {
		t.Errorf("the requirements file did not survive: %q %v", body, err)
	}
}

// buildPayloadWithUV is a payload carrying a python, a uv and a wheel.
func buildPayloadWithUV(t *testing.T, containerRoot string) string {
	t.Helper()
	base := buildPayload(t, containerRoot,
		map[string][]string{"python": {"3.12.15"}, "uv": {"0.13.5"}},
		[]payloadFixture{
			{rel: "python/3.12.15", dir: true, mode: 0o755},
			{rel: "python/3.12.15/bin", dir: true, mode: 0o755},
			{rel: "uv/0.13.5", dir: true, mode: 0o755},
		})
	// Append the uv section to the same stream.
	return appendToPayload(t, base, map[string]string{
		RequirementsName: "example>=1.0\n",
		ConstraintsName:  "example==1.0\n",
		WheelhouseDir + "/example-1.0-py3-none-any.whl": "not really a wheel",
	})
}

// A malformed manifest must produce an error, not a panic. The digest is
// read from an archive, which is not a trusted input.
func TestRestoreRejectsAMalformedHashWithoutPanicking(t *testing.T) {
	project := writeProject(t, map[string]string{projectFiles.Pyproject: samplePyproject})
	dest := filepath.Join(project, MpackSubdir)
	unpackedTree(t, dest)

	for _, bad := range []struct{ name, body string }{
		{"empty pyproject hash", `{"schema":2,"uv_project":{"pyproject_sha256":""}}`},
		{"short pyproject hash", `{"schema":2,"uv_project":{"pyproject_sha256":"abc"}}`},
		{"non-hex pyproject hash", `{"schema":2,"uv_project":{"pyproject_sha256":"` + strings.Repeat("z", 64) + `"}}`},
		{"short lock hash", `{"schema":2,"uv_project":{"pyproject_sha256":"` + strings.Repeat("a", 64) + `","lock_sha256":"ab"}}`},
	} {
		t.Run(bad.name, func(t *testing.T) {
			if err := os.WriteFile(filepath.Join(dest, InstalledManifestName), []byte(bad.body), 0o644); err != nil {
				t.Fatal(err)
			}
			// The panic this guards against was a slice out of range.
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("restore panicked on a malformed manifest: %v", r)
				}
			}()
			_, _, err := RestoreVenv(RestoreOptions{Project: project})
			if err == nil {
				t.Fatal("a malformed manifest should be an error")
			}
			if !strings.Contains(err.Error(), "malformed") && !strings.Contains(err.Error(), "records no") {
				t.Errorf("unhelpful error: %v", err)
			}
		})
	}
}

// mise reads configuration mpack cannot shut out, so `which` can answer with
// the machine's own Python or uv. Building the environment with those while
// believing the archive's were used produces an environment nobody packed.
func TestRestoreRefusesToolsFromOutsideTheArchive(t *testing.T) {
	dest := t.TempDir()
	inArchive := filepath.Join(dest, InstallsPrefix, "python", "3.12.15", "bin", "python")
	if err := os.MkdirAll(filepath.Dir(inArchive), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(inArchive, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := checkToolProvenance(dest, "python", "3.12.15", inArchive); err != nil {
		t.Errorf("the archive's own python was rejected: %v", err)
	}
	for _, outsider := range []string{
		"/usr/bin/python3",
		filepath.Join(dest, InstallsPrefix, "python", "3.11.9", "bin", "python"), // wrong version
		filepath.Join(dest, "..", "elsewhere", "python"),
	} {
		err := checkToolProvenance(dest, "python", "3.12.15", outsider)
		if err == nil {
			t.Errorf("accepted %s as the archive's python", outsider)
		} else if !strings.Contains(err.Error(), "not the python") {
			t.Errorf("unhelpful error for %s: %v", outsider, err)
		}
	}
	// A manifest with no recorded version cannot be checked, so it is refused.
	if err := checkToolProvenance(dest, "python", "", inArchive); err == nil {
		t.Error("a missing recorded version should be refused, not assumed fine")
	}
}

// appendToPayload rewrites a payload with extra files added to the stream.
func appendToPayload(t *testing.T, payloadPath string, extra map[string]string) string {
	t.Helper()
	in, err := os.Open(payloadPath)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	gzr, err := gzip.NewReader(in)
	if err != nil {
		t.Fatal(err)
	}
	defer gzr.Close()

	var buf bytes.Buffer
	gzw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gzw)
	tr := tar.NewReader(gzr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if hdr.Typeflag == tar.TypeReg {
			if _, err := io.CopyN(tw, tr, hdr.Size); err != nil {
				t.Fatal(err)
			}
		}
	}
	names := make([]string, 0, len(extra))
	for name := range extra {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := writeBytes(tw, name, 0o644, []byte(extra[name]), time.Unix(0, 0).UTC()); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzw.Close(); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "payload-uv.tar.gz")
	if err := os.WriteFile(out, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return out
}
