package ofpack

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// EnvrcName is the file direnv looks for in a project directory.
const EnvrcName = ".envrc"

// DirenvPlan is what `ofpack direnv init` worked out: which unpacked tree to
// use, and the one line a project's .envrc needs.
type DirenvPlan struct {
	// Project is the directory the .envrc belongs to.
	Project string
	// Dest is the unpacked ofpack tree.
	Dest string
	// Line is the direnv directive to put in .envrc.
	Line string
	// EnvrcPath is where the .envrc is, or would be.
	EnvrcPath string
	// EnvrcExists reports whether the project already has one. ofpack never
	// edits an existing .envrc; the caller is shown Line to add by hand.
	EnvrcExists bool
	// DirenvInstalled reports whether direnv is on PATH. It is an optional
	// prerequisite, not something ofpack ships.
	DirenvInstalled bool
	// ScopePath is the selected project-relative mise scope ("." for root).
	ScopePath string
}

// PlanDirenv resolves the destination and the .envrc line for project. dest
// may be empty, in which case the conventional location inside the project is
// used.
func PlanDirenv(project, dest string) (*DirenvPlan, error) {
	return PlanDirenvWithRoot(project, dest, "")
}

// PlanDirenvWithRoot permits an explicit project root when the toolchain was
// unpacked to a custom location rather than the project's .ofpack directory.
func PlanDirenvWithRoot(project, dest, explicitRoot string) (*DirenvPlan, error) {
	project, err := filepath.Abs(project)
	if err != nil {
		return nil, err
	}
	if fi, err := os.Stat(project); err != nil || !fi.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", project)
	}

	if dest == "" {
		// Child projects share the archive unpacked at the selected root.
		for dir := project; ; dir = filepath.Dir(dir) {
			candidate := filepath.Join(dir, OfpackSubdir)
			if fi, err := os.Stat(candidate); err == nil && fi.IsDir() {
				dest = candidate
				break
			}
			if filepath.Dir(dir) == dir {
				return nil, fmt.Errorf("no unpacked toolchain at %s or its project ancestors: unpack an archive first, or point at one with --dest DIR", filepath.Join(project, OfpackSubdir))
			}
		}
	}
	dest, err = filepath.Abs(dest)
	if err != nil {
		return nil, err
	}
	if err := checkUnpackedTree(dest); err != nil {
		return nil, err
	}
	m, err := readScopedInstalledManifest(dest)
	if err != nil {
		return nil, err
	}
	scopePath := "."
	projectRoot := ""
	if m.Schema == SchemaVersionScoped {
		if explicitRoot != "" {
			projectRoot, err = filepath.Abs(explicitRoot)
			if err != nil {
				return nil, err
			}
		} else if filepath.Base(dest) == OfpackSubdir {
			projectRoot = filepath.Dir(dest)
		} else if m.HomeRelativeProjectPath != "" {
			if home, err := os.UserHomeDir(); err == nil {
				projectRoot = filepath.Join(home, filepath.FromSlash(m.HomeRelativeProjectPath))
			}
		}
		if projectRoot == "" {
			return nil, fmt.Errorf("cannot identify the project root for scoped toolchain %s; pass --project-root DIR", dest)
		}
		rel, ok := RelUnder(projectRoot, project)
		if !ok {
			return nil, fmt.Errorf("project %s is outside the scoped archive root %s", project, projectRoot)
		}
		scopePath = selectedScopePath(m.Scopes, filepath.ToSlash(rel))
	}

	// A destination inside the project is referred to relatively, so the
	// .envrc keeps working if the project is moved or cloned elsewhere.
	sourceName := DirenvName
	if scopePath != "." {
		sourceName = scopeDirenvName(scopePath)
	}
	target := filepath.Join(dest, filepath.FromSlash(sourceName))
	ref := target
	if rel, ok := RelUnder(project, dest); ok && rel != "." {
		ref = filepath.ToSlash(filepath.Join(rel, filepath.FromSlash(sourceName)))
	} else if rel, ok := RelUnder(project, target); ok {
		ref = filepath.ToSlash(rel)
	} else if m.Schema == SchemaVersionScoped && filepath.Base(dest) == OfpackSubdir && projectRoot == filepath.Dir(dest) {
		if rel, err := filepath.Rel(project, target); err == nil {
			ref = filepath.ToSlash(rel)
		}
	}

	envrc := filepath.Join(project, EnvrcName)
	_, statErr := os.Lstat(envrc)
	_, lookErr := exec.LookPath("direnv")

	return &DirenvPlan{
		Project:         project,
		Dest:            dest,
		Line:            "source_env " + shellQuote(ref),
		EnvrcPath:       envrc,
		EnvrcExists:     statErr == nil,
		DirenvInstalled: lookErr == nil,
		ScopePath:       scopePath,
	}, nil
}

