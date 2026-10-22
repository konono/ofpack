// Command mpack packs the mise tools that are active in a directory into a
// single archive, so they can be carried to a disconnected machine and
// unpacked there without any network access.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"mpack/internal/mpack"
)

// version is overridable at build time: -ldflags "-X main.version=v1.2.3".
var version = "dev"

const usage = `mpack - carry a mise toolchain to a disconnected machine

Usage:
  mpack pack    [flags]            Pack the mise tools active in this directory
  mpack unpack  -f ARCHIVE [flags] Unpack an archive (no network needed)
  mpack inspect -f ARCHIVE         Show what an archive contains
  mpack env     --dest DIR         Print the shell setup for an unpacked tree
  mpack version

Typical flow:
  # on the connected machine
  cd ~/work/myapp
  mpack pack -o myapp.mpack.tar.gz

  # one file to copy: an mpack for the target travels inside it
  scp myapp.mpack.tar.gz user@host:~/

  # on the disconnected machine, from your home directory
  mkdir -p /tmp/mpack-boot
  tar -xzf myapp.mpack.tar.gz -C /tmp/mpack-boot bin/mpack
  /tmp/mpack-boot/bin/mpack unpack -f myapp.mpack.tar.gz
  source ~/work/myapp/.mpack/activate.sh

Extracting into a temporary directory avoids overwriting a bin/mpack of your
own; the same binary is installed under the destination for later use.

A project outside your home directory has no default destination, so pass
--dest when unpacking it:
  /tmp/mpack-boot/bin/mpack unpack -f myapp.mpack.tar.gz --dest /work/myapp/.mpack

Targeting another OS needs no extra steps: the tools are installed in a
Podman container of that platform, and the bundled unpacker is built for it
too, so the archive is still the only file to copy:
  cd ~/work/myapp
  mpack pack --target linux/amd64 -o myapp.linux-amd64.mpack.tar.gz
  scp myapp.linux-amd64.mpack.tar.gz user@host:~/

Run "mpack <command> -h" for the flags of a command.
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
	case "version", "--version", "-v":
		fmt.Println("mpack", version)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "mpack: unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "mpack:", err)
		os.Exit(1)
	}
}

type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

func cmdPack(args []string) error {
	fs := flag.NewFlagSet("pack", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), `Usage: mpack pack [flags]

Packs every mise tool that is installed and active in the working directory,
i.e. the merged result of your global and local mise configuration, together
with the mise binary itself and an mpack for the target platform.

  cd /work/myapp && mpack pack -o myapp.mpack.tar.gz

