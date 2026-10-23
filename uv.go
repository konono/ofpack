package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"mpack/internal/mpack"
)

// uvPack packs a uv Python project: the target's Python and uv, plus every
// wheel its dependencies need, so the environment can be built on a machine
// with no network. Neither the project tree nor the .venv on this machine
// goes into the archive - a .venv holds host-native binaries and absolute
// paths, and would not work on the other side even if it were copied.
func uvPack(target, outName, projectDir, pythonSpec, image, builderImage, engine, mpackBin string,
	noUnpacker, printOnly, dryRun bool) error {

	source, err := mpack.ReadUVProject(projectDir)
	if err != nil {
		return err
	}
	requires := mpack.RequiresPython(source)
	if pythonSpec == "" {
		pythonSpec, err = mpack.PythonSpecFor(source)
		if err != nil {
			return err
		}
	} else if ok, why := mpack.PythonSatisfies(pythonSpec, requires); !ok {
		// A version chosen by hand must still be one the project accepts,
		// or the archive would carry an interpreter it cannot run on.
		return fmt.Errorf("--python %s conflicts with requires-python = %q in %s: %s",
			pythonSpec, requires, "pyproject.toml", why)
	}

	fmt.Fprintf(os.Stderr, "Packing the Python environment for %s, built for %s.\n", source.Dir, target)
	fmt.Fprintf(os.Stderr, "  python %s, dependencies from %s:\n", pythonSpec, "pyproject.toml")
	for _, r := range source.Requirements {
		fmt.Fprintf(os.Stderr, "    %s\n", r)
	}
	if source.HasLock {
		fmt.Fprintf(os.Stderr, "  uv.lock pins %d package(s) exactly\n", len(source.Constraints))
	} else {
		fmt.Fprintf(os.Stderr, "  no uv.lock; versions will be resolved at pack time and frozen into the wheels\n")
	}
	// requirements.txt listing fewer packages than pyproject is a real trap:
	// installing from it would leave Ansible out entirely.
	if missing := source.MissingFromRequirementsTxt(); len(missing) > 0 {
		fmt.Fprintf(os.Stderr, "  note: requirements.txt does not mention %s; packing what pyproject.toml declares\n",
			strings.Join(missing, ", "))
	}
	fmt.Fprintf(os.Stderr, "  the project's own files are not packed, and only pyproject.toml, uv.lock and requirements.txt were read\n\n")

	output, err := filepath.Abs(outName)
	if err != nil {
		return err
	}
	plan, err := mpack.BuildCrossPlan(mpack.CrossOptions{
		Target:     target,
		Image:      image,
		ProjectDir: projectDir,
		Output:     output,
		Engine:     engine,
		UV:         source,
		PythonSpec: pythonSpec,
	})
	if err != nil {
		return err
	}
	if printOnly {
		fmt.Println("# Installs a Python and uv for " + target + ", downloads the project's wheels,")
		fmt.Println("# and streams a payload on stdout. mpack builds the archive from it afterwards.")
		fmt.Println(plan.CommandLine())
		return nil
	}

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
		WorkDir: projectDir,
		Version: version,
		Log:     os.Stderr,
	})
	if err != nil {
		return err
	}
	uvRecord, err := mpack.AttachUVProject(packPlan, source, payload)
	if err != nil {
		return err
	}

	// An archive that cannot actually be restored is the failure worth
	// catching here rather than on the disconnected machine.
	fmt.Fprintf(os.Stderr, "\n--- checking the closure installs with no network ---\n")
	how, err := mpack.VerifyUVPayloadOffline(payload, engine, image, target, os.Stderr)
	if err != nil {
		return fmt.Errorf("the wheel closure did not install offline, so the archive would not restore: %w", err)
	}
	uvRecord.VerifiedOffline = how

	if err := attachUnpacker(packPlan, target, mpackBin, builderImage, engine, noUnpacker); err != nil {
		return err
	}
	reportPlan(packPlan)
	fmt.Fprintf(os.Stderr, "  %d wheels for python %s, verified offline (%s)\n", uvRecord.Wheels, uvRecord.PythonVersion, how)
	if dryRun {
		fmt.Fprintln(os.Stderr, "dry run: no archive written")
		return nil
	}
	if err := writeArchive(packPlan, output); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "\nOn the disconnected machine:\n"+
		"  tar -xzf %s -C /tmp/mpack-boot bin/mpack\n"+
		"  /tmp/mpack-boot/bin/mpack unpack -f %s\n"+
		"  cd <project> && /tmp/mpack-boot/bin/mpack uv restore\n",
		filepath.Base(output), filepath.Base(output))
	return nil
}

func cmdUV(args []string) error {
	if len(args) == 0 || args[0] != "restore" {
		return fmt.Errorf("usage: mpack uv restore [--dest DIR] [--force|--skip-existing]")
	}
	fs := flag.NewFlagSet("uv restore", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), `Usage: mpack uv restore [flags]

Builds the project's .venv from the wheels in an unpacked archive, with no
network access. Run it from the project directory:

  cd ~/work/myapp
  mpack uv restore

Nothing outside .venv is written, and an existing .venv is never replaced
unless you ask for it. With --force the new environment is built and checked
first, and only then swapped in; if anything fails the old one stays.

Flags:
`)
		fs.PrintDefaults()
	}
	dest := fs.String("dest", "", "the unpacked mpack directory (default: ./"+mpack.MpackSubdir+")")
	force := fs.Bool("force", false, "replace an existing .venv, keeping it until the new one is built and checked")
	skipExisting := fs.Bool("skip-existing", false, "leave an existing .venv completely alone")
	if err := fs.Parse(args[1:]); err != nil {
		return err
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
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	venv, built, err := mpack.RestoreVenv(mpack.RestoreOptions{
		Project: cwd, Dest: *dest, Policy: policy, Log: os.Stderr,
	})
	if err != nil {
		return err
	}
	if !built {
		fmt.Fprintf(os.Stderr, "Left %s as it was.\n", venv)
		return nil
	}
	fmt.Fprintf(os.Stderr, "Built %s\n\nUse it with:\n  source %s/bin/activate\n", venv, venv)
	return nil
}
