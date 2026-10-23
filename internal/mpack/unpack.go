package mpack

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// OverwritePolicy decides what unpack does when a destination item already
// exists.
type OverwritePolicy int

const (
	// OverwriteFail aborts before writing anything. This is the default.
	OverwriteFail OverwritePolicy = iota
	// OverwriteForce replaces the existing item, keeping a backup until the
	// whole unpack has succeeded.
	OverwriteForce
	// OverwriteSkip leaves the existing item alone.
	OverwriteSkip
)

// UnpackOptions controls an unpack run.
type UnpackOptions struct {
	// Dest becomes MISE_DATA_DIR on the target machine.
	Dest string
	// SafeBase, when set, is a trusted ancestor of Dest: no existing symlink
	// between it and Dest will be followed. It is set when Dest was derived
	// from the archive's own manifest.
	SafeBase  string
	Policy    OverwritePolicy
	SkipArch  bool
	DryRun    bool
	Log       io.Writer
	MpackVers string
}

// unit is one independently installable subtree of the archive.
type unit struct {
	archivePrefix string // path inside the archive, no trailing slash
	destRel       string // path relative to Dest
	isFile        bool
	// generated marks a unit produced by unpack rather than read from the
	// archive; it is staged separately but installed the same way.
	generated bool
}

// DefaultDest reproduces the source project's home-relative location under
// the current user's home and puts the toolchain in its .mpack subdirectory:
//
//	packed   /Users/alice/work/myapp
//	unpacked /home/bob/work/myapp/.mpack
//
// Only the project's tooling is placed there; mpack archives never contain
// project source files, and nothing outside .mpack is touched.
//
// The archive supplies the relative path, so it is treated as untrusted input
// and validated before use. If the project was packed from outside the home
// directory there is nothing to reproduce and the caller must pass --dest.
func DefaultDest(m *Manifest) (string, error) {
	dest, _, err := DefaultDestWithBase(m)
	return dest, err
}

