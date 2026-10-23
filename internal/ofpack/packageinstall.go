package ofpack

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// InstallOptions controls `ofpack packages install`.
type InstallOptions struct {
	// Dest is the unpacked ofpack directory.
	Dest string
	// OSReleasePath is where the running system identifies itself; it is a
	// field so tests can point it elsewhere.
	OSReleasePath string
	// SkipOSCheck installs despite a release mismatch. The closure was
	// resolved for one release, so this is a deliberate override.
	SkipOSCheck bool
	// DryRun reports what would run without running it.
	DryRun bool
	Log    io.Writer
	// uid is the effective user id; a field so the root check is testable.
	uid int
}

// InstallPackages puts the carried packages onto this system.
//
// Nothing here happens implicitly. Unpack leaves the files on disk and stops;
// installing them changes the whole machine, so it is a separate command, it
// refuses to run against a different distribution release, and it verifies
// every file against the manifest before a privileged package manager is
// allowed near them.
func InstallPackages(opts InstallOptions) error {
	dest, err := filepath.Abs(opts.Dest)
	if err != nil {
		return err
	}
	m, err := readInstalledManifest(dest)
	if err != nil {
		return err
	}
	if m.Packages == nil {
		return fmt.Errorf("%s carries no operating system packages; it was not packed with --packages", dest)
	}
	pkgs := m.Packages

	osPath := opts.OSReleasePath
	if osPath == "" {
		osPath = "/etc/os-release"
	}
	here, err := ReadOSRelease(osPath)
	if err != nil {
		return fmt.Errorf("reading %s: %w", osPath, err)
	}
	if !pkgs.OS.Matches(here) {
		if !opts.SkipOSCheck {
			return fmt.Errorf(`these packages were resolved for %s, but this system is %s.

A dependency closure is only valid for the release it was resolved against; installing it elsewhere can pull in libraries built for another release. Re-pack on a %s machine, or pass --no-os-check if you are certain.`,
				pkgs.OS, here, pkgs.OS)
		}
		logf(opts.Log, "warning: installing packages resolved for %s on %s, as asked\n", pkgs.OS, here)
	}

	dir := filepath.Join(dest, PackagesPrefix, pkgs.Manager)
	files, err := verifyPackageFiles(dest, dir, pkgs)
	if err != nil {
		return err
	}
	logf(opts.Log, "  %d package files verified against the manifest\n", len(files))

	name, args, err := installCommand(pkgs.Manager, files)
	if err != nil {
		return err
	}
	logf(opts.Log, "  %s runs with its repositories disabled, so nothing is fetched\n", name)
	if opts.DryRun {
		logf(opts.Log, "dry run, would run:\n  %s %s\n", name, strings.Join(args, " "))
		return nil
	}

	// Only now, with the files checked and the command fixed, does this need
	// to be root. Asking earlier would make people run the whole thing as
	// root to find out it was going to refuse anyway.
	uid := opts.uid
	if uid == 0 && os.Getuid() != 0 {
		uid = os.Getuid()
	}
	if uid != 0 {
		return fmt.Errorf("installing packages needs root. ofpack does not escalate by itself; re-run it as root:\n  sudo ofpack packages install --dest %s", dest)
	}
	return runPackageTool(opts.Log, name, args...)
}

// verifyPackageFiles checks every carried file against the manifest before
// any of them is handed to a package manager running as root. An archive is
// untrusted input, and a package file is executable content.
func verifyPackageFiles(dest, dir string, pkgs *Packages) ([]string, error) {
	if len(pkgs.Files) == 0 {
		return nil, fmt.Errorf("the manifest lists no package files")
	}
	// The directory itself must not be reached through a link. Everything
	// below is about to be handed to a package manager running as root, and
	// a link swapped in after unpack would redirect it somewhere else
	// entirely.
	if err := checkNoSymlinkPath(dest, PackagesPrefix, pkgs.Manager); err != nil {
		return nil, err
	}

	var paths []string
	seen := map[string]bool{}
	for _, want := range pkgs.Files {
		if err := checkPackageFileName(want.Name); err != nil {
			return nil, err
		}
		if seen[want.Name] {
			return nil, fmt.Errorf("the manifest lists %s more than once", want.Name)
		}
		seen[want.Name] = true
		if len(want.SHA256) != 64 {
			return nil, fmt.Errorf("the manifest records a malformed digest for %s", want.Name)
		}
		full := filepath.Join(dir, want.Name)
		// Lstat, not Stat: a symlink here would be hashed and installed
		// through, so what is checked would not be what is read.
		fi, err := os.Lstat(full)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", want.Name, err)
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("%s is a symlink; package files must be regular files, and nothing was installed", want.Name)
		}
		if !fi.Mode().IsRegular() {
			return nil, fmt.Errorf("%s is not a regular file (%s); nothing was installed", want.Name, fi.Mode().Type())
		}
		sum, size, err := HashFile(full)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", want.Name, err)
		}
		if sum != want.SHA256 {
			return nil, fmt.Errorf("%s does not match the manifest (%s on disk, %s recorded); nothing was installed",
				want.Name, sum[:12], want.SHA256[:12])
		}
		if size != want.Size {
			return nil, fmt.Errorf("%s is %d bytes, the manifest records %d; nothing was installed", want.Name, size, want.Size)
		}
		paths = append(paths, full)
	}

	// A file in the directory that the manifest does not account for has no
	// business being installed, and its presence means the tree is not what
	// the archive described.
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	known := map[string]bool{}
	for _, f := range pkgs.Files {
		known[f.Name] = true
	}
	var extra []string
	suffix := PackageSuffix(pkgs.Manager)
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), suffix) && !known[e.Name()] {
			extra = append(extra, e.Name())
		}
	}
	if len(extra) > 0 {
		sort.Strings(extra)
		return nil, fmt.Errorf("%s holds package files the manifest does not list: %s; nothing was installed",
			dir, strings.Join(extra, ", "))
	}
	sort.Strings(paths)
	return paths, nil
}