func readScopedInstalledManifest(dest string) (*Manifest, error) {
	data, err := os.ReadFile(filepath.Join(dest, InstalledManifestName))
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parsing installed manifest: %w", err)
	}
	if err := checkSchema(m.Schema); err != nil {
		return nil, err
	}
	if err := validateScopedManifest(&m); err != nil {
		return nil, err
	}
	return &m, nil
}

func selectedScopePath(scopes []Scope, rel string) string {
	selected := "."
	for _, scope := range scopes {
		if scope.Path != "." && (rel == scope.Path || strings.HasPrefix(rel, scope.Path+"/")) && len(scope.Path) > len(selected) {
			selected = scope.Path
		}
	}
	return selected
}

// checkUnpackedTree makes sure dest really is an unpacked archive, so that a
// mistyped --dest produces a clear error rather than an .envrc pointing at
// nothing.
func checkUnpackedTree(dest string) error {
	if fi, err := os.Stat(dest); err != nil || !fi.IsDir() {
		return fmt.Errorf("%s is not a directory", dest)
	}
	for _, rel := range []string{DirenvName, InstalledManifestName, MiseBinName} {
		if _, err := os.Stat(filepath.Join(dest, filepath.FromSlash(rel))); err != nil {
			return fmt.Errorf("%s does not look like an unpacked ofpack directory: %s is missing", dest, rel)
		}
	}
	return nil
}