// DefaultDestWithBase is DefaultDest plus the trusted base (the home
// directory) to pass as UnpackOptions.SafeBase, so no existing symlink on the
// way down to the destination is followed.
func DefaultDestWithBase(m *Manifest) (dest, base string, err error) {
	rel := m.HomeRelativeProjectPath
	if rel == "" {
		return "", "", errors.New("this archive was packed from a directory outside the home directory, so there is no sensible default location: pass --dest DIR")
	}
	clean, err := CheckArchiveName(rel)
	if err != nil {
		return "", "", fmt.Errorf("archive records an unusable project path %q: %w (pass --dest DIR)", rel, err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", "", fmt.Errorf("cannot determine the home directory: %w (pass --dest DIR)", err)
	}
	home, err = filepath.Abs(home)
	if err != nil {
		return "", "", err
	}
	home = filepath.Clean(home)
	dest = filepath.Join(home, filepath.FromSlash(clean), MpackSubdir)
	// Belt and braces: the join above must not have escaped the home dir.
	if !isUnder(home, dest) {
		return "", "", fmt.Errorf("refusing default destination %q: it is outside %s (pass --dest DIR)", dest, home)
	}
	return dest, home, nil
}

// ReadManifest reads just the manifest from an archive. The manifest is always
// the first entry, so this never scans the whole payload.
func ReadManifest(r io.Reader) (*Manifest, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, fmt.Errorf("not a gzip archive: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	hdr, err := tr.Next()
	if err != nil {
		return nil, fmt.Errorf("reading archive: %w", err)
	}
	if hdr.Name != ManifestName {
		return nil, fmt.Errorf("first archive entry is %q, want %q; not an mpack archive", hdr.Name, ManifestName)
	}
	var m Manifest
	if err := json.NewDecoder(io.LimitReader(tr, 1<<22)).Decode(&m); err != nil {
		return nil, fmt.Errorf("parsing manifest: %w", err)
	}
	if m.Schema != SchemaVersion {
		return nil, fmt.Errorf("archive schema %d is not supported by this mpack (want %d)", m.Schema, SchemaVersion)
	}
	return &m, nil
}

// CheckPlatform compares the archive's platform against the current one.
func CheckPlatform(m *Manifest) error {
	if m.OS != runtime.GOOS || m.Arch != runtime.GOARCH {
		return fmt.Errorf("archive was packed on %s/%s but this machine is %s/%s; mise tools are native binaries and will not run (use --no-arch-check to override)",
			m.OS, m.Arch, runtime.GOOS, runtime.GOARCH)
	}
	return nil
}

func unitsFor(m *Manifest) []unit {
	units := []unit{
		{archivePrefix: MiseBinName, destRel: filepath.FromSlash(MiseBinName), isFile: true},
		{archivePrefix: ConfigName, destRel: filepath.FromSlash(ConfigName), isFile: true},
	}
	if m.HasUnpacker {
		units = append(units, unit{archivePrefix: UnpackerName, destRel: filepath.FromSlash(UnpackerName), isFile: true})
	}
	for _, t := range m.Tools {
		p := path.Join(InstallsPrefix, t.RelPath)
		units = append(units, unit{archivePrefix: p, destRel: filepath.FromSlash(p)})
	}
	if m.HasCatalog {
		p := path.Join(InstallsPrefix, CatalogDir)
		units = append(units, unit{archivePrefix: p, destRel: filepath.FromSlash(p)})
	}
	// The generated sidecars are units too: they must respect the overwrite
	// policy and go through the same transaction, never a blind write.
	units = append(units,
		unit{archivePrefix: ActivateName, destRel: ActivateName, isFile: true, generated: true},
		unit{archivePrefix: DirenvName, destRel: DirenvName, isFile: true, generated: true},
		unit{archivePrefix: InstalledManifestName, destRel: InstalledManifestName, isFile: true, generated: true},
	)
	return units
}

// stageSidecars writes the generated files into the staging directory so the
// move phase treats them exactly like archived content.
func stageSidecars(staging, dest string, m *Manifest) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	files := []struct {
		name string
		body []byte
		mode os.FileMode
	}{
		{InstalledManifestName, append(data, '\n'), 0o644},
		{ActivateName, []byte(ActivateScript(dest)), 0o755},
		{DirenvName, []byte(DirenvScript(dest)), 0o644},
	}
	for _, f := range files {
		full, err := ResolveUnder(staging, f.name)
		if err != nil {
			return err
		}
		if err := writeRegular(full, f.mode, bytes.NewReader(f.body), int64(len(f.body))); err != nil {
			return err
		}
	}
	return nil
}

// ensureDestDir creates the destination directory without ever following an
// existing symlink into it. When safeBase is set (the destination came from
// the archive's own project path, which is untrusted input) every component
// below safeBase is checked.
func ensureDestDir(dest, safeBase string) error {
	if fi, err := os.Lstat(dest); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing to unpack into %s: it is a symlink", dest)
	}
	if safeBase == "" {
		return os.MkdirAll(dest, 0o755)
	}
	safeBase = filepath.Clean(safeBase)
	if fi, err := os.Lstat(safeBase); err != nil {
		return err
	} else if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing to unpack below %s: it is a symlink", safeBase)
	}
	rel, ok := RelUnder(safeBase, dest)
	if !ok {
		return fmt.Errorf("destination %s is outside %s", dest, safeBase)
	}
	if rel == "." {
		return os.MkdirAll(dest, 0o755)
	}
	return MkdirAllNoSymlink(safeBase, rel, 0o755)
}

