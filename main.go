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

  # copy the archive, plus an mpack binary that runs on the target machine
  # (the same one here; for a different OS see --target below)
  scp myapp.mpack.tar.gz mpack user@host:~/

  # on the disconnected machine, from your home directory
  ./mpack unpack -f myapp.mpack.tar.gz          # lands in ~/work/myapp/.mpack
  source ~/work/myapp/.mpack/activate.sh

A project outside your home directory has no default destination, so pass
--dest when unpacking it:
  ./mpack unpack -f myapp.mpack.tar.gz --dest /work/myapp/.mpack

Targeting another OS (the tools are installed in a Podman container of that
platform, so the archive holds its binaries, not this machine's). The pack
runs inside the container, so build an mpack for the target first, and send
that one across rather than this binary:
  cd /path/to/mpack && GOOS=linux GOARCH=amd64 go build -o mpack-linux-amd64 .
  cd ~/work/myapp
  mpack pack --target linux/amd64 \
    --mpack-bin /path/to/mpack/mpack-linux-amd64 \
    -o myapp.linux-amd64.mpack.tar.gz
  scp myapp.linux-amd64.mpack.tar.gz \
    /path/to/mpack/mpack-linux-amd64 user@host:~/
  ssh user@host './mpack-linux-amd64 unpack -f myapp.linux-amd64.mpack.tar.gz'
  # then on that machine: source ~/work/myapp/.mpack/activate.sh

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
with the mise binary itself.

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
	image := fs.String("image", "", "container image for --target (default "+mpack.DefaultCrossImage+")")
	engine := fs.String("engine", "podman", "container engine for --target")
	mpackBin := fs.String("mpack-bin", "", "mpack binary built for --target (default: ./mpack-<os>-<arch>)")
	printCommand := fs.Bool("print-command", false, "with --target, print the container command instead of running it")
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
		return crossPack(*target, outName, workDir, *image, *engine, *mpackBin, exclude, *allowUnresolved, *printCommand, *dryRun)
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

	m := plan.Manifest
	for _, t := range m.Tools {
		fmt.Fprintf(os.Stderr, "  %-28s %s\n", t.Name, t.Version)
	}
	fmt.Fprintf(os.Stderr, "  mise %s (%s/%s), %d files, %d dirs, %d symlinks, %s\n",
		m.MiseVersion, m.OS, m.Arch, m.Stats.Files, m.Stats.Dirs, m.Stats.Symlinks, humanBytes(m.Stats.Bytes))
	if n := len(m.DanglingLinks); n > 0 {
		fmt.Fprintf(os.Stderr, "  note: %d symlink(s) already dangle on this machine; recorded in the manifest\n", n)
	}
	if len(m.EnvVarNames) > 0 {
		fmt.Fprintf(os.Stderr, "  note: config sets %d env var(s) (%s); mpack records only their names, never their values\n",
			len(m.EnvVarNames), strings.Join(m.EnvVarNames, ", "))
	}
	if *dryRun {
		fmt.Fprintln(os.Stderr, "dry run: no archive written")
		return nil
	}

	archivePath, err := filepath.Abs(outName)
	if err != nil {
		return err
	}
	// Write to a temporary file and rename, so an interrupted pack never
	// leaves a half-written archive that looks usable.
	tmp, err := os.CreateTemp(filepath.Dir(archivePath), ".mpack-*.tmp")
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
	if err := os.Rename(tmpName, archivePath); err != nil {
		return err
	}
	fi, err := os.Stat(archivePath)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "Wrote %s (%s)\n", archivePath, humanBytes(fi.Size()))
	return nil
}

// crossPack packs for a different platform by installing the project's tools
// inside a container of that platform. The host's own tool binaries are never
// archived: everything in the result is built for the target.
func crossPack(target, outName, workDir, image, engine, mpackBin string, exclude []string, allowUnresolved, printOnly, dryRun bool) error {
	bin, err := mpack.FindCrossMpack(mpackBin, target)
	if err != nil {
		return err
	}
	output, err := filepath.Abs(outName)
	if err != nil {
		return err
	}
	plan, err := mpack.BuildCrossPlan(mpack.CrossOptions{
		Target:          target,
		Image:           image,
		ProjectDir:      workDir,
		GlobalConfigDir: mpack.DefaultGlobalConfigDir(),
		MpackBin:        bin,
		Output:          output,
		Engine:          engine,
		Exclude:         exclude,
		AllowUnresolved: allowUnresolved,
	})
	if err != nil {
		return err
	}
	if printOnly || dryRun {
		fmt.Println(plan.CommandLine())
		return nil
	}
	fmt.Fprintf(os.Stderr, "Packing for %s in a container (host is %s).\nThe tools are installed inside the container, so the archive holds %s binaries.\n\n",
		target, mpack.HostPlatform(), target)
	if err := mpack.RunCross(plan, os.Stderr); err != nil {
		return err
	}
	fi, err := os.Stat(output)
	if err != nil {
		return fmt.Errorf("the container finished but produced no archive at %s: %w", output, err)
	}
	fmt.Fprintf(os.Stderr, "\nWrote %s (%s)\nCheck it with: mpack inspect -f %s\n", output, humanBytes(fi.Size()), output)
	return nil
}

func cmdUnpack(args []string) error {
	fs := flag.NewFlagSet("unpack", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), `Usage: mpack unpack -f ARCHIVE [flags]

Unpacks an archive into a self contained mise data directory. No network
access is performed or required.

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
