package mpack

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The direnv file must not install a mise shell hook: direnv evaluates it in
// its own process and manages entering and leaving the directory itself.
func TestDirenvScriptInstallsNoShellHook(t *testing.T) {
	script := DirenvScript("/home/bob/work/myapp/.mpack")
	if strings.Contains(script, "mise\" activate") || strings.Contains(script, "mise activate") {
		t.Errorf("the direnv file installs a mise hook, which would fight direnv:\n%s", script)
	}
	for _, want := range []string{
		`export MISE_DATA_DIR="$MPACK_ROOT"`,
		`export MISE_CONFIG_DIR="$MPACK_ROOT/config"`,
		`export MISE_OFFLINE=1`,
		`-C / env -s bash`,
		`PATH_add "$MPACK_ROOT/bin"`,
		"watch_file",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("the direnv file is missing %q:\n%s", want, script)
		}
	}
}

// An inherited MISE_GLOBAL_CONFIG_FILE overrides MISE_CONFIG_DIR outright, so
// both generated scripts have to clear what they inherit and pin that
// variable themselves, or the snapshot silently loads someone else's config.
func TestGeneratedScriptsPinConfigAgainstInheritedOverrides(t *testing.T) {
	for name, script := range map[string]string{
		DirenvName:   DirenvScript("/home/bob/work/myapp/.mpack"),
		ActivateName: ActivateScript("/home/bob/work/myapp/.mpack"),
	} {
		if !strings.Contains(script, `unset "$_mpack_var"`) {
			t.Errorf("%s does not clear inherited MISE_* variables:\n%s", name, script)
		}
		if !strings.Contains(script, `export MISE_GLOBAL_CONFIG_FILE="$MPACK_ROOT/config/config.toml"`) {
			t.Errorf("%s does not pin MISE_GLOBAL_CONFIG_FILE, so an inherited one would win:\n%s", name, script)
		}
		if !strings.Contains(script, "-C / env -s bash") {
			t.Errorf("%s does not evaluate the snapshot with -C /:\n%s", name, script)
		}
	}
}

