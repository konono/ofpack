package ofpack

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// DiscoverMiseScopes returns the selected root and each descendant directory
// containing a mise config. It never walks upward or follows directory links.
func DiscoverMiseScopes(root string) ([]string, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	scopes := []string{root}
	global := canonical(DefaultGlobalConfigDir())
	err = filepath.WalkDir(root, func(dir string, ent fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !ent.IsDir() {
			return nil
		}
		if dir != root {
			switch ent.Name() {
			case ".git", ".venv", ".ofpack", "node_modules":
				return filepath.SkipDir
			}
			if canonical(dir) == global {
				return filepath.SkipDir
			}
			if hasMiseConfig(dir) {
				scopes = append(scopes, dir)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(scopes[1:])
	return scopes, nil
}

func hasMiseConfig(dir string) bool {
	for _, name := range miseConfigFiles {
		fi, err := os.Lstat(filepath.Join(dir, filepath.FromSlash(name)))
		if err == nil && fi.Mode().IsRegular() {
			return true
		}
	}
	return false
}

func canonical(p string) string {
	if p == "" {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return filepath.Clean(resolved)
	}
	return filepath.Clean(p)
}

// ScopedMiseEnv excludes configurations outside the chosen tree while
// retaining the user's global config. mise itself still resolves precedence
// between the allowed files, including each child's overrides.
func ScopedMiseEnv(env Env, root string) (Env, error) {
	root = canonical(root)
	out, err := runMiseWithVars(env.Bin, root, env.Vars, "config", "ls", "--json")
	if err != nil {
		return Env{}, fmt.Errorf("listing mise configs for %s: %w", root, err)
	}
	var loaded []struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(out, &loaded); err != nil {
		return Env{}, fmt.Errorf("parsing mise config list: %w", err)
	}
	global := canonical(DefaultGlobalConfigDir())
	globalFile := canonical(os.Getenv("MISE_GLOBAL_CONFIG_FILE"))
	ignored := filepath.SplitList(os.Getenv("MISE_IGNORED_CONFIG_PATHS"))
	for _, c := range loaded {
		p := canonical(c.Path)
		if p == "" {
			continue
		}
		if _, ok := RelUnder(root, p); ok {
			continue
		}
		if global != "" {
			if _, ok := RelUnder(global, p); ok {
				continue
			}
		}
		if p == globalFile {
			continue
		}
		ignored = append(ignored, p)
	}
	if len(ignored) > 0 {
		env.Vars = append(env.Vars, "MISE_IGNORED_CONFIG_PATHS="+strings.Join(ignored, string(os.PathListSeparator)))
	}
	return env, nil
}

// AnyScopeHasTools reports whether the selected root or any descendant mise
// scope activates a tool.
//
// Asking only about the root would miss a project whose tools live in a
// child directory: it would look toolless and be packed as though it had no
// toolchain at all, silently dropping the child's tools.
func AnyScopeHasTools(root string) (bool, error) {
	dirs, err := DiscoverMiseScopes(root)
	if err != nil {
		return false, err
	}
	env, lookupErr := LookupEnv()
	if lookupErr != nil {
		// No mise to ask. That only means "no tools" when the tree does not
		// ask for any either; a mise config with no mise to read it is a
		// problem to report, not a scope to drop quietly.
		if len(dirs) > 1 || hasMiseConfig(root) {
			return false, fmt.Errorf("%s has mise configuration but mise is not available: %w", root, lookupErr)
		}
		return false, nil
	}
	env, err = ScopedMiseEnv(env, root)
	if err != nil {
		return false, err
	}
	for _, dir := range dirs {
		tools, _, err := ScopeTools(env, dir)
		if err != nil {
			return false, fmt.Errorf("mise scope %s: %w", dir, err)
		}
		if len(tools) > 0 {
			return true, nil
		}
	}
	return false, nil
}
