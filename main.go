// Command ofpack packs the mise tools that are active in a directory into a
// single archive, so they can be carried to a disconnected machine and
// unpacked there without any network access.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"ofpack/internal/ofpack"
)

// version is overridable at build time: -ldflags "-X main.version=v1.2.3".
var version = "dev"

const usage = `ofpack - carry a mise toolchain to a disconnected machine

Usage:
  ofpack pack    [flags]            Pack global plus this directory and descendant mise scopes
  ofpack unpack  -f ARCHIVE [flags] Unpack an archive (no network needed)
  ofpack inspect -f ARCHIVE         Show what an archive contains
  ofpack env     --dest DIR         Print the shell setup for an unpacked tree
  ofpack direnv  init [flags]       Hook a directory up to its packed mise scope
  ofpack packages install          Install an archive's OS packages (explicit, never automatic)
  ofpack uv      restore            Build a packed Python project's .venv, offline
  ofpack version

Typical flow:
  # on the connected machine
  cd ~/work/myapp
  ofpack pack -o myapp.ofpack.tar.gz

  # one file to copy: an ofpack for the target travels inside it
  scp myapp.ofpack.tar.gz user@host:~/

  # on the disconnected machine, from your home directory
  mkdir -p /tmp/ofpack-boot
  tar -xzf myapp.ofpack.tar.gz -C /tmp/ofpack-boot bin/ofpack
  /tmp/ofpack-boot/bin/ofpack unpack -f myapp.ofpack.tar.gz
  source ~/work/myapp/.ofpack/activate.sh

Extracting into a temporary directory avoids overwriting a bin/ofpack of your
own; the same binary is installed under the destination for later use.

For automatic switching between projects, use direnv instead of sourcing by
hand (direnv is an optional prerequisite and is not bundled):
  cd ~/work/myapp
  ofpack direnv init
  direnv allow
Do not also run "mise activate" in the same shell.

A project outside your home directory has no default destination, so pass
--dest when unpacking it:
  /tmp/ofpack-boot/bin/ofpack unpack -f myapp.ofpack.tar.gz --dest /work/myapp/.ofpack

Targeting another OS needs no extra steps: the tools are installed in a
Podman container of that platform, and the bundled unpacker is built for it
too, so the archive is still the only file to copy:
  cd ~/work/myapp
  ofpack pack --target linux/amd64 -o myapp.linux-amd64.ofpack.tar.gz
  scp myapp.linux-amd64.ofpack.tar.gz user@host:~/

Plain pack includes active tools from global configuration, this directory,
and descendant mise scopes. Local configuration above this directory is
excluded. Different versions of one tool are carried separately.

For a Python project, --uv-project finds exactly one pyproject.toml below
this directory and carries Python, uv and wheels. Python selection is:
--python, then mise.toml from this directory to the Python project (nearest
wins), then the requires-python lower bound, then global mise Python. A
selector that conflicts with requires-python is rejected:
  ofpack pack --uv-project --python 3.12 --target linux/amd64 -o myapp.tar.gz

Run "ofpack <command> -h" for the flags of a command.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "pack":
		err = cmdPack(os.Args[2:])
	case "unpack":
		err = cmdUnpack(os.Args[2:])
	case "inspect":
		err = cmdInspect(os.Args[2:])
	case "env":
		err = cmdEnv(os.Args[2:])
	case "direnv":
		err = cmdDirenv(os.Args[2:])
	case "uv":
		err = cmdUV(os.Args[2:])
	case "packages":
		err = cmdPackages(os.Args[2:])
	case "version", "--version", "-v":
		fmt.Println("ofpack", version)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "ofpack: unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "ofpack:", err)
		os.Exit(1)
	}
}

type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

func cmdPack(args []string) error {
	fs := flag.NewFlagSet("pack", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), `Usage: ofpack pack [flags]

Packs every installed tool active in the global configuration, the selected
directory and its descendants. Local mise files above the selected directory
are excluded. Each directory's tool version selection is preserved, together
with mise and an ofpack for the target platform.

  cd /work/myapp && ofpack pack -o myapp.ofpack.tar.gz