// WriteEnvrc creates the project's .envrc with just the ofpack line. It
// refuses to touch an existing file: a project's .envrc is the user's, and
// appending to it silently is not ofpack's call.
func (p *DirenvPlan) WriteEnvrc() error {
	if p.EnvrcExists {
		return fmt.Errorf("%s already exists", p.EnvrcPath)
	}
	body := fmt.Sprintf("# Added by ofpack: load the toolchain unpacked in %s.\n%s\n",
		shellQuote(p.Dest), p.Line)
	f, err := os.OpenFile(p.EnvrcPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(body); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// Instructions is what to tell the user once the plan is known.
func (p *DirenvPlan) Instructions(wrote bool) string {
	var b strings.Builder
	if wrote {
		fmt.Fprintf(&b, "Created %s:\n\n    %s\n\n", p.EnvrcPath, p.Line)
	} else {
		fmt.Fprintf(&b, "%s already exists and was left alone.\nAdd this line to it:\n\n    %s\n\n", p.EnvrcPath, p.Line)
	}
	if p.DirenvInstalled {
		fmt.Fprintf(&b, "Then allow it:\n\n    direnv allow %s\n", shellQuote(p.Project))
		b.WriteString(`
The tools are on PATH inside this directory and gone again when you leave it.
Do not also run "mise activate" in the same shell: direnv and the mise hook
would both be managing the same variables.
`)
		return b.String()
	}
	b.WriteString(`direnv is not installed. It is an optional prerequisite - ofpack does not
bundle it - and the archive works without it:

    source ` + shellQuote(filepath.Join(p.Dest, ActivateName)) + `

To get the automatic switching instead, install direnv, add its hook to your
shell (for example: eval "$(direnv hook zsh)" in ~/.zshrc), then run:

    direnv allow ` + shellQuote(p.Project) + `
`)
	return b.String()
}

// InitDirenv is the whole of `ofpack direnv init`: work out the destination,
// make sure the environment it produces really is the archive's, and only
// then write the project's .envrc.
//
// The order matters. Writing the file first and checking afterwards would
// leave the user with an .envrc, an invitation to run `direnv allow`, and a
// toolchain that is not the one they packed.
func InitDirenv(project, dest string) (plan *DirenvPlan, check *SnapshotCheck, wrote bool, err error) {
	return InitDirenvWithRoot(project, dest, "")
}

// InitDirenvWithRoot is InitDirenv with an explicit root for custom --dest.
func InitDirenvWithRoot(project, dest, projectRoot string) (plan *DirenvPlan, check *SnapshotCheck, wrote bool, err error) {
	plan, err = PlanDirenvWithRoot(project, dest, projectRoot)
	if err != nil {
		return nil, nil, false, err
	}
	check, err = CheckSnapshotScope(plan.Dest, plan.ScopePath)
	if err != nil {
		return plan, nil, false, fmt.Errorf("could not verify the environment %s would produce: %w\n"+
			"Nothing was written. Fix the unpacked tree, or source %s by hand",
			plan.Dest, err, filepath.Join(plan.Dest, ActivateName))
	}
	if !check.OK() {
		return plan, check, false, fmt.Errorf("%sNothing was written, because this would not give you the archive's toolchain.\n"+
			"Unpack the archive again, or source %s by hand to see what you get",
			check.Report(), filepath.Join(plan.Dest, ActivateName))
	}
	if !plan.EnvrcExists {
		if err := plan.WriteEnvrc(); err != nil {
			return plan, check, false, err
		}
		wrote = true
	}
	return plan, check, wrote, nil
}

// SnapshotCheck is the result of comparing what the generated environment
// actually resolves against what the archive recorded.
type SnapshotCheck struct {
	// Extra are tools present in the environment but not in the manifest:
	// something outside the snapshot is being merged in.
	Extra []string
	// Missing are tools in the manifest that the environment does not
	// resolve.
	Missing []string
	// SystemConfigs are config files ofpack cannot shut out, listed when they
	// exist so a mismatch has somewhere obvious to start.
	SystemConfigs []string
}

// OK reports whether the environment matches the archive exactly.
func (c SnapshotCheck) OK() bool { return len(c.Extra) == 0 && len(c.Missing) == 0 }

// systemConfigPaths are read by mise regardless of MISE_CONFIG_DIR and of the
// directory it is run from, so they are the one leak the generated scripts
// cannot close. They are reported rather than silently tolerated.
var systemConfigPaths = []string{"/etc/mise/config.toml", "/etc/mise/mise.toml", "/mise.toml", "/.mise.toml"}

// CheckSnapshot runs the bundled mise the way the generated scripts do and
// compares the tools it resolves with the manifest. Isolating a snapshot from
// every config mise might read is not something that can be asserted in the
// abstract, so it is measured here instead.
func CheckSnapshot(dest string) (*SnapshotCheck, error) {
	return CheckSnapshotScope(dest, ".")
}

// CheckSnapshotScope verifies the environment selected for one directory.
func CheckSnapshotScope(dest, scopePath string) (*SnapshotCheck, error) {
	m, err := readScopedInstalledManifest(dest)
	if err != nil {
		return nil, err
	}
	wantTools := m.Tools
	configDir := filepath.Join(dest, "config")
	if m.Schema == SchemaVersionScoped {
		found := false
		for _, scope := range m.Scopes {
			if scope.Path == scopePath {
				wantTools = scope.Tools
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("scope %q is absent from the archive", scopePath)
		}
		if scopePath != "." {
			configDir = filepath.Join(dest, filepath.FromSlash(filepath.Dir(scopeConfigName(scopePath))))
		}
	}

	run := func(args ...string) ([]byte, error) {
		cmd := exec.Command(filepath.Join(dest, filepath.FromSlash(MiseBinName)), args...)
		cmd.Dir = dest
		// Exactly the environment the generated scripts build: nothing MISE_*
		// is inherited, and the global config file is pinned.
		cmd.Env = append(scrubbedEnv(),
			"MISE_DATA_DIR="+dest,
			"MISE_CONFIG_DIR="+configDir,
			"MISE_GLOBAL_CONFIG_FILE="+filepath.Join(configDir, "config.toml"),
			"MISE_OFFLINE=1",
		)
		return cmd.Output()
	}

	// The command the generated scripts actually depend on. Checking only
	// `ls` would miss a mise that lists tools happily but cannot produce an
	// environment, which is the failure that matters here.
	envOut, err := run("-C", "/", "env", "-s", "bash")
	if err != nil {
		return nil, fmt.Errorf("the bundled mise cannot produce an environment (%s): %w",
			strings.Join([]string{"mise", "-C", "/", "env", "-s", "bash"}, " "), withStderr(err))
	}
	if !bytes.Contains(envOut, []byte("export PATH=")) {
		return nil, fmt.Errorf("the bundled mise produced an environment with no PATH in it")
	}

	out, err := run("-C", "/", "ls", "--current", "--json")
	if err != nil {
		return nil, fmt.Errorf("running the bundled mise: %w", withStderr(err))
	}
	var listed map[string][]miseToolEntry
	if err := json.Unmarshal(out, &listed); err != nil {
		return nil, fmt.Errorf("parsing mise output: %w", err)
	}

	want := map[string]bool{}
	for _, t := range wantTools {
		want[t.Name+"@"+t.Version] = true
	}
	got := map[string]bool{}
	for name, entries := range listed {
		for _, e := range entries {
			// Active alone is not enough: mise lists a tool the config asks
			// for even when its files are not there, and a tool that is not
			// installed is not usable.
			if e.Active && e.Installed {
				got[name+"@"+e.Version] = true
			}
		}
	}
	c := &SnapshotCheck{}
	for k := range got {
		if !want[k] {
			c.Extra = append(c.Extra, k)
		}
	}
	for k := range want {
		if !got[k] {
			c.Missing = append(c.Missing, k)
		}
	}
	sort.Strings(c.Extra)
	sort.Strings(c.Missing)
	for _, p := range systemConfigPaths {
		if _, err := os.Stat(p); err == nil {
			c.SystemConfigs = append(c.SystemConfigs, p)
		}
	}
	return c, nil
}

// Report renders a check for the user, or "" when there is nothing to say.
func (c SnapshotCheck) Report() string {
	if c.OK() {
		if len(c.SystemConfigs) == 0 {
			return ""
		}
		return fmt.Sprintf("note: %s exist(s) and mise reads them whatever ofpack sets; the environment still matches the archive\n",
			strings.Join(c.SystemConfigs, ", "))
	}
	var b strings.Builder
	b.WriteString("warning: the environment this produces does not match the archive\n")
	if len(c.Extra) > 0 {
		fmt.Fprintf(&b, "  unexpected: %s\n", strings.Join(c.Extra, ", "))
	}
	if len(c.Missing) > 0 {
		fmt.Fprintf(&b, "  missing:    %s\n", strings.Join(c.Missing, ", "))
	}
	if len(c.SystemConfigs) > 0 {
		fmt.Fprintf(&b, "  likely cause: %s, which mise reads whatever ofpack sets\n", strings.Join(c.SystemConfigs, ", "))
	}
	return b.String()
}

// withStderr folds a failed command's stderr into the error, since that is
// where mise explains itself.
func withStderr(err error) error {
	var ee *exec.ExitError
	if errors.As(err, &ee) && len(ee.Stderr) > 0 {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(ee.Stderr)))
	}
	return err
}

// scrubbedEnv is the current environment with every MISE_* removed, matching
// what the generated scripts do before setting their own.
func scrubbedEnv() []string {
	var out []string
	for _, kv := range os.Environ() {
		if name, _, ok := strings.Cut(kv, "="); ok && strings.HasPrefix(name, "MISE_") {
			continue
		}
		out = append(out, kv)
	}
	return out
}