Flags:
`)
		fs.PrintDefaults()
	}
	out := fs.String("o", "", "output archive (default \"<directory name>.mpack.tar.gz\")")
	dir := fs.String("C", "", "pack the tools active in this directory instead of the current one")
	var exclude stringList
	fs.Var(&exclude, "exclude", "mise tool name to leave out (repeatable)")
	allowUnresolved := fs.Bool("allow-unresolved", false, "pack even if some symlinks cannot be relocated (they will be missing on the target)")
	dryRun := fs.Bool("dry-run", false, "report what would be packed without writing an archive")
	target := fs.String("target", "", "pack for another platform by installing the tools in a container, e.g. linux/amd64")
	image := fs.String("image", "", "container image the tools are installed in for --target (default "+mpack.DefaultCrossImage+")")
	builderImage := fs.String("builder-image", "", "container image used to build the bundled unpacker when this machine has no Go (default "+mpack.DefaultBuilderImage+")")
	engine := fs.String("engine", "podman", "container engine for --target")
	mpackBin := fs.String("mpack-bin", "", "use this prebuilt mpack as the bundled unpacker instead of building one")
	noUnpacker := fs.Bool("no-unpacker", false, "do not bundle "+mpack.UnpackerName+"; the target will need its own mpack")
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
		outName = filepath.Base(workDir) + ".mpack.tar.gz"
	}

	if *target != "" && *target != mpack.HostPlatform() {
		return crossPack(*target, outName, workDir, *image, *builderImage, *engine, *mpackBin,
			exclude, *allowUnresolved, *noUnpacker, *printCommand, *dryRun)
	}

	fmt.Fprintf(os.Stderr, "Packing the mise tools active in %s\n", workDir)
	plan, err := mpack.BuildPlan(mpack.PackOptions{
		WorkDir:         workDir,
		Exclude:         exclude,
		AllowUnresolved: *allowUnresolved,
		Version:         version,
		Log:             os.Stderr,
	})
	if err != nil {
		return err
	}

	if err := attachUnpacker(plan, mpack.HostTarget(), *mpackBin, *builderImage, *engine, *noUnpacker); err != nil {
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

func crossPack(target, outName, workDir, image, builderImage, engine, mpackBin string, exclude []string, allowUnresolved, noUnpacker, printOnly, dryRun bool) error {
	output, err := filepath.Abs(outName)
	if err != nil {
		return err
	}
	plan, err := mpack.BuildCrossPlan(mpack.CrossOptions{
		Target:          target,
		Image:           image,
		ProjectDir:      workDir,
		GlobalConfigDir: mpack.DefaultGlobalConfigDir(),
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
		fmt.Println("# This is one step of the pack, not the whole of it: mpack builds the")
		fmt.Println("# archive from the payload afterwards. Printed for inspection.")
		fmt.Println(plan.CommandLine())
		return nil
	}

	fmt.Fprintf(os.Stderr, "Packing for %s in a container (host is %s).\nThe tools are installed inside the container, so the archive holds %s binaries.\n\n",
		target, mpack.HostPlatform(), target)

	tmpDir, err := os.MkdirTemp(filepath.Dir(output), ".mpack-payload-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)
	payload := filepath.Join(tmpDir, "payload.tar.gz")
	if err := mpack.RunCross(plan, payload, os.Stderr); err != nil {
		return err
	}

	packPlan, err := mpack.BuildPlanFromPayload(payload, target, mpack.PackOptions{
		WorkDir:         workDir,
		Exclude:         exclude,
		AllowUnresolved: allowUnresolved,
		Version:         version,
		Log:             os.Stderr,
	})
	if err != nil {
		return err
	}
	if err := attachUnpacker(packPlan, target, mpackBin, builderImage, engine, noUnpacker); err != nil {
		return err
	}
	reportPlan(packPlan)
	if dryRun {
		fmt.Fprintln(os.Stderr, "dry run: no archive written")
		return nil
	}
	return writeArchive(packPlan, output)
}

// attachUnpacker puts an mpack for the archive's own platform inside it.
func attachUnpacker(plan *mpack.Plan, target, mpackBin, builderImage, engine string, noUnpacker bool) error {
	if noUnpacker {
		return nil
	}
	if mpackBin != "" {
		data, err := os.ReadFile(mpackBin)
		if err != nil {
			return fmt.Errorf("--mpack-bin: %w", err)
		}
		// A binary that does not run on the target would break the one-file
		// bootstrap, so one supplied by hand is checked as closely as one
		// mpack builds itself.
		if err := mpack.VerifyUnpacker(data, target); err != nil {
			return fmt.Errorf("--mpack-bin %s is not an executable for %s: %w", mpackBin, target, err)
		}
		plan.SetUnpacker(data)
		return nil
	}
	if target == mpack.HostTarget() {
		data, err := mpack.SelfBinary()
		if err != nil {
			return fmt.Errorf("reading this mpack binary to carry along: %w", err)
		}
		if err := mpack.VerifyUnpacker(data, target); err != nil {
			return fmt.Errorf("this mpack binary does not look like a %s executable: %w", target, err)
		}
		plan.SetUnpacker(data)
		return nil
	}
	data, err := mpack.UnpackerBuilder{
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
func reportPlan(plan *mpack.Plan) {
	m := plan.Manifest
	for _, t := range m.Tools {
		fmt.Fprintf(os.Stderr, "  %-28s %s\n", t.Name, t.Version)
	}
	fmt.Fprintf(os.Stderr, "  mise %s (%s/%s), %d files, %d dirs, %d symlinks, %s\n",
		m.MiseVersion, m.OS, m.Arch, m.Stats.Files, m.Stats.Dirs, m.Stats.Symlinks, humanBytes(m.Stats.Bytes))
	if plan.HasUnpacker() {
		fmt.Fprintf(os.Stderr, "  includes %s so the target needs only tar to bootstrap\n", mpack.UnpackerName)
	}
	if n := len(m.DanglingLinks); n > 0 {
		fmt.Fprintf(os.Stderr, "  note: %d symlink(s) already dangle on this machine; recorded in the manifest\n", n)
	}
	if len(m.EnvVarNames) > 0 {
		fmt.Fprintf(os.Stderr, "  note: config sets %d env var(s) (%s); mpack records only their names, never their values\n",
			len(m.EnvVarNames), strings.Join(m.EnvVarNames, ", "))
	}
}

// writeArchive writes the plan to target via a temporary file, so an
// interrupted pack never leaves a half-written archive that looks usable.
func writeArchive(plan *mpack.Plan, target string) error {
	tmp, err := os.CreateTemp(filepath.Dir(target), ".mpack-*.tmp")
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
		fmt.Fprint(fs.Output(), `Usage: mpack unpack -f ARCHIVE [flags]