// unpackedTree builds a directory that looks like an unpacked archive.
func unpackedTree(t *testing.T, dir string, tools ...Tool) {
	t.Helper()
	for _, sub := range []string{"bin", "config"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []struct{ rel, body string }{
		{MiseBinName, "#!/bin/sh\n"},
		{ConfigName, string(RenderConfig(tools))},
		{DirenvName, DirenvScript(dir)},
		{ActivateName, ActivateScript(dir)},
	} {
		if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(f.rel)), []byte(f.body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	data, err := json.Marshal(Manifest{Schema: SchemaVersion, Tools: tools})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, InstalledManifestName), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestPlanDirenvUsesTheConventionalDestination(t *testing.T) {
	project := t.TempDir()
	dest := filepath.Join(project, MpackSubdir)
	unpackedTree(t, dest, Tool{Name: "jq", Version: "1.8.1", RelPath: "jq/1.8.1"})

	plan, err := PlanDirenv(project, "")
	if err != nil {
		t.Fatal(err)
	}
	if plan.Dest != dest {
		t.Errorf("dest = %q, want %q", plan.Dest, dest)
	}
	// A destination inside the project is referenced relatively, so the
	// .envrc survives the project being moved.
	if want := "source_env '.mpack/direnv.envrc'"; plan.Line != want {
		t.Errorf("line = %q, want %q", plan.Line, want)
	}
}

func TestPlanDirenvRejectsSomethingThatIsNotAnUnpackedTree(t *testing.T) {
	project := t.TempDir()
	if _, err := PlanDirenv(project, ""); err == nil || !strings.Contains(err.Error(), "unpack an archive first") {
		t.Fatalf("a project with no .mpack should say so, got %v", err)
	}
	empty := t.TempDir()
	_, err := PlanDirenv(project, empty)
	if err == nil || !strings.Contains(err.Error(), "does not look like an unpacked mpack directory") {
		t.Fatalf("a bogus --dest should be rejected, got %v", err)
	}
	if _, err := PlanDirenv(filepath.Join(project, "nope"), ""); err == nil {
		t.Fatal("a missing project directory should be rejected")
	}
}

// Paths with spaces have to survive into the .envrc, both for a destination
// inside the project and for one somewhere else entirely.
func TestPlanDirenvQuotesPathsWithSpaces(t *testing.T) {
	base := t.TempDir()
	project := filepath.Join(base, "my projects", "app one")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(project, MpackSubdir)
	unpackedTree(t, inside, Tool{Name: "jq", Version: "1.8.1", RelPath: "jq/1.8.1"})

	plan, err := PlanDirenv(project, "")
	if err != nil {
		t.Fatal(err)
	}
	if want := "source_env '.mpack/direnv.envrc'"; plan.Line != want {
		t.Errorf("line = %q, want %q", plan.Line, want)
	}
	if err := plan.WriteEnvrc(); err != nil {
		t.Fatal(err)
	}

	outside := filepath.Join(base, "tool chains", "app one")
	unpackedTree(t, outside, Tool{Name: "jq", Version: "1.8.1", RelPath: "jq/1.8.1"})
	plan2, err := PlanDirenv(project, outside)
	if err != nil {
		t.Fatal(err)
	}
	// A destination outside the project is absolute, and single quoted so the
	// spaces cannot split it into several words.
	if !strings.HasPrefix(plan2.Line, "source_env '/") || !strings.HasSuffix(plan2.Line, "/direnv.envrc'") {
		t.Errorf("an outside destination should be absolute and quoted: %q", plan2.Line)
	}
	if strings.Contains(plan2.Line, `\ `) {
		t.Errorf("escaping should come from quoting, not backslashes: %q", plan2.Line)
	}
	// What the line expands to must be the file itself.
	quoted := strings.TrimPrefix(plan2.Line, "source_env ")
	if got := strings.Trim(quoted, "'"); got != filepath.Join(outside, DirenvName) {
		t.Errorf("the quoted path is %q, want %q", got, filepath.Join(outside, DirenvName))
	}
}

// A project's .envrc belongs to the user: mpack prints the line to add rather
// than editing or appending to it.
func TestDirenvInitLeavesAnExistingEnvrcAlone(t *testing.T) {
	project := t.TempDir()
	unpackedTree(t, filepath.Join(project, MpackSubdir), Tool{Name: "jq", Version: "1.8.1", RelPath: "jq/1.8.1"})
	envrc := filepath.Join(project, EnvrcName)
	original := "export MY_OWN=1\nlayout python\n"
	if err := os.WriteFile(envrc, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	plan, err := PlanDirenv(project, "")
	if err != nil {
		t.Fatal(err)
	}
	if !plan.EnvrcExists {
		t.Fatal("the existing .envrc was not noticed")
	}
	if err := plan.WriteEnvrc(); err == nil {
		t.Fatal("WriteEnvrc should refuse to touch an existing file")
	}
	if got, _ := os.ReadFile(envrc); string(got) != original {
		t.Fatalf(".envrc was modified: %q", got)
	}
	out := plan.Instructions(false)
	if !strings.Contains(out, plan.Line) || !strings.Contains(out, "left alone") {
		t.Errorf("the user is not told what to add:\n%s", out)
	}
}

func TestDirenvInitWritesAMinimalEnvrcWhenThereIsNone(t *testing.T) {
	project := t.TempDir()
	unpackedTree(t, filepath.Join(project, MpackSubdir), Tool{Name: "jq", Version: "1.8.1", RelPath: "jq/1.8.1"})
	plan, err := PlanDirenv(project, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := plan.WriteEnvrc(); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(project, EnvrcName))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), plan.Line) {
		t.Errorf(".envrc lacks the source_env line:\n%s", body)
	}
	// Minimal means minimal: one directive and a comment.
	var directives int
	for _, line := range strings.Split(strings.TrimSpace(string(body)), "\n") {
		if line != "" && !strings.HasPrefix(line, "#") {
			directives++
		}
	}
	if directives != 1 {
		t.Errorf("expected a single directive, got %d:\n%s", directives, body)
	}
	// And it must not be written twice on a second run.
	plan2, err := PlanDirenv(project, "")
	if err != nil {
		t.Fatal(err)
	}
	if !plan2.EnvrcExists {
		t.Error("the freshly written .envrc was not detected on a second run")
	}
}

// The direnv file is generated by unpack, so it has to obey the same
// overwrite policy and rollback as everything else.
func TestDirenvSidecarGoesThroughTheUnpackTransaction(t *testing.T) {
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
	existing := filepath.Join(dest, DirenvName)
	if err := os.WriteFile(existing, []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err = Unpack(bytes.NewReader(archive), UnpackOptions{Dest: dest})
	if err == nil || !strings.Contains(err.Error(), DirenvName) {
		t.Fatalf("an existing %s should block the unpack, got %v", DirenvName, err)
	}
	if got, _ := os.ReadFile(existing); string(got) != "mine" {
		t.Fatalf("the existing file was overwritten: %q", got)
	}
	if _, err := Unpack(bytes.NewReader(archive), UnpackOptions{Dest: dest, Policy: OverwriteSkip}); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(existing); string(got) != "mine" {
		t.Fatalf("--skip-existing replaced it: %q", got)
	}
	if _, err := Unpack(bytes.NewReader(archive), UnpackOptions{Dest: dest, Policy: OverwriteForce}); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(existing); !strings.Contains(string(got), "PATH_add") {
		t.Fatalf("--force did not replace it: %q", got)
	}

	// And a failed unpack must not leave it behind.
	plan2, err := fakePlan(t, root, false, "node/22.1.0")
	if err != nil {
		t.Fatal(err)
	}
	plan2.Manifest.Tools = append(plan2.Manifest.Tools, Tool{Name: "ghost", Version: "1", RelPath: "ghost/1"})
	fresh := filepath.Join(t.TempDir(), "fresh")
	if _, err := Unpack(bytes.NewReader(packToBytes(t, plan2)), UnpackOptions{Dest: fresh}); err == nil {
		t.Fatal("expected the unpack to fail")
	}
	if _, err := os.Stat(filepath.Join(fresh, DirenvName)); !os.IsNotExist(err) {
		t.Errorf("%s survived a rolled back unpack", DirenvName)
	}
}

// Isolating a snapshot from every config mise might read cannot be asserted
// in the abstract, so mpack measures it. The check has to notice a
// difference, not just report success.
func TestCheckSnapshotNoticesAMismatch(t *testing.T) {
	dest := t.TempDir()
	tools := []Tool{{Name: "jq", Version: "1.8.1", RelPath: "jq/1.8.1"}}
	unpackedTree(t, dest, tools...)
	// A stub mise that reports a different tool set than the manifest.
	writeStubMise(t, dest, `{"node":[{"version":"22.1.0","active":true,"installed":true}]}`, 0)
	check, err := CheckSnapshot(dest)
	if err != nil {
		t.Fatal(err)
	}
	if check.OK() {
		t.Fatal("a differing environment was reported as matching")
	}
	if len(check.Extra) != 1 || check.Extra[0] != "node@22.1.0" {
		t.Errorf("extra = %v, want [node@22.1.0]", check.Extra)
	}
	if len(check.Missing) != 1 || check.Missing[0] != "jq@1.8.1" {
		t.Errorf("missing = %v, want [jq@1.8.1]", check.Missing)
	}
	report := check.Report()
	for _, want := range []string{"does not match", "node@22.1.0", "jq@1.8.1"} {
		if !strings.Contains(report, want) {
			t.Errorf("report should mention %q:\n%s", want, report)
		}
	}

	// A matching environment is silent.
	writeStubMise(t, dest, `{"jq":[{"version":"1.8.1","active":true,"installed":true}]}`, 0)
	check, err = CheckSnapshot(dest)
	if err != nil {
		t.Fatal(err)
	}
	if !check.OK() {
		t.Fatalf("a matching environment was reported as differing: %+v", check)
	}
	if len(check.SystemConfigs) == 0 && check.Report() != "" {
		t.Errorf("a clean check should be silent, got %q", check.Report())
	}
}

// CheckSnapshot must run mise the way the generated scripts do, or it would
// be verifying something other than what the user gets.
func TestCheckSnapshotRunsMiseWithTheSnapshotEnvironment(t *testing.T) {
	dest := t.TempDir()
	unpackedTree(t, dest, Tool{Name: "jq", Version: "1.8.1", RelPath: "jq/1.8.1"})
	// This stub records the environment and arguments it was given.
	record := filepath.Join(dest, "seen.txt")
	stub := `#!/bin/sh
{ echo "ARGS: $*"; env | sed -n 's/^\(MISE_[A-Za-z0-9_]*=.*\)/\1/p'; } >> ` + shellQuote(record) + `
for a in "$@"; do
  case "$a" in
    env) printf 'export PATH=%s\n' "'/stub/bin:$PATH'"; exit 0 ;;
    ls)  printf '%s' '{"jq":[{"version":"1.8.1","active":true,"installed":true}]}'; exit 0 ;;
  esac
done
`
	if err := os.WriteFile(filepath.Join(dest, filepath.FromSlash(MiseBinName)), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	// A hostile value that would otherwise be inherited.
	t.Setenv("MISE_GLOBAL_CONFIG_FILE", "/tmp/somebody-elses.toml")

	if _, err := CheckSnapshot(dest); err != nil {
		t.Fatal(err)
	}
	seen, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	got := string(seen)
	if !strings.Contains(got, "ARGS: -C / env -s bash") {
		t.Errorf("the environment command itself was never run:\n%s", got)
	}
	if !strings.Contains(got, "ARGS: -C / ls --current --json") {
		t.Errorf("mise was not run with -C /:\n%s", got)
	}
	if strings.Contains(got, "somebody-elses.toml") {
		t.Errorf("the inherited MISE_GLOBAL_CONFIG_FILE leaked through:\n%s", got)
	}
	for _, want := range []string{
		"MISE_GLOBAL_CONFIG_FILE=" + filepath.Join(dest, "config", "config.toml"),
		"MISE_DATA_DIR=" + dest,
		"MISE_OFFLINE=1",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in the environment:\n%s", want, got)
		}
	}
}

// The direnv file must not bake in the path it was generated at: direnv
// sources it from its own directory, so deriving the root keeps a moved or
// cloned project working.
func TestDirenvScriptDerivesItsOwnRoot(t *testing.T) {
	dest := "/home/bob/work/myapp/.mpack"
	script := DirenvScript(dest)
	if !strings.Contains(script, `MPACK_ROOT="$PWD"`) {
		t.Errorf("the root is not derived at evaluation time:\n%s", script)
	}
	if !strings.Contains(script, `watch_file "$MPACK_ROOT/`+InstalledManifestName+`"`) {
		t.Errorf("watch_file does not use the derived root:\n%s", script)
	}
	// The generated path may appear in comments, but nothing executable may
	// depend on it.
	for i, line := range strings.Split(script, "\n") {
		code := strings.TrimSpace(line)
		if code == "" || strings.HasPrefix(code, "#") {
			continue
		}
		if strings.Contains(code, dest) {
			t.Errorf("line %d bakes in the generation path, so the project cannot move: %s", i+1, code)
		}
	}
	// A tree that is not an unpacked archive has to say so rather than
	// silently producing an empty environment.
	if !strings.Contains(script, "does not contain bin/mise") {
		t.Errorf("no guard for being sourced from the wrong place:\n%s", script)
	}
}

// stubMiseTree is an unpacked tree whose mise answers both of the commands
// mpack relies on: the tool listing, and the environment itself. envExit
// makes the environment command fail, which is the interesting case - a mise
// that lists tools happily but cannot produce an environment.
func stubMiseTree(t *testing.T, dir, listJSON string, envExit int, tools ...Tool) {
	t.Helper()
	unpackedTree(t, dir, tools...)
	writeStubMise(t, dir, listJSON, envExit)
}

func writeStubMise(t *testing.T, dir, listJSON string, envExit int) {
	t.Helper()
	stub := fmt.Sprintf(`#!/bin/sh
for a in "$@"; do
  case "$a" in
    env) exit_code=%d
         if [ "$exit_code" -ne 0 ]; then
           echo "stub mise: refusing to build an environment" >&2
           exit "$exit_code"
         fi
         printf 'export PATH=%%s
' "'/stub/bin:$PATH'"
         exit 0 ;;
    ls)  printf '%%s' %s; exit 0 ;;
  esac
done
exit 0
`, envExit, shellQuote(listJSON))
	if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(MiseBinName)), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
}

// A tool mise lists but has not installed is not usable, so it counts as
// missing rather than as a match.
func TestCheckSnapshotTreatsUninstalledAsMissing(t *testing.T) {
	dest := t.TempDir()
	stubMiseTree(t, dest, `{"jq":[{"version":"1.8.1","active":true,"installed":false}]}`, 0,
		Tool{Name: "jq", Version: "1.8.1", RelPath: "jq/1.8.1"})
	check, err := CheckSnapshot(dest)
	if err != nil {
		t.Fatal(err)
	}
	if check.OK() {
		t.Fatal("a tool that is listed but not installed was accepted as present")
	}
	if len(check.Missing) != 1 || check.Missing[0] != "jq@1.8.1" {
		t.Fatalf("missing = %v, want [jq@1.8.1]", check.Missing)
	}
}

// init must verify before it writes: an .envrc plus "run direnv allow" is an
// endorsement, and it must not be given for an environment that is wrong.
func TestInitDirenvWritesNothingWhenTheEnvironmentDoesNotMatch(t *testing.T) {
	project := t.TempDir()
	dest := filepath.Join(project, MpackSubdir)
	stubMiseTree(t, dest, `{"node":[{"version":"22.1.0","active":true,"installed":true}]}`, 0,
		Tool{Name: "jq", Version: "1.8.1", RelPath: "jq/1.8.1"})

	_, check, wrote, err := InitDirenv(project, "")
	if err == nil {
		t.Fatal("a mismatched environment should fail the command")
	}
	if wrote {
		t.Error("an .envrc was written despite the mismatch")
	}
	if _, statErr := os.Stat(filepath.Join(project, EnvrcName)); !os.IsNotExist(statErr) {
		t.Error("the .envrc exists on disk after a failed init")
	}
	for _, want := range []string{"Nothing was written", "node@22.1.0", "jq@1.8.1"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error should mention %q: %v", want, err)
		}
	}
	if check == nil || check.OK() {
		t.Error("the caller should get the failed check back")
	}
	// And no suggestion to allow something that was never created.
	if strings.Contains(err.Error(), "direnv allow") {
		t.Errorf("the error invites direnv allow for a file that does not exist: %v", err)
	}
}

