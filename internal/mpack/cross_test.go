package mpack

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveTargetRejectsUnsupportedPlatforms(t *testing.T) {
	if _, _, err := ResolveTarget("linux/amd64"); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"darwin/arm64", "windows/amd64", "linux", "", "linux/riscv64"} {
		if _, _, err := ResolveTarget(bad); err == nil {
			t.Errorf("ResolveTarget(%q) should fail: mpack cannot build a toolchain for it", bad)
		}
	}
}

func TestBuildCrossPlanRunsTheInstallInsideTheContainer(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	project := filepath.Join(home, "work", "myapp")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "mise.toml"), []byte("[tools]\njq = \"1.8.1\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	global := filepath.Join(home, ".config", "mise")
	if err := os.MkdirAll(global, 0o755); err != nil {
		t.Fatal(err)
	}
	outDir := t.TempDir()

	plan, err := BuildCrossPlan(CrossOptions{
		Target:          "linux/amd64",
		ProjectDir:      project,
		GlobalConfigDir: global,
		Output:          filepath.Join(outDir, "myapp.tar.gz"),
		Exclude:         []string{"oc"},
	})
	if err != nil {
		t.Fatal(err)
	}
	// The archive comes back over stdout, so no writable mount is needed.
	if strings.Contains(strings.Join(plan.Args, " "), ":/out") {
		t.Error("the output directory must not be bind mounted")
	}
	if plan.Engine != "podman" {
		t.Errorf("engine = %q, want podman", plan.Engine)
	}
	args := strings.Join(plan.Args, " ")
	for _, want := range []string{
		"--platform linux/amd64",
		"--rm",
		filepath.Join(project, "mise.toml") + ":/host/mise-config/0:ro", // read only
		global + ":/host/global-config:ro",
		DefaultCrossImage,
	} {
		if !strings.Contains(args, want) {
			t.Errorf("container args missing %q:\n%s", want, args)
		}
	}

	// The tools must be installed in the container, not copied from the host,
	// and the container streams a payload rather than running mpack.
	for _, want := range []string{"mise install", "mise ls --current --json", "mise bin-paths", "gzip"} {
		if !strings.Contains(plan.Script, want) {
			t.Errorf("script missing %q:\n%s", want, plan.Script)
		}
	}
	// The container project path mirrors the host's home-relative path, so the
	// archive keeps a usable default destination.
	if !strings.Contains(args, "MPACK_CONTAINER_PROJECT=/root/work/myapp") {
		t.Errorf("container project path not mirrored:\n%s", args)
	}
	// Only mise's own config files are copied in, never the project itself.
	if !strings.Contains(plan.Script, "cp /host/mise-config/0 /root/work/myapp/mise.toml") {
		t.Errorf("mise.toml is not copied to the mirrored path:\n%s", plan.Script)
	}
	if strings.Contains(plan.Script, project+":") && !strings.Contains(strings.Join(plan.Args, " "), "mise.toml") {
		t.Errorf("the whole project must not be mounted:\n%v", plan.Args)
	}
}

func TestBuildCrossPlanWithoutGlobalConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	project := t.TempDir() // outside the home directory
	if err := os.WriteFile(filepath.Join(project, "mise.toml"), []byte("[tools]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	plan, err := BuildCrossPlan(CrossOptions{
		Target:          "linux/arm64",
		ProjectDir:      project,
		GlobalConfigDir: filepath.Join(home, "does-not-exist"),
		Output:          filepath.Join(t.TempDir(), "out.tar.gz"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(plan.Args, " "), "global-config") {
		t.Error("a missing global config dir must not be mounted")
	}
	// Outside the home directory the absolute path is reproduced as is.
	if !strings.Contains(strings.Join(plan.Args, " "), "MPACK_CONTAINER_PROJECT="+filepath.ToSlash(project)) {
		t.Errorf("unexpected container project path: %v", plan.Args)
	}
	if cl := plan.CommandLine(); !strings.HasPrefix(cl, "podman run --rm -i --platform linux/arm64 ") {
		t.Errorf("unexpected rendered command: %s", cl)
	}
}

// mise reads config from parent directories too, so a cross pack that copied
// only the project directory would silently install fewer tools.
func TestCrossPackCarriesAncestorConfigs(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	project := filepath.Join(home, "work", "myapp")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	// ~/work/mise.toml applies to ~/work/myapp.
	if err := os.WriteFile(filepath.Join(home, "work", "mise.toml"), []byte("[tools]\nnode = \"22.1.0\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "mise.toml"), []byte("[tools]\njq = \"1.8.1\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	plan, err := BuildCrossPlan(CrossOptions{
		Target: "linux/amd64", ProjectDir: project,
		Output: filepath.Join(t.TempDir(), "o.tar.gz"),
	})
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Join(plan.Args, " ")
	for _, want := range []string{
		filepath.Join(home, "work", "mise.toml") + ":/host/mise-config/0:ro",
		filepath.Join(project, "mise.toml") + ":/host/mise-config/1:ro",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("missing mount %q:\n%s", want, args)
		}
	}
	// Parents are copied first, so the nearer config still wins.
	parent := strings.Index(plan.Script, "/root/work/mise.toml")
	child := strings.Index(plan.Script, "/root/work/myapp/mise.toml")
	if parent < 0 || child < 0 || parent > child {
		t.Errorf("ancestor config must be copied before the project's own:\n%s", plan.Script)
	}
}

