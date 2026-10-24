package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/konono/ofpack/internal/ofpack"
)

// packageOptions are the --packages flags, gathered so the pack paths can
// pass them around as one thing.
type packageOptions struct {
	enabled bool
	repos   []string
	base    string
	engine  string
	// target is the platform the packages are for, as "<os>/<arch>". It is
	// required with a base image, since the closure is then not for this
	// machine.
	target string
}

// fetchPackages resolves the project's packages.txt into a closure on disk.
// It returns nil when --packages was not given: the file is only read when
// it is asked for, so a project can hold one without every pack picking it
// up.
func fetchPackages(workDir, stageDir string, opts packageOptions) (*ofpack.Packages, error) {
	if !opts.enabled {
		return nil, nil
	}
	listPath := filepath.Join(workDir, ofpack.PackagesListName)
	names, err := ofpack.ReadPackagesList(listPath)
	if err != nil {
		return nil, err
	}

	// A base image means the closure is resolved somewhere other than here,
	// for a platform that is not necessarily this one. Reading this
	// machine's /etc/os-release would be meaningless - and on macOS there
	// is not one - so the image is asked instead.
	if opts.base != "" {
		if opts.target == "" {
			return nil, fmt.Errorf("--package-base needs --target, for example --target linux/amd64:\nthe packages are for another machine, so the architecture has to be stated rather than taken from this one")
		}
		fmt.Fprintf(os.Stderr, "Resolving %d package(s) from %s in %s for %s (apt)\n",
			len(names), ofpack.PackagesListName, opts.base, opts.target)
		for _, n := range names {
			fmt.Fprintf(os.Stderr, "    %s\n", n)
		}
		dest := filepath.Join(stageDir, ofpack.PackagesPrefix, ofpack.ManagerAPT)
		pkgs, err := ofpack.FetchPackages(ofpack.ManagerAPT, ofpack.OSRelease{}, ofpack.FetchOptions{
			Names: names, Dest: dest, BaseImage: opts.base,
			Engine: opts.engine, Platform: opts.target, Log: os.Stderr,
		})
		if err != nil {
			return nil, err
		}
		fmt.Fprintf(os.Stderr, "  %d package file(s), %s\n", len(pkgs.Files), pkgs.Closure)
		return pkgs, nil
	}

	// Native: this machine's own package manager, against its own
	// repositories.
	host, err := ofpack.ReadOSRelease("/etc/os-release")
	if err != nil {
		return nil, fmt.Errorf("--packages without --package-base resolves on this machine, which needs to be the target distribution: %w", err)
	}
	manager, err := ofpack.ManagerFor(host)
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(os.Stderr, "Resolving %d package(s) from %s for %s (%s)\n",
		len(names), ofpack.PackagesListName, host, manager)
	for _, n := range names {
		fmt.Fprintf(os.Stderr, "    %s\n", n)
	}
	dest := filepath.Join(stageDir, ofpack.PackagesPrefix, manager)
	pkgs, err := ofpack.FetchPackages(manager, host, ofpack.FetchOptions{
		Names: names, Dest: dest, Repos: opts.repos, Engine: opts.engine,
		Platform: opts.target, Log: os.Stderr,
	})
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(os.Stderr, "  %d package file(s), %s\n", len(pkgs.Files), pkgs.Closure)
	return pkgs, nil
}