// The same applies when the check cannot be run at all.
func TestInitDirenvWritesNothingWhenTheCheckFails(t *testing.T) {
	project := t.TempDir()
	dest := filepath.Join(project, MpackSubdir)
	unpackedTree(t, dest, Tool{Name: "jq", Version: "1.8.1", RelPath: "jq/1.8.1"})
	// A mise that cannot run.
	if err := os.WriteFile(filepath.Join(dest, filepath.FromSlash(MiseBinName)), []byte("#!/bin/sh\nexit 3\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, _, wrote, err := InitDirenv(project, "")
	if err == nil {
		t.Fatal("init should fail when the environment cannot be verified")
	}
	if wrote {
		t.Error("an .envrc was written despite the failure")
	}
	if _, statErr := os.Stat(filepath.Join(project, EnvrcName)); !os.IsNotExist(statErr) {
		t.Error("the .envrc exists on disk after a failed init")
	}
	if !strings.Contains(err.Error(), "Nothing was written") {
		t.Errorf("the user is not told the state was left alone: %v", err)
	}
}

// The happy path still writes, and reports the check as clean.
func TestInitDirenvWritesWhenTheEnvironmentMatches(t *testing.T) {
	project := t.TempDir()
	dest := filepath.Join(project, MpackSubdir)
	stubMiseTree(t, dest, `{"jq":[{"version":"1.8.1","active":true,"installed":true}]}`, 0,
		Tool{Name: "jq", Version: "1.8.1", RelPath: "jq/1.8.1"})

	plan, check, wrote, err := InitDirenv(project, "")
	if err != nil {
		t.Fatal(err)
	}
	if !wrote {
		t.Fatal("the .envrc was not created")
	}
	if !check.OK() {
		t.Fatalf("the check should be clean: %+v", check)
	}
	body, err := os.ReadFile(filepath.Join(project, EnvrcName))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), plan.Line) {
		t.Errorf(".envrc lacks the line:\n%s", body)
	}
}

