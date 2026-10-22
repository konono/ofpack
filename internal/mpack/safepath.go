package mpack

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// RootSentinel is the placeholder stored in an archived symlink target when the
// original target was an absolute path inside the source installs root. On
// unpack it is replaced by the destination installs root so the link keeps
// working after relocation.
const RootSentinel = "@mpack-root@"

// CheckArchiveName validates a path as it appears inside an mpack archive.
// Archive paths are always slash separated, relative and free of "." / ".."
// components. Anything else is rejected before it can reach the filesystem.
func CheckArchiveName(name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("empty archive path")
	}
	if strings.ContainsRune(name, 0) {
		return "", fmt.Errorf("archive path %q contains a NUL byte", name)
	}
	// A backslash is a separator on some platforms; refuse it everywhere so a
	// single archive cannot mean two different things on two machines.
	if strings.ContainsRune(name, '\\') {
		return "", fmt.Errorf("archive path %q contains a backslash", name)
	}
	if strings.HasPrefix(name, "/") {
		return "", fmt.Errorf("archive path %q is absolute", name)
	}
	if filepath.VolumeName(name) != "" {
		return "", fmt.Errorf("archive path %q has a volume name", name)
	}
	clean := path.Clean(name)
	if clean != strings.TrimSuffix(name, "/") {
		return "", fmt.Errorf("archive path %q is not in canonical form (want %q)", name, clean)
	}
	for _, part := range strings.Split(clean, "/") {
		switch part {
		case "", ".", "..":
			return "", fmt.Errorf("archive path %q contains a %q component", name, part)
		}
	}
	return clean, nil
}

// ResolveUnder joins a validated archive path onto root and double checks that
// the result cannot escape root. root must be absolute and already cleaned.
func ResolveUnder(root, archiveName string) (string, error) {
	clean, err := CheckArchiveName(archiveName)
	if err != nil {
		return "", err
	}
	full := filepath.Join(root, filepath.FromSlash(clean))
	if !isUnder(root, full) {
		return "", fmt.Errorf("archive path %q escapes the destination", archiveName)
	}
	return full, nil
}

// isUnder reports whether p is root itself or lives somewhere below it. Both
// arguments must already be cleaned absolute paths.
func isUnder(root, p string) bool {
	if p == root {
		return true
	}
	sep := string(filepath.Separator)
	if !strings.HasSuffix(root, sep) {
		root += sep
	}
	return strings.HasPrefix(p, root)
}

// RelUnder returns the slash separated path of p relative to root, or ok=false
// if p is not inside root. Both arguments must be cleaned absolute paths.
func RelUnder(root, p string) (string, bool) {
	if !isUnder(root, p) {
		return "", false
	}
	if p == root {
		return ".", true
	}
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return "", false
	}
	return filepath.ToSlash(rel), true
}

// MkdirAllNoSymlink creates relDir underneath root, one component at a time,
// refusing to walk through a component that already exists as a symlink. This
// is what stops an archive from first unpacking a symlink and then writing
// "through" it to somewhere outside the destination.
func MkdirAllNoSymlink(root, relDir string, perm os.FileMode) error {
	if relDir == "" || relDir == "." {
		return os.MkdirAll(root, perm)
	}
	if _, err := CheckArchiveName(relDir); err != nil {
		return err
	}
	if err := os.MkdirAll(root, perm); err != nil {
		return err
	}
	cur := root
	for _, part := range strings.Split(relDir, "/") {
		cur = filepath.Join(cur, part)
		fi, err := os.Lstat(cur)
		switch {
		case err == nil && fi.IsDir():
			continue
		case err == nil && fi.Mode()&os.ModeSymlink != 0:
			return fmt.Errorf("refusing to descend through symlink %s", cur)
		case err == nil:
			return fmt.Errorf("refusing to replace non-directory %s", cur)
		case os.IsNotExist(err):
			if err := os.Mkdir(cur, perm); err != nil {
				return err
			}
		default:
			return err
		}
	}
	return nil
}
