package mpack

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strings"
)

// DefaultCrossImage is the base image the cross-platform pack runs in. Debian
// stable carries a widely compatible glibc plus the shared libraries most mise
// tools expect, and it is small enough to pull quickly.
const DefaultCrossImage = "docker.io/library/debian:bookworm-slim"

// SupportedTargets are the platforms a cross pack can produce. Each one is a
// real Linux platform; mpack cannot build a macOS toolchain from Linux or vice
// versa, because that needs the target OS to run the installers.
var SupportedTargets = []string{"linux/amd64", "linux/arm64"}

// CrossOptions describes a cross-platform pack: mise runs inside a container
// of the target platform, installs the target's native binaries and packs them
// there, so no host (for example macOS) binary ever enters the archive.
type CrossOptions struct {
	// Target is a "<os>/<arch>" platform from SupportedTargets.
	Target string
	// Image is the container base image; empty means DefaultCrossImage.
	Image string
	// ProjectDir is the host directory whose mise config drives the install.
	ProjectDir string
	// GlobalConfigDir is the host mise config directory to merge in. Empty
	// means do not merge a global config.
	GlobalConfigDir string
	// Unpacker, when non-empty, is an mpack binary for Target to archive as
	// bin/mpack. Packing itself never needs it: that happens on this machine.
	Unpacker []byte
	// Output is the host path of the archive to produce.
	Output string
	// Engine is the container CLI to use; empty means "podman".
	Engine string
	// Exclude is passed through to the in-container pack.
	Exclude []string
	// AllowUnresolved is passed through to the in-container pack.
	AllowUnresolved bool
	// UV, when set, switches to packing a Python project: the container
	// installs only a Python and a uv for the target and downloads the
	// project's wheels, instead of mirroring this machine's mise setup.
	UV *UVSource
	// PythonSpec is the Python version for UV mode.
	PythonSpec string
}

// CrossPlan is a fully resolved cross pack, ready to run or to print.
type CrossPlan struct {
	Engine string
	Args   []string
	Script string
	// Output is the host path the streamed archive is written to.
	Output     string
	OutputName string
}

// ResolveTarget validates a platform string.
func ResolveTarget(target string) (goos, goarch string, err error) {
	for _, s := range SupportedTargets {
		if s == target {
			os, arch, _ := strings.Cut(s, "/")
			return os, arch, nil
		}
	}
	return "", "", fmt.Errorf("unsupported --target %q (supported: %s)", target, strings.Join(SupportedTargets, ", "))
}

// DefaultGlobalConfigDir returns the host mise global config directory.
func DefaultGlobalConfigDir() string {
	if v := os.Getenv("MISE_CONFIG_DIR"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "mise")
}

// configMount is one host mise config file copied into the container, at the
// container path that mirrors its host location.
type configMount struct {
	// Host is the absolute path of the file on this machine.
	Host string
	// ContainerPath is the absolute path it is copied to in the container.
	ContainerPath string
	// MountPath is the read-only path it is mounted at inside the container.
	MountPath string
}

// containerPathFor mirrors a host directory inside the container: under the
// home directory it is rebased onto the container home (so the archive keeps a
// usable default destination), and elsewhere the absolute path is reproduced
// as is.
func containerPathFor(hostDir, home, containerHome string) string {
	if home != "" {
		if rel, ok := RelUnder(filepath.Clean(home), filepath.Clean(hostDir)); ok {
			if rel == "." {
				return containerHome
			}
			return path.Join(containerHome, rel)
		}
	}
	return filepath.ToSlash(filepath.Clean(hostDir))
}

// collectProjectConfigs is retained for older callers and tests. New scoped
// packs use collectScopedConfigs, which never includes ancestors of the root.
func collectProjectConfigs(projectDir, home, containerHome string) []configMount {
	projectDir = filepath.Clean(projectDir)
	stop := ""
	if home != "" {
		if _, ok := RelUnder(filepath.Clean(home), projectDir); ok {
			stop = filepath.Clean(home)
		}
	}
	var dirs []string
	for d := projectDir; ; d = filepath.Dir(d) {
		dirs = append(dirs, d)
		if d == stop || filepath.Dir(d) == d {
			break
		}
	}
	// Walk outermost first, so a nearer config is copied last and wins.
	var mounts []configMount
	for n := len(dirs) - 1; n >= 0; n-- {
		dir := dirs[n]
		for _, name := range miseConfigFiles {
			host := filepath.Join(dir, filepath.FromSlash(name))
			if fi, err := os.Lstat(host); err != nil || !fi.Mode().IsRegular() {
				continue
			}
			mounts = append(mounts, configMount{
				Host:          host,
				ContainerPath: path.Join(containerPathFor(dir, home, containerHome), name),
				MountPath:     fmt.Sprintf("/host/mise-config/%d", len(mounts)),
			})
		}
	}
	return mounts
}