Flags:
`)
		fs.PrintDefaults()
	}
	out := fs.String("o", "", "output archive (default \"<directory name>.ofpack.tar.gz\")")
	dir := fs.String("C", "", "selected root for tool scopes or the pyproject.toml search (default: cwd)")
	var exclude stringList
	fs.Var(&exclude, "exclude", "mise tool name to leave out (repeatable)")
	allowUnresolved := fs.Bool("allow-unresolved", false, "pack even if some symlinks cannot be relocated (they will be missing on the target)")
	dryRun := fs.Bool("dry-run", false, "report what would be packed without writing an archive")
	target := fs.String("target", "", "pack for another platform by installing the tools in a container, e.g. linux/amd64")
	image := fs.String("image", "", "container image the tools are installed in for --target (default "+ofpack.DefaultCrossImage+")")
	builderImage := fs.String("builder-image", "", "container image used to build the bundled unpacker when this machine has no Go (default "+ofpack.DefaultBuilderImage+")")
	engine := fs.String("engine", "podman", "container engine for --target")
	ofpackBin := fs.String("ofpack-bin", "", "use this prebuilt ofpack as the bundled unpacker instead of building one")
	noUnpacker := fs.Bool("no-unpacker", false, "do not bundle "+ofpack.UnpackerName+"; the target will need its own ofpack")
	packages := fs.Bool("packages", false, "also carry the operating system packages listed in "+ofpack.PackagesListName+" (read only when this flag is given)")
	var packageRepos packageRepoList
	fs.Var(&packageRepos, "package-repo", "repository id to resolve packages from, repeatable; required for dnf so the fetch cannot use whatever happens to be enabled")
	packageBase := fs.String("package-base", "", "container image to resolve apt packages in; needs --target, for example --target linux/amd64 --package-base docker.io/library/debian:bookworm-slim (or docker.io/library/ubuntu:24.04)")
	uvProject := fs.Bool("uv-project", false, "pack a uv Python project: the target's Python, uv and a wheel closure, instead of this machine's mise tools")
	pythonSpec := fs.String("python", "", "Python X.Y for --uv-project (overrides the project's own mise.toml; latest there uses the requires-python lower bound when present). Must satisfy requires-python")
	printCommand := fs.Bool("print-command", false, "with --target, print the tool-installation container command (which streams a payload, not a finished archive) instead of running it")
	if err := fs.Parse(args); err != nil {
		return err
	}

	workDir := *dir
	if workDir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return err
		}
		workDir = wd
	}
	workDir, err := filepath.Abs(workDir)
	if err != nil {
		return err
	}
	if fi, err := os.Stat(workDir); err != nil || !fi.IsDir() {
		return fmt.Errorf("%s is not a directory", workDir)
	}

	outName := *out
	if outName == "" {
		outName = filepath.Base(workDir) + ".ofpack.tar.gz"
	}

	pkgOpts := packageOptions{enabled: *packages, repos: packageRepos, base: *packageBase, engine: *engine, target: *target}
	// Checked before any work: resolving a closure in a base image says
	// nothing about which architecture it is for, and finding that out
	// after a download would waste the download.
	if *packageBase != "" {
		if !*packages {
			return fmt.Errorf("--package-base only applies with --packages")
		}
		if *target == "" {
			return fmt.Errorf("--package-base needs --target, for example --target linux/amd64:\nthe packages are for another machine, so the architecture has to be stated rather than taken from this one")
		}
	}
	if *packages && !*uvProject && !hasMiseProject(workDir) {
		// packages.txt and nothing else: an archive of operating system
		// packages alone, rather than a toolchain archive with none.
		return packagesOnlyPack(outName, workDir, pkgOpts, *ofpackBin, *builderImage, *engine, *noUnpacker, *dryRun)
	}
	if *uvProject {
		if *target == "" {
			return fmt.Errorf("--uv-project needs --target, for example --target linux/amd64:\nthe Python and the wheels have to be built for the machine that will use them")
		}
		return uvPack(*target, outName, workDir, *pythonSpec, *image, *builderImage, *engine, *ofpackBin, pkgOpts, *noUnpacker, *printCommand, *dryRun)
	}
	if *target != "" && *target != ofpack.HostPlatform() {
		return crossPack(*target, outName, workDir, *image, *builderImage, *engine, *ofpackBin,
			exclude, pkgOpts, *allowUnresolved, *noUnpacker, *printCommand, *dryRun)
	}

	fmt.Fprintf(os.Stderr, "Packing the mise tools active in %s\n", workDir)
	plan, err := ofpack.BuildPlan(ofpack.PackOptions{
		WorkDir:         workDir,
		Exclude:         exclude,
		AllowUnresolved: *allowUnresolved,
		Version:         version,
		Log:             os.Stderr,
	})
	if err != nil {
		return err
	}

	cleanupPkgs, err := attachPackagesTo(plan, workDir, pkgOpts)
	if err != nil {
		return err
	}
	defer cleanupPkgs()
	if err := attachUnpacker(plan, ofpack.HostTarget(), *ofpackBin, *builderImage, *engine, *noUnpacker); err != nil {
		return err
	}
	reportPlan(plan)
	if *dryRun {
		fmt.Fprintln(os.Stderr, "dry run: no archive written")
		return nil
	}

	archivePath, err := filepath.Abs(outName)
	if err != nil {
		return err
	}
	return writeArchive(plan, archivePath)
}

func crossPack(target, outName, workDir, image, builderImage, engine, ofpackBin string, exclude []string, pkgOpts packageOptions, allowUnresolved, noUnpacker, printOnly, dryRun bool) error {
	output, err := filepath.Abs(outName)
	if err != nil {
		return err
	}
	plan, err := ofpack.BuildCrossPlan(ofpack.CrossOptions{
		Target:          target,
		Image:           image,
		ProjectDir:      workDir,
		GlobalConfigDir: ofpack.DefaultGlobalConfigDir(),
		Output:          output,
		Engine:          engine,
		Exclude:         exclude,
		AllowUnresolved: allowUnresolved,
	})
	if err != nil {
		return err
	}
	if printOnly {
		// This is the tool-installation step only. It streams a payload, not
		// a finished archive: the archive is assembled here afterwards, from
		// that payload.
		fmt.Println("# Installs the tools for " + target + " and streams a payload on stdout.")
		fmt.Println("# This is one step of the pack, not the whole of it: ofpack builds the")
		fmt.Println("# archive from the payload afterwards. Printed for inspection.")
		fmt.Println(plan.CommandLine())
		return nil
	}

	fmt.Fprintf(os.Stderr, "Packing for %s in a container (host is %s).\nThe tools are installed inside the container, so the archive holds %s binaries.\n\n",
		target, ofpack.HostPlatform(), target)

	tmpDir, err := os.MkdirTemp(filepath.Dir(output), ".ofpack-payload-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)
	payload := filepath.Join(tmpDir, "payload.tar.gz")
	if err := ofpack.RunCross(plan, payload, os.Stderr); err != nil {
		return err
	}

	packPlan, err := ofpack.BuildPlanFromPayload(payload, target, ofpack.PackOptions{
		WorkDir:         workDir,
		Exclude:         exclude,
		AllowUnresolved: allowUnresolved,
		Version:         version,
		Log:             os.Stderr,
	})
	if err != nil {
		return err
	}
	cleanupPkgs, err := attachPackagesTo(packPlan, workDir, pkgOpts)
	if err != nil {
		return err
	}
	defer cleanupPkgs()
	if err := attachUnpacker(packPlan, target, ofpackBin, builderImage, engine, noUnpacker); err != nil {
		return err
	}
	reportPlan(packPlan)
	if dryRun {
		fmt.Fprintln(os.Stderr, "dry run: no archive written")
		return nil
	}
	return writeArchive(packPlan, output)
}

// attachUnpacker puts an ofpack for the archive's own platform inside it.
func attachUnpacker(plan *ofpack.Plan, target, ofpackBin, builderImage, engine string, noUnpacker bool) error {
	if noUnpacker {
		return nil
	}
	if ofpackBin != "" {
		data, err := os.ReadFile(ofpackBin)
		if err != nil {
			return fmt.Errorf("--ofpack-bin: %w", err)
		}
		// A binary that does not run on the target would break the one-file
		// bootstrap, so one supplied by hand is checked as closely as one
		// ofpack builds itself.
		if err := ofpack.VerifyUnpacker(data, target); err != nil {
			return fmt.Errorf("--ofpack-bin %s is not an executable for %s: %w", ofpackBin, target, err)
		}
		plan.SetUnpacker(data)
		return nil
	}
	if target == ofpack.HostTarget() {
		data, err := ofpack.SelfBinary()
		if err != nil {
			return fmt.Errorf("reading this ofpack binary to carry along: %w", err)
		}
		if err := ofpack.VerifyUnpacker(data, target); err != nil {
			return fmt.Errorf("this ofpack binary does not look like a %s executable: %w", target, err)
		}
		plan.SetUnpacker(data)
		return nil
	}
	data, err := ofpack.UnpackerBuilder{
		Source: embeddedSource(),
		Image:  builderImage,
		Engine: engine,
		Log:    os.Stderr,
	}.Build(target)
	if err != nil {
		return err
	}
	plan.SetUnpacker(data)
	return nil
}

// reportPlan prints the human summary of what is about to be archived.
func reportPlan(plan *ofpack.Plan) {
	m := plan.Manifest
	for _, t := range m.Tools {
		fmt.Fprintf(os.Stderr, "  %-28s %s\n", t.Name, t.Version)
	}
	fmt.Fprintf(os.Stderr, "  mise %s (%s/%s), %d files, %d dirs, %d symlinks, %s\n",
		m.MiseVersion, m.OS, m.Arch, m.Stats.Files, m.Stats.Dirs, m.Stats.Symlinks, humanBytes(m.Stats.Bytes))
	if plan.HasUnpacker() {
		fmt.Fprintf(os.Stderr, "  includes %s so the target needs only tar to bootstrap\n", ofpack.UnpackerName)
	}
	if n := len(m.DanglingLinks); n > 0 {
		fmt.Fprintf(os.Stderr, "  note: %d symlink(s) already dangle on this machine; recorded in the manifest\n", n)
	}
	if len(m.EnvVarNames) > 0 {
		fmt.Fprintf(os.Stderr, "  note: config sets %d env var(s) (%s); ofpack records only their names, never their values\n",
			len(m.EnvVarNames), strings.Join(m.EnvVarNames, ", "))
	}
}

// writeArchive writes the plan to target via a temporary file, so an
// interrupted pack never leaves a half-written archive that looks usable.
func writeArchive(plan *ofpack.Plan, target string) error {
	tmp, err := os.CreateTemp(filepath.Dir(target), ".ofpack-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		tmp.Close()
		os.Remove(tmpName)
	}()
	if err := plan.Write(tmp); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmpName, target); err != nil {
		return err
	}
	fi, err := os.Stat(target)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "Wrote %s (%s)\n", target, humanBytes(fi.Size()))
	return nil
}

func cmdUnpack(args []string) error {
	fs := flag.NewFlagSet("unpack", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), `Usage: ofpack unpack -f ARCHIVE [flags]

