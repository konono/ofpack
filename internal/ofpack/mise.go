package ofpack

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// miseToolEntry mirrors the subset of `mise ls --current --json` we rely on.
type miseToolEntry struct {
	Version          string `json:"version"`
	RequestedVersion string `json:"requested_version"`
	InstallPath      string `json:"install_path"`
	Installed        bool   `json:"installed"`
	Active           bool   `json:"active"`
	Source           struct {
		Path string `json:"path"`
	} `json:"source"`
}

// Env describes the mise installation we are packing from.
type Env struct {
	// Bin is the real mise executable with all symlinks resolved. Homebrew
	// installs mise as a symlink into the Cellar, and we must archive the
	// actual binary, not a link that will dangle on the target machine.
	Bin string
	// Version is the first token of `mise --version`.
	Version string
	// Vars are per-command environment overrides used to keep a pack rooted
	// at its selected directory without changing the process environment.
	Vars []string
}

// LookupEnv locates the mise executable to pack. OFPACK_MISE_BIN overrides the
// PATH lookup.
func LookupEnv() (Env, error) {
	bin := os.Getenv("OFPACK_MISE_BIN")
	if bin == "" {
		found, err := exec.LookPath("mise")
		if err != nil {
			return Env{}, fmt.Errorf("mise not found on PATH (set OFPACK_MISE_BIN to override): %w", err)
		}
		bin = found
	}
	real, err := filepath.EvalSymlinks(bin)
	if err != nil {
		return Env{}, fmt.Errorf("resolving mise binary %s: %w", bin, err)
	}
	fi, err := os.Stat(real)
	if err != nil {
		return Env{}, err
	}
	if fi.IsDir() || fi.Mode()&0o111 == 0 {
		return Env{}, fmt.Errorf("%s is not an executable file", real)
	}
	out, err := runMise(real, "", "--version")
	if err != nil {
		return Env{}, err
	}
	version := "unknown"
	if f := strings.Fields(string(out)); len(f) > 0 {
		version = f[0]
	}
	return Env{Bin: real, Version: version}, nil
}

func runMise(bin, dir string, args ...string) ([]byte, error) {
	return runMiseWithVars(bin, dir, nil, args...)
}

