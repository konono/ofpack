package ofpack

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// FetchOptions describes a package fetch.
type FetchOptions struct {
	// Names are the packages asked for, already validated.
	Names []string
	// Dest is where the package files are written.
	Dest string
	// Repos are the repository identifiers to use. Nothing else is
	// consulted: "whatever happens to be enabled" is not a reproducible
	// input, and a broken third-party repository should not be able to fail
	// or redirect the fetch.
	Repos []string
	// BaseImage is the container image an apt closure is resolved in.
	BaseImage string
	// Engine is the container CLI for apt fetches.
	Engine string
	// Platform is the "<os>/<arch>" these packages are for. An apt closure
	// is resolved in a container of that platform; a native dnf fetch
	// refuses to run unless it matches this machine, since the RPMs it
	// downloads are for the architecture it runs on.
	Platform string
	Log      io.Writer
}

// FetchPackages resolves the dependency closure and downloads it. The
// package names are passed as arguments, never interpolated into a shell
// command: they are the one piece of this that comes from a file.
func FetchPackages(manager string, host OSRelease, opts FetchOptions) (*Packages, error) {
	if err := os.MkdirAll(opts.Dest, 0o755); err != nil {
		return nil, err
	}
	switch manager {
	case ManagerDNF:
		return fetchWithDNF(host, opts)
	case ManagerAPT:
		return fetchWithAPT(opts)
	default:
		return nil, fmt.Errorf("unknown package manager %q", manager)
	}
}

// fetchWithDNF downloads an RPM closure on this machine. It has to run on a
// host of the target distribution with access to its repositories - on RHEL
// that means a registered system - because the entitlement and the
// repository metadata live there.
func fetchWithDNF(host OSRelease, opts FetchOptions) (*Packages, error) {
	// dnf downloads for the architecture it runs on. Packing an archive for
	// another platform while fetching RPMs here would mix architectures in
	// one archive, and the mismatch would only show up at install time.
	if opts.Platform != "" && opts.Platform != HostPlatform() {
		return nil, fmt.Errorf(`cannot fetch RPMs for %s on a %s machine.

dnf resolves against this system's own repositories and downloads for its own architecture, so the packages would not match the rest of the archive. Run the pack on a %s machine, or leave --target off to pack for this one.`,
			opts.Platform, HostPlatform(), opts.Platform)
	}
	if len(opts.Repos) == 0 {
		return nil, fmt.Errorf("--package-repo is required: ofpack will not fetch from whatever repositories happen to be enabled.\n%s", availableRepoHint())
	}
	args := []string{
		"download", "--resolve", "--alldeps",
		"--destdir", opts.Dest,
		// Only the repositories that were named. A broken or unexpected
		// third-party repository cannot then fail the fetch or supply a
		// package.
		"--disablerepo", "*",
		// GPG keys are never imported on the fetch's say-so; an unknown key
		// has to be a visible failure, not a prompt answered for the user.
		"--setopt", "localpkg_gpgcheck=1",
		"--assumeno",
	}
	for _, repo := range opts.Repos {
		args = append(args, "--enablerepo", repo)
	}
	args = append(args, opts.Names...)

	logf(opts.Log, "  dnf download --resolve --alldeps from %s\n", strings.Join(opts.Repos, ", "))
	if err := runPackageTool(opts.Log, "dnf", args...); err != nil {
		return nil, fmt.Errorf("%w\n\nThe repositories used were: %s.\nIf one of them is unavailable or its signing key is missing, drop it from --package-repo or install the key first; ofpack does not import keys by itself",
			err, strings.Join(opts.Repos, ", "))
	}
	files, err := CollectPackageFiles(opts.Dest, ".rpm")
	if err != nil {
		return nil, err
	}
	return &Packages{
		Manager:   ManagerDNF,
		OS:        host,
		Repos:     opts.Repos,
		Requested: opts.Names,
		Closure:   "dnf --resolve --alldeps",
		Files:     files,
	}, nil
}

func availableRepoHint() string {
	out, err := exec.Command("dnf", "repolist", "--quiet").Output()
	if err != nil {
		return "Run `dnf repolist` to see the repository identifiers available here."
	}
	return "Repositories available here:\n" + strings.TrimRight(string(out), "\n")
}