// The failure that slips past a tool listing: mise reports the right tools
// but cannot produce an environment. The generated script would eval an
// empty string, lose the exit status, and carry on adding to PATH, so init
// has to exercise the environment command itself and refuse.
func TestInitDirenvRefusesWhenMiseCanListButNotBuildTheEnvironment(t *testing.T) {
	project := t.TempDir()
	dest := filepath.Join(project, MpackSubdir)
	// Exactly the archive's tools from `ls`, but `env` exits 42.
	stubMiseTree(t, dest, `{"jq":[{"version":"1.8.1","active":true,"installed":true}]}`, 42,
		Tool{Name: "jq", Version: "1.8.1", RelPath: "jq/1.8.1"})

	_, _, wrote, err := InitDirenv(project, "")
	if err == nil {
		t.Fatal("init succeeded although the environment cannot be built")
	}
	if wrote {
		t.Error("an .envrc was written")
	}
	if _, statErr := os.Stat(filepath.Join(project, EnvrcName)); !os.IsNotExist(statErr) {
		t.Error("the .envrc exists on disk")
	}
	for _, want := range []string{"cannot produce an environment", "Nothing was written"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error should mention %q: %v", want, err)
		}
	}
	// mise's own explanation is worth passing on.
	if !strings.Contains(err.Error(), "refusing to build an environment") {
		t.Errorf("mise's stderr is not reported: %v", err)
	}
}