// packagesOnlyPack builds an archive that carries operating system packages
// and nothing else.
//
// A project may have a packages.txt and no mise tools at all. Such an archive
// has no mise binary, no tool tree and no activate.sh, and inventing those
// would hand the target files that point at things the archive does not
// contain.
func packagesOnlyPack(outName, workDir string, opts packageOptions, ofpackBin, builderImage, noUnpackerEngine string, noUnpacker, dryRun bool) error {
	// The archive describes the machine the packages are for, not the one
	// that fetched them, and the bundled unpacker has to run there too.
	target := opts.target
	if target == "" {
		target = ofpack.HostTarget()
	}
	output, err := filepath.Abs(outName)
	if err != nil {
		return err
	}
	stage, err := os.MkdirTemp(filepath.Dir(output), ".ofpack-packages-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)

	pkgs, err := fetchPackages(workDir, stage, opts)
	if err != nil {
		return err
	}

	plan, err := ofpack.BuildPackagesOnlyPlan(ofpack.PackagesOnlyOptions{
		WorkDir:  workDir,
		StageDir: stage,
		Packages: pkgs,
		Target:   target,
		Version:  version,
		Log:      os.Stderr,
	})
	if err != nil {
		return err
	}
	if err := attachUnpacker(plan, target, ofpackBin, builderImage, noUnpackerEngine, noUnpacker); err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "\nThis archive carries operating system packages only: no mise, no tools,\nand no activate.sh, because there is nothing to activate.\n")
	fmt.Fprint(os.Stderr, ofpack.PackagesSummary(pkgs))
	if dryRun {
		fmt.Fprintln(os.Stderr, "dry run: no archive written")
		return nil
	}
	if err := writeArchive(plan, output); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "\nOn the disconnected machine:\n"+
		"  ofpack unpack -f %s --dest DIR\n"+
		"  sudo ofpack packages install --dest DIR\n", filepath.Base(output))
	return nil
}

// attachPackagesTo fetches the project's packages and adds them to a plan
// that already carries a toolchain, so a single archive holds both.
func attachPackagesTo(plan *ofpack.Plan, workDir string, opts packageOptions) (func(), error) {
	noop := func() {}
	if !opts.enabled {
		return noop, nil
	}
	stage, err := os.MkdirTemp("", "ofpack-packages-")
	if err != nil {
		return noop, err
	}
	cleanup := func() { os.RemoveAll(stage) }
	pkgs, err := fetchPackages(workDir, stage, opts)
	if err != nil {
		cleanup()
		return noop, err
	}
	if err := plan.AttachPackages(stage, pkgs); err != nil {
		cleanup()
		return noop, err
	}
	fmt.Fprint(os.Stderr, ofpack.PackagesSummary(pkgs))
	return cleanup, nil
}

func cmdPackages(args []string) error {
	if len(args) == 0 || args[0] != "install" {
		return fmt.Errorf("usage: ofpack packages install [--dest DIR] [--no-os-check] [--dry-run]")
	}
	fs := flag.NewFlagSet("packages install", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), `Usage: ofpack packages install [flags]

Installs the operating system packages an archive carries. Unpack puts them
on disk and stops; installing them changes the whole machine, so it is this
separate command.

  ofpack unpack -f archive.tar.gz --dest ~/tools
  sudo ofpack packages install --dest ~/tools

Every package file is checked against the manifest before the package
manager is run, the manager runs with its repositories disabled so nothing
is fetched, and a closure resolved for another distribution release is
refused.

Flags:
`)
		fs.PrintDefaults()
	}
	dest := fs.String("dest", "", "the unpacked ofpack directory (default: ./"+ofpack.OfpackSubdir+")")
	noOSCheck := fs.Bool("no-os-check", false, "install even though the packages were resolved for another distribution release")
	dryRun := fs.Bool("dry-run", false, "verify and print the command without installing")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	target := *dest
	if target == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return err
		}
		target = filepath.Join(cwd, ofpack.OfpackSubdir)
	}
	return ofpack.InstallPackages(ofpack.InstallOptions{
		Dest:        target,
		SkipOSCheck: *noOSCheck,
		DryRun:      *dryRun,
		Log:         os.Stderr,
	})
}

// packageRepoList collects repeated --package-repo values.
type packageRepoList []string

func (p *packageRepoList) String() string     { return strings.Join(*p, ",") }
func (p *packageRepoList) Set(v string) error { *p = append(*p, v); return nil }