// checkPackageFileName limits a manifest entry to one plain file name. A
// name with a separator in it would place the install somewhere other than
// the directory that was verified.
func checkPackageFileName(name string) error {
	if name == "" || name == "." || name == ".." {
		return fmt.Errorf("the manifest names an unusable package file %q", name)
	}
	if name != filepath.Base(name) || strings.ContainsAny(name, `/\`) {
		return fmt.Errorf("the manifest names a package file with a path in it (%q); only plain file names belong there", name)
	}
	if _, err := CheckArchiveName(name); err != nil {
		return fmt.Errorf("the manifest names an unusable package file %q: %w", name, err)
	}
	return nil
}

// checkNoSymlinkPath walks the components below root and refuses any that is
// a symlink.
func checkNoSymlinkPath(root string, parts ...string) error {
	// The root is checked too: a destination that is itself a link would
	// otherwise be walked straight through.
	cur := root
	fi, err := os.Lstat(cur)
	if err != nil {
		return fmt.Errorf("%s: %w", cur, err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symlink; refusing to install packages through it", cur)
	}
	for _, part := range parts {
		cur = filepath.Join(cur, part)
		fi, err = os.Lstat(cur)
		if err != nil {
			return fmt.Errorf("%s: %w", cur, err)
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%s is a symlink; refusing to install packages through it", cur)
		}
		if !fi.IsDir() {
			return fmt.Errorf("%s is not a directory", cur)
		}
	}
	return nil
}

// installCommand builds the offline installation command for a manager.
//
// Both forms keep the package manager away from the network by configuration
// rather than by trusting the machine to be disconnected, and both let the
// manager order the packages itself instead of installing them one by one.
func installCommand(manager string, files []string) (string, []string, error) {
	switch manager {
	case ManagerDNF:
		args := []string{"install", "-y", "--disablerepo", "*", "--cacheonly"}
		return "dnf", append(args, files...), nil
	case ManagerAPT:
		// --no-download is not usable here: with local paths apt fails with
		// "Pathname to install is not absolute" on a transitive dependency.
		// Emptying the source lists has the same effect - no repository is
		// consulted - and works.
		args := []string{
			"install", "-y",
			"-o", "Dir::Etc::sourcelist=/dev/null",
			"-o", "Dir::Etc::sourceparts=/dev/null",
			"-o", "Acquire::Retries=0",
		}
		return "apt-get", append(args, files...), nil
	default:
		return "", nil, fmt.Errorf("unknown package manager %q", manager)
	}
}

// PackagesSummary describes a carried package set for `inspect`.
func PackagesSummary(p *Packages) string {
	var b strings.Builder
	fmt.Fprintf(&b, "packages: %d %s file(s) for %s\n", len(p.Files), p.Manager, p.OS)
	fmt.Fprintf(&b, "  requested: %s\n", strings.Join(p.Requested, ", "))
	fmt.Fprintf(&b, "  closure:   %s\n", p.Closure)
	if len(p.Repos) > 0 {
		fmt.Fprintf(&b, "  repos:     %s\n", strings.Join(p.Repos, ", "))
	}
	if p.BaseImage != "" {
		fmt.Fprintf(&b, "  resolved in: %s\n", p.BaseImage)
	}
	if p.EssentialExcluded {
		fmt.Fprintf(&b, "  note: covers declared dependencies; Essential packages are assumed present\n")
	}
	return b.String()
}