// CheckSnapshot must run the environment command, not just the listing.
func TestCheckSnapshotExercisesTheEnvironmentCommand(t *testing.T) {
	dest := t.TempDir()
	stubMiseTree(t, dest, `{"jq":[{"version":"1.8.1","active":true,"installed":true}]}`, 7,
		Tool{Name: "jq", Version: "1.8.1", RelPath: "jq/1.8.1"})
	if _, err := CheckSnapshot(dest); err == nil {
		t.Fatal("a mise that cannot build an environment was reported as fine")
	}

	// An environment with no PATH in it is no good either.
	empty := "#!/bin/sh\nfor a in \"$@\"; do case \"$a\" in env) exit 0;; " +
		"ls) printf '%s' '{\"jq\":[{\"version\":\"1.8.1\",\"active\":true,\"installed\":true}]}'; exit 0;; esac; done\n"
	if err := os.WriteFile(filepath.Join(dest, filepath.FromSlash(MiseBinName)), []byte(empty), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := CheckSnapshot(dest); err == nil || !strings.Contains(err.Error(), "no PATH") {
		t.Fatalf("an empty environment should be rejected, got %v", err)
	}
}

// The generated scripts must not let a failed "mise env" pass for success.
// Running them for real is the only way to know: the mistake is a shell one.
//
// They fail differently on purpose. direnv.envrc runs inside direnv's own
// process and must end the whole evaluation, or direnv exports whatever was
// set before the failure and reports success. activate.sh is sourced by the
// user's interactive shell, so it returns non-zero instead of closing it.
func TestGeneratedScriptsStopWhenMiseEnvFails(t *testing.T) {
	t.Run(DirenvName, func(t *testing.T) {
		dest := t.TempDir()
		unpackedTree(t, dest, Tool{Name: "jq", Version: "1.8.1", RelPath: "jq/1.8.1"})
		writeStubMise(t, dest, `{}`, 42)
		if err := os.WriteFile(filepath.Join(dest, DirenvName), []byte(DirenvScript(dest)), 0o644); err != nil {
			t.Fatal(err)
		}
		pathAdd := filepath.Join(dest, "path_add_was_called")
		after := filepath.Join(dest, "kept_going")
		harness := fmt.Sprintf("PATH_add() { : > %s; }\nwatch_file() { :; }\ncd %s\n. ./%s\n: > %s\n",
			shellQuote(pathAdd), shellQuote(dest), DirenvName, shellQuote(after))

		cmd := exec.Command("bash", "-c", harness)
		cmd.Env = append(scrubbedEnv(), "PATH=/usr/bin:/bin")
		out, err := cmd.CombinedOutput()
		if err == nil {
			t.Fatalf("direnv.envrc reported success although mise env failed:\n%s", out)
		}
		if !strings.Contains(string(out), "could not produce the environment") {
			t.Errorf("the failure was not explained:\n%s", out)
		}
		if _, statErr := os.Stat(pathAdd); statErr == nil {
			t.Error("PATH_add ran after mise env failed")
		}
		if _, statErr := os.Stat(after); statErr == nil {
			t.Error("the evaluation continued past the failure; direnv would apply a half-built environment")
		}
	})

	t.Run(ActivateName, func(t *testing.T) {
		dest := t.TempDir()
		unpackedTree(t, dest, Tool{Name: "jq", Version: "1.8.1", RelPath: "jq/1.8.1"})
		writeStubMise(t, dest, `{}`, 42)
		if err := os.WriteFile(filepath.Join(dest, ActivateName), []byte(ActivateScript(dest)), 0o755); err != nil {
			t.Fatal(err)
		}
		// Sourced by hand: it must report failure and leave the shell alive.
		harness := fmt.Sprintf("cd %s\n. ./%s\necho \"sourced_status=$?\"\necho shell_survived\n",
			shellQuote(dest), ActivateName)
		cmd := exec.Command("bash", "-c", harness)
		cmd.Env = append(scrubbedEnv(), "PATH=/usr/bin:/bin")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("sourcing activate.sh killed the shell: %v\n%s", err, out)
		}
		got := string(out)
		if !strings.Contains(got, "could not produce the environment") {
			t.Errorf("the failure was not explained:\n%s", got)
		}
		if strings.Contains(got, "sourced_status=0") {
			t.Errorf("activate.sh returned success although mise env failed:\n%s", got)
		}
		if !strings.Contains(got, "shell_survived") {
			t.Errorf("the user's shell did not survive sourcing it:\n%s", got)
		}
	})
}