Unpacks an archive into a self contained mise data directory. No network
access is performed or required.

Archives carry an mpack for their own platform at bin/mpack, so on a machine
without mpack you can bootstrap with tar alone:
  mkdir -p /tmp/mpack-boot
  tar -xzf ARCHIVE -C /tmp/mpack-boot bin/mpack
  /tmp/mpack-boot/bin/mpack unpack -f ARCHIVE

With no --dest, the source project's home-relative path is reproduced under
your own home and the toolchain goes in its .mpack subdirectory, so
/Users/alice/work/myapp becomes /home/bob/work/myapp/.mpack:

  mpack unpack -f myapp.mpack.tar.gz
  source ~/work/myapp/.mpack/activate.sh

Archives contain the mise environment only, never project source files, and
unpack writes nothing outside that .mpack directory. If the project was packed
from outside the home directory, --dest is required.

Existing files are never replaced unless you ask for it: by default unpack
aborts and lists the conflicts.

Flags:
`)
		fs.PrintDefaults()
	}
	file := fs.String("f", "", "archive to unpack (required)")
	dest := fs.String("dest", "", "destination directory, becomes MISE_DATA_DIR (default: the source project's path under your home, plus /.mpack)")
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
	policy := mpack.OverwriteFail
	switch {
	case *force:
		policy = mpack.OverwriteForce
	case *skipExisting:
		policy = mpack.OverwriteSkip
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
		m, err := mpack.ReadManifest(f)
		if err != nil {
			return err
		}
		absDest, safeBase, err = mpack.DefaultDestWithBase(m)
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
	m, err := mpack.Unpack(f, mpack.UnpackOptions{
		Dest:      absDest,
		SafeBase:  safeBase,
		Policy:    policy,
		SkipArch:  *noArchCheck,
		DryRun:    *dryRun,
		Log:       os.Stderr,
		MpackVers: version,
	})
	if err != nil {
		return err
	}
	if *dryRun {
		fmt.Fprintln(os.Stderr, "dry run: nothing written")
		return nil
	}
	fmt.Fprintf(os.Stderr, "Installed mise %s and %d tool(s).\n\nNext:\n  source %s/activate.sh\n  mise ls --current\n",
		m.MiseVersion, len(m.Tools), absDest)
	if len(m.EnvVarNames) > 0 {
		fmt.Fprintf(os.Stderr, "\nThe source configuration also set these env vars, which mpack does not carry\n(their values may be secrets) - set them yourself if the tools need them:\n  %s\n",
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
	m, err := mpack.ReadManifest(f)
	if err != nil {
		return err
	}
	fmt.Printf("packed:   %s by mpack %s\n", m.CreatedAt, m.MpackVersion)
	fmt.Printf("platform: %s/%s\n", m.OS, m.Arch)
	fmt.Printf("mise:     %s\n", m.MiseVersion)
	if m.HomeRelativeProjectPath != "" {
		fmt.Printf("project:  ~/%s\n", m.HomeRelativeProjectPath)
		if d, err := mpack.DefaultDest(m); err == nil {
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
	if len(m.EnvVarNames) > 0 {
		fmt.Printf("env var names (values not carried): %s\n", strings.Join(m.EnvVarNames, ", "))
	}
	report := func(title string, notes []mpack.LinkNote) {
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
	if err := mpack.CheckPlatform(m); err != nil {
		fmt.Printf("\nwarning: %v\n", err)
	}
	return nil
}

func cmdEnv(args []string) error {
	fs := flag.NewFlagSet("env", flag.ExitOnError)
	dest := fs.String("dest", "", "an unpacked mpack directory (required)")
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
	fmt.Print(mpack.ActivateScript(abs))
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