func collectScopedConfigs(scopes []string, home, containerHome string) []configMount {
	var mounts []configMount
	for _, dir := range scopes {
		for _, name := range miseConfigFiles {
			host := filepath.Join(dir, filepath.FromSlash(name))
			if fi, err := os.Lstat(host); err != nil || !fi.Mode().IsRegular() {
				continue
			}
			mounts = append(mounts, configMount{Host: host, ContainerPath: path.Join(containerPathFor(dir, home, containerHome), name), MountPath: fmt.Sprintf("/host/mise-config/%d", len(mounts))})
		}
	}
	return mounts
}

// miseConfigFiles are the per-directory files mise reads. Only these are
// copied into the container: the archive carries a toolchain, never project
// sources, and the host directories are mounted read only regardless.
var miseConfigFiles = []string{
	"mise.toml", ".mise.toml",
	"mise.local.toml", ".mise.local.toml",
	".tool-versions",
	"mise/config.toml", ".mise/config.toml",
	".config/mise.toml", ".config/mise/config.toml",
}

// miseConfigPrecedence is miseConfigFiles ordered the way mise resolves them
// within one directory, highest priority first. Measured with
// `mise config ls` rather than assumed: reading them in the wrong order
// would pick a version the directory does not actually use.
//
// .tool-versions is last and is not TOML; it needs its own reader.
var miseConfigPrecedence = []string{
	".mise.local.toml",
	"mise.local.toml",
	".mise.toml",
	"mise.toml",
	"mise/config.toml",
	".mise/config.toml",
	".config/mise.toml",
	".config/mise/config.toml",
	".tool-versions",
}

// init keeps the two lists in step. A config file mpack copies into the
// container but never consults when choosing Python would be a silent gap,
// which is the defect this ordering was added to close.
func init() {
	if len(miseConfigPrecedence) != len(miseConfigFiles) {
		panic("miseConfigPrecedence and miseConfigFiles disagree on how many config files mise reads")
	}
	known := make(map[string]bool, len(miseConfigFiles))
	for _, name := range miseConfigFiles {
		known[name] = true
	}
	for _, name := range miseConfigPrecedence {
		if !known[name] {
			panic("miseConfigPrecedence lists " + name + ", which is not in miseConfigFiles")
		}
	}
}

// BuildCrossPlan assembles the container invocation without running anything,
// so it can be printed and reviewed.
func BuildCrossPlan(opts CrossOptions) (*CrossPlan, error) {
	if _, _, err := ResolveTarget(opts.Target); err != nil {
		return nil, err
	}
	engine := opts.Engine
	if engine == "" {
		engine = "podman"
	}
	image := opts.Image
	if image == "" {
		image = DefaultCrossImage
	}
	project, err := filepath.Abs(opts.ProjectDir)
	if err != nil {
		return nil, err
	}
	output, err := filepath.Abs(opts.Output)
	if err != nil {
		return nil, err
	}
	// Mirror the project's home-relative path inside the container, so the
	// archive records the same home_relative_project_path a native pack would
	// and unpack can still pick its default destination.
	containerHome := "/root"
	home, err := os.UserHomeDir()
	if err != nil {
		home = ""
	} else if abs, err := filepath.Abs(home); err == nil {
		home = filepath.Clean(abs)
	}
	containerProject := containerPathFor(project, home, containerHome)
	var configs []configMount
	var scopePaths []string
	if opts.UV == nil {
		scopes, err := DiscoverMiseScopes(project)
		if err != nil {
			return nil, err
		}
		configs = collectScopedConfigs(scopes, home, containerHome)
		for _, dir := range scopes {
			if strings.ContainsAny(dir, "\n\r") {
				return nil, fmt.Errorf("mise scope path contains a newline: %q", dir)
			}
			scopePaths = append(scopePaths, containerPathFor(dir, home, containerHome))
		}
	}

	// The payload is streamed out over stdout rather than written to a
	// mounted directory: bind mounts are not reliably writable from the
	// container across podman setups (rootless uid mapping, macOS virtiofs),
	// and a stream has no such failure mode.
	args := []string{
		"run", "--rm", "-i", "--platform", opts.Target,
		"-e", "HOME=" + containerHome,
		"-e", "MPACK_CONTAINER_PROJECT=" + containerProject,
	}
	hasGlobal := false
	if opts.UV == nil && opts.GlobalConfigDir != "" {
		if fi, err := os.Stat(opts.GlobalConfigDir); err == nil && fi.IsDir() {
			abs, err := filepath.Abs(opts.GlobalConfigDir)
			if err != nil {
				return nil, err
			}
			args = append(args, "-v", abs+":/host/global-config:ro")
			hasGlobal = true
		}
	}
	// A project can rely entirely on the global config, so only a complete
	// absence of configuration is an error.
	if opts.UV == nil && len(configs) == 0 && !hasGlobal {
		return nil, fmt.Errorf("no mise configuration found within %s or in the global config dir", project)
	}
	for _, c := range configs {
		args = append(args, "-v", c.Host+":"+c.MountPath+":ro")
	}
	// A uv pack writes its own mise configuration inside the container,
	// listing only Python and uv, so nothing of this machine's setup - and
	// none of the unrelocatable symlinks in it - comes along.
	script := scopedCrossScript(opts, hasGlobal, configs, scopePaths)
	if opts.UV != nil {
		script = UVBuildScript(opts.UV, opts.PythonSpec)
	}
	args = append(args, image, "sh", "-euc", script)

	return &CrossPlan{
		Engine:     engine,
		Args:       args,
		Script:     script,
		Output:     output,
		OutputName: filepath.Base(output),
	}, nil
}