// And the same scripts must work when mise env succeeds, so the guard above
// is not simply refusing everything.
func TestGeneratedScriptsLoadWhenMiseEnvSucceeds(t *testing.T) {
	dest := t.TempDir()
	unpackedTree(t, dest, Tool{Name: "jq", Version: "1.8.1", RelPath: "jq/1.8.1"})
	writeStubMise(t, dest, `{}`, 0)
	marker := filepath.Join(dest, "path_add_was_called")
	harness := fmt.Sprintf(`
PATH_add() { : > %s; }
watch_file() { :; }
cd %s
. ./%s
case "$PATH" in /stub/bin:*) echo PATH_OK ;; *) echo "PATH not applied: $PATH" ; exit 1 ;; esac
`, shellQuote(marker), shellQuote(dest), DirenvName)
	if err := os.WriteFile(filepath.Join(dest, DirenvName), []byte(DirenvScript(dest)), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", "-c", harness)
	cmd.Env = append(scrubbedEnv(), "PATH=/usr/bin:/bin")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the script failed on a working mise: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "PATH_OK") {
		t.Errorf("the environment was not applied:\n%s", out)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("PATH_add was never reached: %v", err)
	}
}

// The two generated scripts fail differently on purpose, and the difference
// is easy to get wrong. direnv.envrc runs inside direnv's own process, so it
// exits and the whole evaluation is discarded; activate.sh is sourced by the
// user's interactive shell, where exiting would close their terminal.
func TestGeneratedScriptsUseTheRightFailureAction(t *testing.T) {
	direnv := DirenvScript("/home/bob/work/myapp/.mpack")
	if strings.Contains(direnv, "return 1") {
		t.Errorf("direnv.envrc returns on failure; it must exit so direnv discards the evaluation:\n%s", direnv)
	}
	if strings.Count(direnv, "exit 1") < 2 {
		t.Errorf("direnv.envrc should exit on both failure paths:\n%s", direnv)
	}
	activate := ActivateScript("/home/bob/work/myapp/.mpack")
	if !strings.Contains(activate, "return 1 2>/dev/null || exit 1") {
		t.Errorf("activate.sh must return when sourced, not close the user's shell:\n%s", activate)
	}
}