Unpacks an archive into a self contained mise data directory. No network
access is performed or required.

Archives carry an ofpack for their own platform at bin/ofpack, so on a machine
without ofpack you can bootstrap with tar alone:
  mkdir -p /tmp/ofpack-boot
  tar -xzf ARCHIVE -C /tmp/ofpack-boot bin/ofpack
  /tmp/ofpack-boot/bin/ofpack unpack -f ARCHIVE

With no --dest, the source project's home-relative path is reproduced under
your own home and the toolchain goes in its .ofpack subdirectory, so
/Users/alice/work/myapp becomes /home/bob/work/myapp/.ofpack:

  ofpack unpack -f myapp.ofpack.tar.gz
  source ~/work/myapp/.ofpack/activate.sh

Archives contain the mise environment only, never project source files, and
unpack writes nothing outside that .ofpack directory. If the project was packed
from outside the home directory, --dest is required.

Existing files are never replaced unless you ask for it: by default unpack
aborts and lists the conflicts.

Flags:
`)
		fs.PrintDefaults()
	}
	file := fs.String("f", "", "archive to unpack (required)")
	dest := fs.String("dest", "", "destination directory, becomes MISE_DATA_DIR (default: the source project's path under your home, plus /.ofpack)")
	force := fs.Bool("force", false, "replace existing paths (restored automatically if the unpack fails)")
	skipExisting := fs.Bool("skip-existing", false, "keep existing paths and unpack only what is missing")
	noArchCheck := fs.Bool("no-arch-check", false, "unpack even if the archive was built for another OS or CPU")
	dryRun := fs.Bool("dry-run", false, "report what would be installed without writing anything")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *file == "" {
		return fmt.Errorf("-f ARCHIVE is required")
	}
	if *force && *skipExisting {
		return fmt.Errorf("--force and --skip-existing are mutually exclusive")
	}
	policy := ofpack.OverwriteFail
	switch {
	case *force:
		policy = ofpack.OverwriteForce
	case *skipExisting:
		policy = ofpack.OverwriteSkip
	}
	f, err := os.Open(*file)
	if err != nil {
		return err
	}
	defer f.Close()

	absDest := *dest
	safeBase := ""
	if absDest == "" {
		// Reproduce the source project's home-relative path under this user's
		// home; the manifest is read first so the default can be computed.
		m, err := ofpack.ReadManifest(f)
		if err != nil {
			return err
		}
		absDest, safeBase, err = ofpack.DefaultDestWithBase(m)
		if err != nil {
			return err
		}
		if _, err := f.Seek(0, 0); err != nil {
			return err
		}
	}
	absDest, err = filepath.Abs(absDest)
	if err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "Unpacking %s into %s\n", *file, absDest)
	m, err := ofpack.Unpack(f, ofpack.UnpackOptions{
		Dest:       absDest,
		SafeBase:   safeBase,
		Policy:     policy,
		SkipArch:   *noArchCheck,
		DryRun:     *dryRun,
		Log:        os.Stderr,
		OfpackVers: version,
	})
	if err != nil {
		return err
	}
	if *dryRun {
		fmt.Fprintln(os.Stderr, "dry run: nothing written")
		return nil
	}
	if m.CarriesMise() {
		fmt.Fprintf(os.Stderr, "Installed mise %s and %d tool(s).\n\nNext:\n  source %s/activate.sh\n  mise ls --current\n",
			m.MiseVersion, len(m.Tools), absDest)
		if len(m.Scopes) > 1 {
			fmt.Fprintln(os.Stderr, "For child directories, run 'ofpack direnv init' there to select their packed versions.")
		}
	} else {
		// A package-only archive has no toolchain, so pointing at an
		// activate.sh that was deliberately not written would be a lie.
		fmt.Fprintf(os.Stderr, "Unpacked into %s. This archive carries no mise toolchain.\n", absDest)
	}
	if m.Packages != nil {
		fmt.Fprintf(os.Stderr, "\n%s", ofpack.PackagesSummary(m.Packages))
		fmt.Fprintf(os.Stderr, "They are on disk only. To install them:\n  sudo ofpack packages install --dest %s\n", absDest)
	}
	if len(m.EnvVarNames) > 0 {
		fmt.Fprintf(os.Stderr, "\nThe source configuration also set these env vars, which ofpack does not carry\n(their values may be secrets) - set them yourself if the tools need them:\n  %s\n",
			strings.Join(m.EnvVarNames, ", "))
	}
	return nil
}

func cmdInspect(args []string) error {
	fs := flag.NewFlagSet("inspect", flag.ExitOnError)
	file := fs.String("f", "", "archive to inspect (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *file == "" {
		return fmt.Errorf("-f ARCHIVE is required")
	}
	f, err := os.Open(*file)
	if err != nil {
		return err
	}
	defer f.Close()
	m, err := ofpack.ReadManifest(f)
	if err != nil {
		return err
	}
	fmt.Printf("packed:   %s by ofpack %s\n", m.CreatedAt, m.OfpackVersion)
	fmt.Printf("platform: %s/%s\n", m.OS, m.Arch)
	fmt.Printf("mise:     %s\n", m.MiseVersion)
	if m.HomeRelativeProjectPath != "" {
		fmt.Printf("project:  ~/%s\n", m.HomeRelativeProjectPath)
		if d, err := ofpack.DefaultDest(m); err == nil {
			fmt.Printf("default destination here: %s\n", d)
		}
	} else {
		fmt.Printf("project:  (packed from outside the home directory; --dest is required)\n")
	}
	fmt.Printf("payload:  %d files, %d dirs, %d symlinks, %s\n",
		m.Stats.Files, m.Stats.Dirs, m.Stats.Symlinks, humanBytes(m.Stats.Bytes))
	fmt.Printf("tools (%d):\n", len(m.Tools))
	for _, t := range m.Tools {
		fmt.Printf("  %-28s %-14s (%s)\n", t.Name, t.Version, t.RelPath)
	}
	if len(m.Scopes) > 0 {
		fmt.Printf("directory scopes (%d):\n", len(m.Scopes))
		for _, scope := range m.Scopes {
			fmt.Printf("  %s:", scope.Path)
			if len(scope.Tools) == 0 {
				fmt.Print(" (no tools)")
			}
			for _, tool := range scope.Tools {
				fmt.Printf(" %s@%s", tool.Name, tool.Version)
			}
			fmt.Println()
		}
	}
	if len(m.EnvVarNames) > 0 {
		fmt.Printf("env var names (values not carried): %s\n", strings.Join(m.EnvVarNames, ", "))
	}
	if m.Packages != nil {
		fmt.Print(ofpack.PackagesSummary(m.Packages))
	}
	report := func(title string, notes []ofpack.LinkNote) {
		if len(notes) == 0 {
			return
		}
		fmt.Printf("%s (%d):\n", title, len(notes))
		for _, n := range notes {
			fmt.Printf("  %s -> %s  [%s]\n", n.Path, n.Target, n.Reason)
		}
	}
	report("relocated symlinks", m.RewrittenLinks)
	report("dangling symlinks", m.DanglingLinks)
	report("omitted symlinks", m.UnresolvedLinks)
	if err := ofpack.CheckPlatform(m); err != nil {
		fmt.Printf("\nwarning: %v\n", err)
	}
	return nil
}

func cmdEnv(args []string) error {
	fs := flag.NewFlagSet("env", flag.ExitOnError)
	dest := fs.String("dest", "", "an unpacked ofpack directory (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *dest == "" {
		return fmt.Errorf("--dest DIR is required")
	}
	abs, err := filepath.Abs(*dest)
	if err != nil {
		return err
	}
	fmt.Print(ofpack.ActivateScript(abs))
	return nil
}

func cmdDirenv(args []string) error {
	if len(args) == 0 || args[0] != "init" {
		return fmt.Errorf("usage: ofpack direnv init [--dest DIR] [--project-root DIR]")
	}
	fs := flag.NewFlagSet("direnv init", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), `Usage: ofpack direnv init [--dest DIR] [--project-root DIR]