// aptFetchScript resolves and downloads a .deb closure inside a container of
// the target distribution.
//
// The package names are not in this script. They are read from a file the
// caller mounts, so nothing from packages.txt is ever part of a command
// string - the script is a fixed text.
//
// Dir::State::status points at an empty file so apt plans as though nothing
// were installed. Without it apt downloads only what the container happens
// to be missing, and a target whose base image differs would be short of
// exactly the packages the builder already had.
const aptFetchScript = `
set -eu
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq >&2
: > /tmp/ofpack-empty-status
mkdir -p /out/partial
# xargs -a passes each line as one argument; the names never reach a shell.
xargs -a /ofpack/packages.txt -r -d '\n' -- \
  apt-get install -y --download-only \
    -o Dir::State::status=/tmp/ofpack-empty-status \
    -o Dir::Cache::archives=/out >&2
rm -rf /out/partial /out/lock
count=$(ls /out/*.deb 2>/dev/null | wc -l)
echo "downloaded $count package files" >&2
if [ "$count" -eq 0 ]; then
  echo "ofpack: no package files were downloaded" >&2
  exit 1
fi
`

// fetchWithAPT resolves a .deb closure in a container of the target
// distribution.
func fetchWithAPT(opts FetchOptions) (*Packages, error) {
	if opts.BaseImage == "" {
		return nil, fmt.Errorf(`--package-base is required for apt packages: a .deb closure is only as complete as the image it was resolved in, so the image has to be named rather than guessed.

For example, with the target it has to be paired with:
  --target linux/amd64 --package-base docker.io/library/debian:bookworm-slim
  --target linux/amd64 --package-base docker.io/library/ubuntu:24.04`)
	}
	engine := opts.Engine
	if engine == "" {
		engine = "podman"
	}
	if _, err := exec.LookPath(engine); err != nil {
		return nil, fmt.Errorf("%s is required to resolve apt packages for %s: %w", engine, opts.BaseImage, err)
	}

	listDir, err := os.MkdirTemp("", "ofpack-pkglist-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(listDir)
	listPath := filepath.Join(listDir, PackagesListName)
	if err := os.WriteFile(listPath, []byte(strings.Join(opts.Names, "\n")+"\n"), 0o644); err != nil {
		return nil, err
	}

	logf(opts.Log, "  apt-get install --download-only in %s, with an empty package status\n", opts.BaseImage)
	args := []string{
		"run", "--rm", "-i",
	}
	if opts.Platform != "" {
		args = append(args, "--platform", opts.Platform)
	}
	args = append(args,
		// The :z labels matter on SELinux hosts such as RHEL, where an
		// unlabelled bind mount is simply not readable or writable from
		// inside the container.
		"-v", listDir+":/ofpack:ro,z",
		"-v", opts.Dest+":/out:z",
		opts.BaseImage, "sh", "-euc", aptFetchScript,
	)
	if err := runPackageTool(opts.Log, engine, args...); err != nil {
		return nil, fmt.Errorf("resolving apt packages in %s: %w", opts.BaseImage, err)
	}

	files, err := CollectPackageFiles(opts.Dest, ".deb")
	if err != nil {
		return nil, err
	}
	os, err := aptImageOSRelease(engine, opts.BaseImage, opts.Platform)
	if err != nil {
		return nil, err
	}
	return &Packages{
		Manager:   ManagerAPT,
		OS:        os,
		BaseImage: opts.BaseImage,
		Requested: opts.Names,
		Closure:   "apt-get --download-only with an empty dpkg status",
		// apt treats Essential packages as always present and does not
		// include them even with an empty status, so the closure covers
		// declared dependencies only. Recorded rather than left implied.
		EssentialExcluded: true,
		Files:             files,
	}, nil
}

// aptImageOSRelease asks the base image which release it is, so the archive
// records what the closure was actually resolved against.
func aptImageOSRelease(engine, image, platform string) (OSRelease, error) {
	args := []string{"run", "--rm"}
	if platform != "" {
		args = append(args, "--platform", platform)
	}
	args = append(args, image, "cat", "/etc/os-release")
	out, err := exec.Command(engine, args...).Output()
	if err != nil {
		return OSRelease{}, fmt.Errorf("reading /etc/os-release from %s: %w", image, withStderr(err))
	}
	o := parseOSRelease(string(out))
	if o.ID == "" {
		return OSRelease{}, fmt.Errorf("%s reports no ID in /etc/os-release", image)
	}
	return o, nil
}

// runPackageTool runs a package manager with its arguments as arguments.
// Nothing is handed to a shell, so a package name cannot become anything but
// a package name.
func runPackageTool(log io.Writer, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout, cmd.Stderr = log, log
	cmd.Stdin = nil
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s failed: %w", name, err)
	}
	return nil
}