// Unpack extracts an archive into opts.Dest. It never writes directly into the
// destination: everything lands in a staging directory first and is only moved
// into place once the whole payload has been validated and extracted.
func Unpack(archive io.Reader, opts UnpackOptions) (*Manifest, error) {
	if opts.Dest == "" {
		return nil, errors.New("destination directory is required")
	}
	dest, err := filepath.Abs(opts.Dest)
	if err != nil {
		return nil, err
	}
	dest = filepath.Clean(dest)

	gz, err := gzip.NewReader(archive)
	if err != nil {
		return nil, fmt.Errorf("not a gzip archive: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)

	hdr, err := tr.Next()
	if err != nil {
		return nil, fmt.Errorf("reading archive: %w", err)
	}
	if hdr.Name != ManifestName {
		return nil, fmt.Errorf("first archive entry is %q, want %q; not an mpack archive", hdr.Name, ManifestName)
	}
	var m Manifest
	if err := json.NewDecoder(io.LimitReader(tr, 1<<22)).Decode(&m); err != nil {
		return nil, fmt.Errorf("parsing manifest: %w", err)
	}
	if m.Schema != SchemaVersion {
		return nil, fmt.Errorf("archive schema %d is not supported by this mpack (want %d)", m.Schema, SchemaVersion)
	}
	if !opts.SkipArch {
		if err := CheckPlatform(&m); err != nil {
			return nil, err
		}
	} else if m.OS != runtime.GOOS || m.Arch != runtime.GOARCH {
		logf(opts.Log, "warning: platform mismatch (%s/%s vs %s/%s) ignored via --no-arch-check\n", m.OS, m.Arch, runtime.GOOS, runtime.GOARCH)
	}
	if len(m.UnresolvedLinks) > 0 {
		logf(opts.Log, "warning: archive was packed with %d unrelocatable symlink(s); those links are absent\n", len(m.UnresolvedLinks))
	}

	units := unitsFor(&m)

	// Decide the per-unit action up front so a conflict aborts before any
	// filesystem change.
	skip := map[string]bool{}
	var conflicts []string
	for _, u := range units {
		full := filepath.Join(dest, u.destRel)
		if _, err := os.Lstat(full); err == nil {
			switch opts.Policy {
			case OverwriteFail:
				conflicts = append(conflicts, full)
			case OverwriteSkip:
				skip[u.archivePrefix] = true
			}
		} else if !os.IsNotExist(err) {
			return nil, err
		}
	}
	if len(conflicts) > 0 {
		return nil, fmt.Errorf("%d destination path(s) already exist:\n  %s\nre-run with --force to replace them or --skip-existing to keep them",
			len(conflicts), strings.Join(conflicts, "\n  "))
	}

	if opts.DryRun {
		for _, u := range units {
			action := "install"
			if skip[u.archivePrefix] {
				action = "skip (exists)"
			}
			logf(opts.Log, "  %-12s %s\n", action, filepath.Join(dest, u.destRel))
		}
		return &m, nil
	}

	if err := ensureDestDir(dest, opts.SafeBase); err != nil {
		return nil, err
	}
	staging, err := os.MkdirTemp(dest, ".mpack-staging-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(staging)

	// destInstallsRoot is where relocated absolute symlinks must point. It is
	// the final location, not the staging one, because the links are moved
	// into place intact.
	destInstallsRoot := filepath.Join(dest, InstallsPrefix)
	if err := extract(tr, staging, destInstallsRoot, skip); err != nil {
		return nil, err
	}
	if err := stageSidecars(staging, dest, &m); err != nil {
		return nil, err
	}

	tx := &moveTx{dest: dest}
	if err := tx.run(units, staging, skip, opts); err != nil {
		rollbackErr := tx.rollback()
		if rollbackErr != nil {
			return nil, fmt.Errorf("%w (rollback also failed: %v; backups kept in %s)", err, rollbackErr, tx.backupDir)
		}
		return nil, err
	}
	if err := tx.commit(); err != nil {
		return nil, err
	}
	return &m, nil
}

// extract writes every archive entry below staging, validating each path.
func extract(tr *tar.Reader, staging, destInstallsRoot string, skip map[string]bool) error {
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		name, err := CheckArchiveName(hdr.Name)
		if err != nil {
			return err
		}
		if name == ManifestName {
			return fmt.Errorf("duplicate %s entry in archive", ManifestName)
		}
		if skippedUnit(name, skip) {
			continue
		}
		full, err := ResolveUnder(staging, name)
		if err != nil {
			return err
		}
		if err := MkdirAllNoSymlink(staging, path.Dir(name), 0o755); err != nil {
			return err
		}
		if _, err := os.Lstat(full); err == nil {
			return fmt.Errorf("duplicate archive entry %q", name)
		}
		perm := os.FileMode(hdr.Mode).Perm()

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.Mkdir(full, perm|0o700); err != nil && !os.IsExist(err) {
				return err
			}
		case tar.TypeReg:
			if err := writeRegular(full, perm, tr, hdr.Size); err != nil {
				return err
			}
		case tar.TypeSymlink:
			target, err := resolveLinkTarget(hdr.Linkname, name, staging, destInstallsRoot)
			if err != nil {
				return fmt.Errorf("symlink %q: %w", name, err)
			}
			if err := os.Symlink(target, full); err != nil {
				return err
			}
		default:
			return fmt.Errorf("archive entry %q has unsupported type %q; mpack only extracts regular files, directories and symlinks", name, string(hdr.Typeflag))
		}
		if !hdr.ModTime.IsZero() && hdr.Typeflag != tar.TypeSymlink {
			_ = os.Chtimes(full, hdr.ModTime, hdr.ModTime)
		}
	}
}

func skippedUnit(name string, skip map[string]bool) bool {
	for prefix := range skip {
		if name == prefix || strings.HasPrefix(name, prefix+"/") {
			return true
		}
	}
	return false
}

func writeRegular(full string, perm os.FileMode, r io.Reader, size int64) error {
	f, err := os.OpenFile(full, os.O_CREATE|os.O_EXCL|os.O_WRONLY, perm)
	if err != nil {
		return err
	}
	// Copy at most size bytes so a malformed stream cannot fill the disk.
	n, err := io.Copy(f, io.LimitReader(r, size))
	if err != nil {
		f.Close()
		return err
	}
	if n != size {
		f.Close()
		return fmt.Errorf("%s: truncated archive entry (%d of %d bytes)", full, n, size)
	}
	if err := f.Close(); err != nil {
		return err
	}
	// O_CREATE honours umask; set the archived permissions explicitly so the
	// executable bit survives.
	return os.Chmod(full, perm)
}

// resolveLinkTarget turns an archived symlink target into the target to create
// on disk, rejecting anything that would point outside the unpacked tree.
func resolveLinkTarget(stored, entryName, staging, destInstallsRoot string) (string, error) {
	if stored == "" {
		return "", errors.New("empty target")
	}
	if strings.ContainsRune(stored, 0) {
		return "", errors.New("target contains a NUL byte")
	}
	if rest, ok := strings.CutPrefix(stored, RootSentinel+"/"); ok {
		clean, err := CheckArchiveName(rest)
		if err != nil {
			return "", err
		}
		return filepath.Join(destInstallsRoot, filepath.FromSlash(clean)), nil
	}
	if stored == RootSentinel {
		return destInstallsRoot, nil
	}
	if filepath.IsAbs(stored) || strings.HasPrefix(stored, "/") {
		return "", fmt.Errorf("absolute target %q was not rewritten by pack", stored)
	}
	// Relative target: it must stay inside the unpacked tree.
	joined := path.Join(path.Dir(entryName), filepath.ToSlash(stored))
	if joined == ".." || strings.HasPrefix(joined, "../") {
		return "", fmt.Errorf("relative target %q escapes the archive root", stored)
	}
	if _, err := ResolveUnder(staging, joined); err != nil {
		return "", err
	}
	return stored, nil
}

// moveTx moves staged units into the destination, keeping replaced items in a
// backup directory so a failure halfway through can be undone.
type moveTx struct {
	dest      string
	backupDir string
	moved     []string // destination paths created by this transaction
	backups   [][2]string
}

func (t *moveTx) run(units []unit, staging string, skip map[string]bool, opts UnpackOptions) error {
	for _, u := range units {
		if skip[u.archivePrefix] {
			logf(opts.Log, "  skip     %s (already exists)\n", filepath.Join(t.dest, u.destRel))
			continue
		}
		src := filepath.Join(staging, filepath.FromSlash(u.archivePrefix))
		if _, err := os.Lstat(src); err != nil {
			return fmt.Errorf("archive is missing %s", u.archivePrefix)
		}
		dst := filepath.Join(t.dest, u.destRel)
		if err := MkdirAllNoSymlink(t.dest, path.Dir(u.archivePrefix), 0o755); err != nil {
			return err
		}
		if _, err := os.Lstat(dst); err == nil {
			backup, err := t.stash(dst)
			if err != nil {
				return err
			}
			t.backups = append(t.backups, [2]string{dst, backup})
		}
		if err := os.Rename(src, dst); err != nil {
			return fmt.Errorf("installing %s: %w", u.archivePrefix, err)
		}
		t.moved = append(t.moved, dst)
		logf(opts.Log, "  install  %s\n", dst)
	}
	return nil
}

func (t *moveTx) stash(dst string) (string, error) {
	if t.backupDir == "" {
		dir, err := os.MkdirTemp(t.dest, ".mpack-backup-")
		if err != nil {
			return "", err
		}
		t.backupDir = dir
	}
	backup := filepath.Join(t.backupDir, fmt.Sprintf("%d-%s", len(t.backups), filepath.Base(dst)))
	if err := os.Rename(dst, backup); err != nil {
		return "", fmt.Errorf("backing up %s: %w", dst, err)
	}
	return backup, nil
}

// rollback undoes everything run() did, restoring replaced items.
func (t *moveTx) rollback() error {
	var firstErr error
	for i := len(t.moved) - 1; i >= 0; i-- {
		if err := os.RemoveAll(t.moved[i]); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	for i := len(t.backups) - 1; i >= 0; i-- {
		dst, backup := t.backups[i][0], t.backups[i][1]
		if err := os.Rename(backup, dst); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if firstErr == nil && t.backupDir != "" {
		_ = os.RemoveAll(t.backupDir)
	}
	return firstErr
}

// commit discards the backups of successfully replaced items.
func (t *moveTx) commit() error {
	if t.backupDir == "" {
		return nil
	}
	return os.RemoveAll(t.backupDir)
}

// miseEnvPreamble is the part both generated scripts share: point mise at the
// unpacked tree, and make sure nothing inherited can point it somewhere else.
//
// Clearing MISE_* first is not belt and braces. An inherited
// MISE_GLOBAL_CONFIG_FILE overrides MISE_CONFIG_DIR outright - measured, not
// assumed - so a shell that already had one would silently load a different
// configuration than the archive carries. Every MISE_* the caller happens to
// have is dropped, then only what the snapshot needs is set.
func miseEnvPreamble(rootExpr string) string {
	return fmt.Sprintf(`MPACK_ROOT=%s

# Drop any inherited MISE_* so nothing redirects mise away from the snapshot.
# Names only; no value is read or printed.
for _mpack_var in $(env | sed -n 's/^\(MISE_[A-Za-z0-9_]*\)=.*/\1/p'); do
  unset "$_mpack_var"
done
unset _mpack_var

export MISE_DATA_DIR="$MPACK_ROOT"
export MISE_CONFIG_DIR="$MPACK_ROOT/config"
# Set explicitly: this one wins over MISE_CONFIG_DIR, so leaving it unset
# would let a stray value in the environment take over.
export MISE_GLOBAL_CONFIG_FILE="$MPACK_ROOT/config/config.toml"
# Everything is already on disk; never reach for the network.
export MISE_OFFLINE=1
`, rootExpr)
}

// ActivateScript renders the POSIX shell snippet that points mise at the
// unpacked tree. It is meant to be sourced, not executed, and works in bash
// and zsh alike.
func ActivateScript(dest string) string {
	q := shellQuote(dest)
	return fmt.Sprintf(`#!/bin/sh
# Generated by mpack on %s.
# Source this file; do not execute it:
#   bash/zsh:  source %s/`+ActivateName+`
#   sh/dash:   . %s/`+ActivateName+`
#
# MISE_DATA_DIR and MISE_CONFIG_DIR must be set before mise starts, which is
# why they are exported here rather than passed on the mise command line.

%s
export PATH="$MPACK_ROOT/bin:$PATH"

# Put the tools on PATH right now. "mise activate" alone is not enough: its
# hook only runs on the next prompt, so a non-interactive shell would silently
# keep using the system copy of a tool. "mise env -s bash" emits plain POSIX
# "export" lines, so this works in sh, dash, bash and zsh alike.
#
# -C / evaluates the snapshot rather than whatever mise.toml happens to sit in
# the current directory or above it, which describes the machine the archive
# came from.
# Captured rather than eval'd inline so a failure is not thrown away.
if ! _mpack_env="$("$MPACK_ROOT/bin/mise" -C / env -s bash)"; then
  echo "mpack: $MPACK_ROOT/bin/mise could not produce the environment." >&2
  unset _mpack_env
  return 1 2>/dev/null || exit 1
fi
eval "$_mpack_env"
unset _mpack_env

# Then, for interactive shells, install the hook so the environment keeps up
# with directory changes. Other shells are supported too, e.g.
#   eval "$("$MPACK_ROOT/bin/mise" activate fish)"
if [ -n "${ZSH_VERSION-}" ]; then
  eval "$("$MPACK_ROOT/bin/mise" activate zsh)"
elif [ -n "${BASH_VERSION-}" ]; then
  eval "$("$MPACK_ROOT/bin/mise" activate bash)"
fi
`, time.Now().UTC().Format(time.RFC3339), q, q, miseEnvPreamble(q))
}

// DirenvScript renders the file a project's .envrc sources. It is the direnv
// counterpart of ActivateScript: the same environment, but with no shell hook
// of its own. direnv evaluates this in a separate process and takes care of
// applying the result on entry and undoing it on exit, so a mise hook here
// would be a second mechanism fighting over the same variables.
func DirenvScript(dest string) string {
	q := shellQuote(dest)
	return fmt.Sprintf(`# Generated by mpack on %s. Sourced from a project's .envrc:
#
#     source_env %s/`+DirenvName+`
#
# Not meant to be sourced by hand - use `+ActivateName+` for that.
# No mise shell hook is installed here: direnv evaluates this in its own
# process and handles entering and leaving the directory itself.

# direnv changes into the directory of the file it sources, so $PWD is this
# tree. Deriving the root instead of baking it in keeps the project movable:
# copy or rename it and the .envrc still finds its own .mpack.
%s
# Failures here exit rather than return. "return" would merely end this file
# and let the calling .envrc carry on, leaving direnv to apply whatever had
# been exported so far - a half-built environment reported as success.
if [ ! -x "$MPACK_ROOT/bin/mise" ]; then
  echo "mpack: $MPACK_ROOT does not contain bin/mise." >&2
  echo "mpack: this file must be loaded with direnv's source_env, which runs it" >&2
  echo "mpack: from its own directory. For manual use source `+ActivateName+` instead." >&2
  exit 1
fi

# -C / keeps this to the snapshot: mise reads the config this file points at
# rather than the project's own mise.toml and whatever sits above it, which
# describe the machine the archive came from, not what is unpacked here.
# Captured rather than eval'd inline: "eval $(...)" throws the exit status
# away, so a mise that failed would leave the environment untouched and the
# rest of this file would carry on adding directories to PATH as if it had
# worked.
if ! _mpack_env="$("$MPACK_ROOT/bin/mise" -C / env -s bash)"; then
  echo "mpack: $MPACK_ROOT/bin/mise could not produce the environment." >&2
  echo "mpack: not loading a partial toolchain; the directory is left as it was." >&2
  exit 1
fi
eval "$_mpack_env"
unset _mpack_env

# mise and mpack themselves, for "mise ls" and a later "mpack unpack".
PATH_add "$MPACK_ROOT/bin"

# Re-evaluate when the unpacked tree is replaced.
watch_file "$MPACK_ROOT/`+InstalledManifestName+`"
`, time.Now().UTC().Format(time.RFC3339), q, miseEnvPreamble(`"$PWD"`))
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