func runMiseWithVars(bin, dir string, vars []string, args ...string) ([]byte, error) {
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	if len(vars) > 0 {
		keys := map[string]bool{}
		for _, v := range vars {
			key, _, _ := strings.Cut(v, "=")
			keys[key] = true
		}
		for _, v := range os.Environ() {
			key, _, _ := strings.Cut(v, "=")
			if !keys[key] {
				cmd.Env = append(cmd.Env, v)
			}
		}
		cmd.Env = append(cmd.Env, vars...)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("mise %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// CurrentTools lists the tools that are both installed and active in workDir,
// i.e. the merged result of the global and any local mise configuration. It
// also returns the installs root shared by every tool.
func CurrentTools(env Env, workDir string) (tools []Tool, installsRoot string, err error) {
	return currentTools(env, workDir, false)
}

// ScopeTools allows an empty root scope when only a descendant declares tools.
func ScopeTools(env Env, workDir string) (tools []Tool, installsRoot string, err error) {
	return currentTools(env, workDir, true)
}

func currentTools(env Env, workDir string, allowEmpty bool) (tools []Tool, installsRoot string, err error) {
	out, err := runMiseWithVars(env.Bin, workDir, env.Vars, "ls", "--current", "--json")
	if err != nil {
		return nil, "", err
	}
	var raw map[string][]miseToolEntry
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, "", fmt.Errorf("parsing mise ls output: %w", err)
	}

	for name, entries := range raw {
		for _, e := range entries {
			if !e.Installed || !e.Active {
				continue
			}
			if e.InstallPath == "" {
				return nil, "", fmt.Errorf("tool %s has no install_path", name)
			}
			abs, err := filepath.Abs(e.InstallPath)
			if err != nil {
				return nil, "", err
			}
			root, rel, err := splitInstallsRoot(abs)
			if err != nil {
				return nil, "", fmt.Errorf("tool %s: %w", name, err)
			}
			if installsRoot == "" {
				installsRoot = root
			} else if installsRoot != root {
				return nil, "", fmt.Errorf("tools span multiple installs roots (%s and %s); ofpack packs a single mise data dir", installsRoot, root)
			}
			tools = append(tools, Tool{
				Name:             name,
				Version:          e.Version,
				RequestedVersion: e.RequestedVersion,
				RelPath:          rel,
				ConfigSource:     scrubHome(e.Source.Path),
			})
		}
	}
	if len(tools) == 0 {
		if allowEmpty {
			return nil, "", nil
		}
		return nil, "", fmt.Errorf("no installed and active mise tools in %s", workDir)
	}
	// `mise ls --current --json` returns versions sorted, not in the order
	// the config asked for, but for a tool pinned to several versions the
	// config order decides which one wins on PATH. Recover that order from
	// the PATH mise would export, so the target resolves the same version as
	// the source.
	ranks, rankErr := precedenceRanks(env, workDir, installsRoot)
	sortToolsByPrecedence(tools, ranks)
	if err := verifyPrecedenceKnown(tools, ranks, rankErr); err != nil {
		return nil, "", err
	}
	return tools, installsRoot, nil
}

// verifyPrecedenceKnown refuses to pack a tool that is active at several
// versions when the order between them could not be determined. Falling back
// to version order would silently hand the target a different default version
// than the source has. A tool with a single active version needs no order, so
// it is unaffected.
func verifyPrecedenceKnown(tools []Tool, ranks map[string]int, rankErr error) error {
	counts := map[string]int{}
	for _, t := range tools {
		counts[t.Name]++
	}
	var problems []string
	reported := map[string]bool{}
	for _, t := range tools {
		if counts[t.Name] < 2 || reported[t.Name] {
			continue
		}
		known := true
		versions := make([]string, 0, counts[t.Name])
		for _, other := range tools {
			if other.Name != t.Name {
				continue
			}
			versions = append(versions, other.Version)
			if _, ok := ranks[other.RelPath]; !ok {
				known = false
			}
		}
		if !known {
			reported[t.Name] = true
			problems = append(problems, fmt.Sprintf("  %s (%s)", t.Name, strings.Join(versions, ", ")))
		}
	}
	if len(problems) == 0 {
		return nil
	}
	reason := "they do not all appear in the PATH mise exports"
	if rankErr != nil {
		reason = fmt.Sprintf("`mise env --json` failed: %v", rankErr)
	}
	return fmt.Errorf(`cannot determine which version takes precedence for:
%s
%s

These tools are active at more than one version, and the order they are
written in decides which one the target resolves. Packing them in the wrong
order would silently give the target a different default version, so ofpack
stops instead. Fix the mise environment, or drop the tool with --exclude.`,
		strings.Join(problems, "\n"), reason)
}

// sortToolsByPrecedence groups tools by name and, within a name, orders the
// versions the way PATH does. Without rank information it falls back to
// version order so the result stays deterministic.
func sortToolsByPrecedence(tools []Tool, rank map[string]int) {
	sort.SliceStable(tools, func(i, j int) bool {
		if tools[i].Name != tools[j].Name {
			return tools[i].Name < tools[j].Name
		}
		ri, iok := rank[tools[i].RelPath]
		rj, jok := rank[tools[j].RelPath]
		switch {
		case iok && jok:
			return ri < rj
		case iok != jok:
			return iok // a tool on PATH comes before one that is not
		default:
			return tools[i].Version < tools[j].Version
		}
	})
}

// precedenceRanks maps a tool's install-relative path to its position in the
// PATH mise exports. Earlier means higher precedence. An entry is missing when
// the tool contributes nothing to PATH; for a tool with several active
// versions that is fatal, see verifyPrecedenceKnown.
func precedenceRanks(env Env, workDir, installsRoot string) (map[string]int, error) {
	// `mise bin-paths` lists the tool bin directories in precedence order and
	//, unlike `mise env`, carries no environment values at all, so no secret
	// is ever read. Older mise versions lack it; fall back to the PATH mise
	// would export.
	if out, err := runMiseWithVars(env.Bin, workDir, env.Vars, "bin-paths"); err == nil {
		return RanksFromBinPaths(strings.Split(string(out), "\n"), installsRoot), nil
	}
	vars, err := miseEnv(env, workDir)
	if err != nil {
		return nil, err
	}
	return ranksFromPath(vars["PATH"], installsRoot), nil
}

// ranksFromPath is the pure part of precedenceRanks.
func ranksFromPath(pathValue, installsRoot string) map[string]int {
	return RanksFromBinPaths(filepath.SplitList(pathValue), installsRoot)
}

// RanksFromBinPaths turns an ordered list of bin directories into a map from
// a tool's install-relative path to its precedence, lowest first.
func RanksFromBinPaths(entries []string, installsRoot string) map[string]int {
	ranks := map[string]int{}
	for _, entry := range entries {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		rel, ok := RelUnder(filepath.Clean(installsRoot), filepath.Clean(entry))
		if !ok {
			continue
		}
		// An entry is usually <tool>/<version>/bin; the tool directory is its
		// first two components.
		parts := strings.Split(rel, "/")
		if len(parts) < 2 {
			continue
		}
		key := parts[0] + "/" + parts[1]
		if _, seen := ranks[key]; !seen {
			ranks[key] = len(ranks)
		}
	}
	return ranks
}

// miseEnv returns the environment mise would export in workDir.
func miseEnv(env Env, workDir string) (map[string]string, error) {
	out, err := runMiseWithVars(env.Bin, workDir, env.Vars, "env", "--json")
	if err != nil {
		// `mise env` can fail for reasons unrelated to packing (no shell
		// integration, a broken hook); that is not worth aborting a pack.
		return nil, err
	}
	var m map[string]string
	if err := json.Unmarshal(out, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// splitInstallsRoot splits an absolute install path such as
// /home/u/.local/share/mise/installs/node/22.1.0 into the installs root and
// the tool-relative remainder ("node/22.1.0").
func splitInstallsRoot(abs string) (root, rel string, err error) {
	slash := filepath.ToSlash(filepath.Clean(abs))
	marker := "/" + InstallsPrefix + "/"
	idx := strings.LastIndex(slash, marker)
	if idx < 0 {
		return "", "", fmt.Errorf("install path %s is not under an %q directory", abs, InstallsPrefix)
	}
	root = filepath.FromSlash(slash[:idx+len(marker)-1])
	rel = slash[idx+len(marker):]
	if rel == "" {
		return "", "", fmt.Errorf("install path %s has an empty tool component", abs)
	}
	if _, err := CheckArchiveName(rel); err != nil {
		return "", "", err
	}
	return root, rel, nil
}

// EnvVarNames returns the names of the environment variables the source mise
// configuration exports, excluding PATH. Values are never read into the
// manifest because mise config env entries can hold secrets.
func EnvVarNames(env Env, workDir string) ([]string, error) {
	m, err := miseEnv(env, workDir)
	if err != nil {
		return nil, nil
	}
	names := make([]string, 0, len(m))
	for k := range m {
		if k == "PATH" {
			continue
		}
		names = append(names, k)
	}
	sort.Strings(names)
	return names, nil
}

// RenderConfig builds the minimal global mise config for the destination: every
// packed tool pinned to its exact version, so the merged global+local state of
// the source machine is reproduced without any version resolution (which would
// need the network).
func RenderConfig(tools []Tool) []byte {
	var b strings.Builder
	b.WriteString("# Generated by ofpack. Do not edit.\n")
	b.WriteString("# Exact versions of the tools that were active when the archive was packed.\n\n")
	b.WriteString("[tools]\n")
	// A tool can legitimately be active at several versions at once
	// (node = ["22.22.2", "24.16.0"]). Emitting one key per version would be a
	// duplicate TOML key, which mise rejects with a parse error, so multiple
	// versions are written as an array.
	seen := map[string]bool{}
	for _, t := range tools {
		if seen[t.Name] {
			continue
		}
		seen[t.Name] = true
		versions := make([]string, 0, 1)
		for _, other := range tools {
			if other.Name == t.Name {
				versions = append(versions, other.Version)
			}
		}
		if len(versions) == 1 {
			fmt.Fprintf(&b, "%s = %s\n", tomlQuote(t.Name), tomlQuote(versions[0]))
			continue
		}
		quoted := make([]string, len(versions))
		for i, v := range versions {
			quoted[i] = tomlQuote(v)
		}
		fmt.Fprintf(&b, "%s = [%s]\n", tomlQuote(t.Name), strings.Join(quoted, ", "))
	}
	return []byte(b.String())
}

// scrubHome replaces the user's home directory prefix with "~" so diagnostic
// paths can be kept in the manifest without recording the account name. Paths
// outside the home directory are dropped entirely.
func scrubHome(p string) string {
	if p == "" {
		return ""
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	home, err = filepath.Abs(home)
	if err != nil {
		return ""
	}
	abs := p
	if !filepath.IsAbs(abs) {
		return filepath.ToSlash(abs)
	}
	rel, ok := RelUnder(filepath.Clean(home), filepath.Clean(abs))
	if !ok {
		return ""
	}
	if rel == "." {
		return "~"
	}
	return "~/" + rel
}

func tomlQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\t':
			b.WriteString(`\t`)
		case '\r':
			b.WriteString(`\r`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