func TestCrossPackFailsWithoutAnyMiseConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	project := filepath.Join(home, "empty")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := BuildCrossPlan(CrossOptions{
		Target: "linux/amd64", ProjectDir: project,
		Output: filepath.Join(t.TempDir(), "o.tar.gz"),
	})
	if err == nil || !strings.Contains(err.Error(), "no mise configuration") {
		t.Fatalf("a project with neither local nor global config should be reported, got %v", err)
	}
}

// A project outside the home directory still inherits its parents' config:
// /work/myapp must pick up /work/mise.toml. These paths cannot be mirrored
// under the container home, so they are reproduced absolutely.
func TestCrossPackCarriesAncestorConfigsOutsideHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	// A /work/myapp-shaped layout that is not under the home directory.
	base := t.TempDir()
	work := filepath.Join(base, "work")
	project := filepath.Join(work, "myapp")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "mise.toml"), []byte("[tools]\nnode = \"22.1.0\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "mise.toml"), []byte("[tools]\njq = \"1.8.1\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	plan, err := BuildCrossPlan(CrossOptions{
		Target: "linux/amd64", ProjectDir: project,
		Output: filepath.Join(t.TempDir(), "o.tar.gz"),
	})
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Join(plan.Args, " ")
	if !strings.Contains(args, filepath.Join(work, "mise.toml")+":/host/mise-config/") {
		t.Errorf("the parent config outside the home dir is not mounted:\n%s", args)
	}
	// Both are reproduced at their absolute paths inside the container.
	for _, want := range []string{
		"cp /host/mise-config/0 " + filepath.ToSlash(filepath.Join(work, "mise.toml")),
		"cp /host/mise-config/1 " + filepath.ToSlash(filepath.Join(project, "mise.toml")),
	} {
		if !strings.Contains(plan.Script, want) {
			t.Errorf("script missing %q:\n%s", want, plan.Script)
		}
	}
	if !strings.Contains(args, "MPACK_CONTAINER_PROJECT="+filepath.ToSlash(project)) {
		t.Errorf("the project path should be mirrored absolutely:\n%s", args)
	}
}

// Plenty of projects have no config of their own and rely entirely on the
// global one; that must not be rejected.
func TestCrossPackAcceptsGlobalOnlyProject(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	project := filepath.Join(home, "work", "myapp")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	global := filepath.Join(home, ".config", "mise")
	if err := os.MkdirAll(global, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(global, "config.toml"), []byte("[tools]\njq = \"1.8.1\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	plan, err := BuildCrossPlan(CrossOptions{
		Target: "linux/amd64", ProjectDir: project,
		GlobalConfigDir: global,
		Output:          filepath.Join(t.TempDir(), "o.tar.gz"),
	})
	if err != nil {
		t.Fatalf("a global-only project must be packable: %v", err)
	}
	if !strings.Contains(plan.Script, "cp -R /host/global-config/.") {
		t.Errorf("the global config is not copied in:\n%s", plan.Script)
	}
}

// The error raised when the container engine is missing must not suggest
// --print-command as a way to pack anyway: that flag prints the
// tool-installation step, which streams a payload for inspection and cannot
// produce an archive on its own.
func TestMissingEngineErrorDoesNotPromisePrintCommandPacks(t *testing.T) {
	plan := &CrossPlan{Engine: "definitely-not-installed-engine", Output: filepath.Join(t.TempDir(), "o.tar.gz")}
	err := RunCross(plan, filepath.Join(t.TempDir(), "payload.tar.gz"), nil)
	if err == nil {
		t.Fatal("a missing engine should be reported")
	}
	msg := err.Error()
	if !strings.Contains(msg, plan.Engine) {
		t.Errorf("the error should name the engine: %v", err)
	}
	for _, misleading := range []string{
		"run the container yourself",
		"--print-command to run",
	} {
		if strings.Contains(msg, misleading) {
			t.Errorf("the error implies --print-command can pack (%q): %v", misleading, err)
		}
	}
	// If it mentions the flag at all, it has to say what the flag really does.
	if strings.Contains(msg, "--print-command") && !strings.Contains(msg, "payload") {
		t.Errorf("--print-command is mentioned without saying it only yields a payload: %v", err)
	}
}