Hooks the project in the current directory up to direnv, so its toolchain is
on PATH inside the directory and gone again when you leave. Run it from the
project:

  cd ~/work/myapp
  ofpack direnv init
  direnv allow

direnv is an optional prerequisite; ofpack does not bundle it. Without it,
source the unpacked activate.sh by hand instead.

An existing .envrc is never modified: the line to add is printed instead.

Flags:
`)
		fs.PrintDefaults()
	}
	dest := fs.String("dest", "", "the unpacked ofpack directory (default: ./"+ofpack.OfpackSubdir+")")
	projectRoot := fs.String("project-root", "", "root directory originally packed, when a custom --dest is outside the project tree")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	plan, check, wrote, err := ofpack.InitDirenvWithRoot(cwd, *dest, *projectRoot)
	if err != nil {
		return err
	}
	fmt.Print(plan.Instructions(wrote))
	if report := check.Report(); report != "" {
		fmt.Fprintf(os.Stderr, "\n%s", report)
	}
	return nil
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// hasMiseProject reports whether the selected root or any descendant scope
// has mise tools to pack. Only when none of them does is a package-only
// archive the right answer.
//
// Looking at the root alone was wrong: a project with its tools in a child
// directory looked toolless, and the child's tools were silently dropped.
func hasMiseProject(dir string) bool {
	has, err := ofpack.AnyScopeHasTools(dir)
	if err != nil {
		// Unsure is not the same as "no tools": fall through to the normal
		// pack, which reports the real problem instead of quietly
		// producing an archive without them.
		return true
	}
	return has
}