// crossScript is what runs inside the container: install mise, install the
// project's tools for the target platform, then pack them with the target
// mpack. Mounts are read only, so the config is copied to writable locations
// before mise touches it.
func crossScript(opts CrossOptions, hasGlobal bool, configs []configMount) string {
	return scopedCrossScript(opts, hasGlobal, configs, []string{containerPathFor(opts.ProjectDir, "", "/root")})
}

func scopedCrossScript(opts CrossOptions, hasGlobal bool, configs []configMount, scopes []string) string {
	var b strings.Builder
	b.WriteString(`export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y -qq --no-install-recommends \
  ca-certificates curl git xz-utils unzip bzip2 zstd tar gzip >/dev/null

export MISE_DATA_DIR="$HOME/.local/share/mise"
export MISE_CONFIG_DIR="$HOME/.config/mise"
mkdir -p "$MISE_DATA_DIR" "$MISE_CONFIG_DIR" "$MPACK_CONTAINER_PROJECT"
`)
	if hasGlobal {
		b.WriteString(`cp -R /host/global-config/. "$MISE_CONFIG_DIR"/
`)
	}
	b.WriteString("# Copy only mise configs from the selected project tree.\n")
	for _, c := range configs {
		fmt.Fprintf(&b, "mkdir -p %s && cp %s %s\n",
			shellQuoteIfNeeded(path.Dir(c.ContainerPath)), shellQuoteIfNeeded(c.MountPath), shellQuoteIfNeeded(c.ContainerPath))
	}
	b.WriteString(`
curl -fsSL https://mise.run | MISE_INSTALL_PATH=/usr/local/bin/mise sh >/dev/null
`)
	for _, dir := range scopes {
		fmt.Fprintf(&b, "cd %s\necho %s >&2\nmise install --yes >&2\n", shellQuote(dir), shellQuote("--- installing scope "+dir+" ---"))
	}
	b.WriteString("cd \"$MPACK_CONTAINER_PROJECT\"\necho '--- collecting ---' >&2\n")
	// Everything above logs to stderr so stdout carries only the payload.
	// mise installs the project's tools, then the payload script streams the
	// result out. mpack is not involved inside the container at all;
	// --exclude and --allow-unresolved are applied on this machine, against
	// the payload.
	b.WriteString(ScopedPayloadScript(scopes))
	return b.String()
}

// RunCross executes the plan, writing the container's log output to logs and
// the payload it streams on stdout to payloadPath.
func RunCross(plan *CrossPlan, payloadPath string, logs io.Writer) error {
	if _, err := exec.LookPath(plan.Engine); err != nil {
		return fmt.Errorf("%s is not installed, and packing for another platform needs it: the tools have to be installed on that platform first. --print-command shows that container step, but it only streams a payload for inspection, so it is not a way to produce an archive without %s", plan.Engine, plan.Engine)
	}
	out, err := os.OpenFile(payloadPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer out.Close()

	cmd := exec.Command(plan.Engine, plan.Args...)
	cmd.Stdout = out
	cmd.Stderr = logs
	cmd.Stdin = nil
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s run failed: %w", plan.Engine, err)
	}
	if err := out.Close(); err != nil {
		return err
	}
	fi, err := os.Stat(payloadPath)
	if err != nil {
		return err
	}
	if fi.Size() == 0 {
		return fmt.Errorf("the container produced no payload")
	}
	return nil
}

// CommandLine renders the plan as a copy-pasteable shell command.
func (p *CrossPlan) CommandLine() string {
	parts := make([]string, 0, len(p.Args)+1)
	parts = append(parts, p.Engine)
	for _, a := range p.Args {
		parts = append(parts, shellQuoteIfNeeded(a))
	}
	return strings.Join(parts, " ")
}

// shellQuoteIfNeeded quotes s only when it contains something a shell would
// interpret, keeping generated commands readable.
func shellQuoteIfNeeded(s string) string {
	if s == "" {
		return "''"
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case strings.ContainsRune("@%+=:,./-_", r):
		default:
			return shellQuote(s)
		}
	}
	return s
}

// HostPlatform is the platform string of the machine mpack is running on.
func HostPlatform() string { return runtime.GOOS + "/" + runtime.GOARCH }
