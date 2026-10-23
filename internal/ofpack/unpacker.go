package ofpack

import (
	"archive/tar"
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// DefaultBuilderImage is used to build the target-platform ofpack when this
// machine has no usable Go toolchain. The build itself runs on the host
// architecture and cross compiles, because the Go runtime is not reliable
// under foreign-architecture emulation.
//
// The tag is deliberately newer than the go directive in go.mod, for the
// reason given at UnpackerGoVersion.
const DefaultBuilderImage = "docker.io/library/golang:1.25-bookworm"

// UnpackerGoVersion is the oldest Go that may build a bundled unpacker, by
// either route - the local toolchain or the builder image.
//
// It is above go.mod's directive on purpose. A binary built by go1.24
// segfaults in runtime.netpoll during unpack when it is run under an emulated
// amd64, which is how a cross-built archive usually gets tried out first.
// go1.25 is the oldest version whose output was verified to unpack correctly
// there; go1.24 has not been shown to be safe, so it is not used even when it
// is the toolchain sitting on PATH. Compiling the source itself still only
// needs go1.24.
const UnpackerGoVersion = "go1.25"

// minVerifiedBuilderMinor guards DefaultBuilderImage against being lowered
// back to a toolchain whose output is known to crash.
const minVerifiedBuilderMinor = 25

// UnpackerBuilder produces an ofpack binary for another platform from the Go
// source embedded in this one, so neither a source checkout nor a Go
// toolchain is required to carry an unpacker along with an archive.
type UnpackerBuilder struct {
	// Source is the embedded Go source of this program.
	Source fs.FS
	// Image is the fallback container image; empty means DefaultBuilderImage.
	Image string
	// Engine is the container CLI; empty means "podman".
	Engine string
	// Log receives progress.
	Log io.Writer
}

// Build returns a static ofpack binary for target ("linux/amd64"). It prefers
// a Go toolchain on this machine - cross compiling is instant and needs no
// image - and falls back to a container when there is none.
func (b UnpackerBuilder) Build(target string) ([]byte, error) {
	goos, goarch, err := ResolveTarget(target)
	if err != nil {
		return nil, err
	}
	if b.Source == nil {
		return nil, fmt.Errorf("no embedded source available to build an unpacker from")
	}

	var bin []byte
	if hostGoUsable() {
		logf(b.Log, "building the %s unpacker with the local Go toolchain\n", target)
		bin, err = b.buildWithHostGo(goos, goarch)
	} else {
		image := b.Image
		if image == "" {
			image = DefaultBuilderImage
		}
		logf(b.Log, "no usable Go toolchain here; building the %s unpacker in %s\n", target, image)
		bin, err = b.buildInContainer(goos, goarch, image)
	}
	if err != nil {
		return nil, err
	}
	if err := VerifyUnpacker(bin, target); err != nil {
		return nil, fmt.Errorf("the unpacker that was built is not usable: %w", err)
	}
	return bin, nil
}

// VerifyUnpacker checks that a binary really is an executable for target,
// whatever its provenance. A bundled unpacker that does not run on the target
// breaks the one-file bootstrap, so a caller-supplied binary is checked just
// as closely as one ofpack builds itself.
func VerifyUnpacker(bin []byte, target string) error {
	goos, goarch, err := ResolveTargetOrHost(target)
	if err != nil {
		return err
	}
	switch goos {
	case "linux":
		return verifyELF(bin, goos, goarch)
	case "darwin":
		return verifyMachO(bin, goarch)
	default:
		return fmt.Errorf("cannot verify an executable for %s", target)
	}
}

// ResolveTargetOrHost accepts the platforms a cross pack supports plus this
// machine's own, which is what a native pack bundles for.
func ResolveTargetOrHost(target string) (goos, goarch string, err error) {
	if target == HostTarget() {
		return runtime.GOOS, runtime.GOARCH, nil
	}
	return ResolveTarget(target)
}

// machoCPUTypes maps GOARCH to the Mach-O cpu_type we expect.
var machoCPUTypes = map[string]uint32{
	"arm64": 0x0100000c, // CPU_TYPE_ARM64
	"amd64": 0x01000007, // CPU_TYPE_X86_64
}

// verifyMachO is the macOS counterpart of verifyELF, so that a --ofpack-bin
// given for a native archive is checked too.
func verifyMachO(bin []byte, goarch string) error {
	if len(bin) < 8 {
		return fmt.Errorf("too short to be an executable (%d bytes)", len(bin))
	}
	magic := binary.LittleEndian.Uint32(bin[:4])
	if magic != 0xfeedfacf {
		if magic == 0xbebafeca || magic == 0xcafebabe {
			return fmt.Errorf("universal (fat) binaries are not accepted; supply a single-architecture executable")
		}
		return fmt.Errorf("not a 64-bit Mach-O file")
	}
	want, ok := machoCPUTypes[goarch]
	if !ok {
		return fmt.Errorf("no expected cpu type known for %s", goarch)
	}
	if got := binary.LittleEndian.Uint32(bin[4:8]); got != want {
		return fmt.Errorf("Mach-O cpu type is 0x%08x, want 0x%08x for %s", got, want, goarch)
	}
	return nil
}

// hostGoUsable reports whether this machine has a Go toolchain new enough for
// the embedded source.
func hostGoUsable() bool {
	exe, err := exec.LookPath("go")
	if err != nil {
		return false
	}
	out, err := exec.Command(exe, "env", "GOVERSION").Output()
	if err != nil {
		return false
	}
	return goVersionAtLeast(strings.TrimSpace(string(out)), UnpackerGoVersion)
}

// goVersionAtLeast compares "go1.24.3" style versions on major and minor.
func goVersionAtLeast(have, want string) bool {
	hMaj, hMin, ok := parseGoVersion(have)
	if !ok {
		return false
	}
	wMaj, wMin, ok := parseGoVersion(want)
	if !ok {
		return false
	}
	return hMaj > wMaj || (hMaj == wMaj && hMin >= wMin)
}

func parseGoVersion(v string) (major, minor int, ok bool) {
	v = strings.TrimPrefix(strings.TrimSpace(v), "go")
	parts := strings.Split(v, ".")
	if len(parts) < 2 {
		return 0, 0, false
	}
	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, false
	}
	minor, err = strconv.Atoi(strings.TrimSuffix(parts[1], "rc"))
	if err != nil {
		// Trailing text such as "1.24rc1"; take the leading digits.
		digits := strings.TrimLeftFunc(parts[1], func(r rune) bool { return false })
		minor, err = strconv.Atoi(strings.TrimRightFunc(digits, func(r rune) bool { return r < '0' || r > '9' }))
		if err != nil {
			return 0, 0, false
		}
	}
	return major, minor, true
}

// writeSource materialises the embedded source into dir, skipping tests.
func (b UnpackerBuilder) writeSource(dir string) error {
	return fs.WalkDir(b.Source, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if strings.HasSuffix(name, "_test.go") {
			return nil
		}
		data, err := fs.ReadFile(b.Source, name)
		if err != nil {
			return err
		}
		out := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
			return err
		}
		return os.WriteFile(out, data, 0o644)
	})
}

// buildFlags are shared by both build paths: static, reproducible, offline.
func buildFlags(output string) []string {
	return []string{"build", "-trimpath", "-ldflags", "-s -w", "-o", output, "."}
}

func buildEnv(goos, goarch, gocache, gopath string) []string {
	return []string{
		"GOOS=" + goos, "GOARCH=" + goarch,
		"CGO_ENABLED=0", // static, so it runs on any glibc
		"GOPROXY=off",   // the standard library is all it needs
		"GOFLAGS=-mod=mod",
		"GOCACHE=" + gocache,
		"GOPATH=" + gopath,
		"HOME=" + gopath,
		"PATH=" + os.Getenv("PATH"),
	}
}

func (b UnpackerBuilder) buildWithHostGo(goos, goarch string) ([]byte, error) {
	dir, err := os.MkdirTemp("", "ofpack-build-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)

	src := filepath.Join(dir, "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		return nil, err
	}
	if err := b.writeSource(src); err != nil {
		return nil, err
	}
	out := filepath.Join(dir, "ofpack")
	cmd := exec.Command("go", buildFlags(out)...)
	cmd.Dir = src
	cmd.Env = buildEnv(goos, goarch, filepath.Join(dir, "cache"), filepath.Join(dir, "gopath"))
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("go build for %s/%s: %w: %s", goos, goarch, err, strings.TrimSpace(stderr.String()))
	}
	return os.ReadFile(out)
}

// buildInContainer compiles in a container of *this* machine's architecture
// and cross compiles to the target. Running a foreign-architecture Go
// toolchain under emulation crashes (the runtime's pointer packing assumes
// 48-bit addresses), so the container platform is deliberately the host's.
func (b UnpackerBuilder) buildInContainer(goos, goarch, image string) ([]byte, error) {
	engine := b.Engine
	if engine == "" {
		engine = "podman"
	}
	if _, err := exec.LookPath(engine); err != nil {
		return nil, fmt.Errorf("bundling an unpacker needs either a Go toolchain (%s or newer) or %s, and neither is available; alternatively pass --ofpack-bin with a binary already built for the target, or --no-unpacker to leave it out", UnpackerGoVersion, engine)
	}

	var srcTar bytes.Buffer
	if err := b.sourceTar(&srcTar); err != nil {
		return nil, err
	}
	args := []string{
		"run", "--rm", "-i",
		"--platform", hostContainerPlatform(),
		"-e", "GOOS=" + goos,
		"-e", "GOARCH=" + goarch,
		"-e", "CGO_ENABLED=0",
		"-e", "GOPROXY=off",
		image,
		"sh", "-euc",
		"mkdir -p /src && cd /src && tar -x && " +
			"go build -trimpath -ldflags '-s -w' -o /tmp/ofpack . >&2 && cat /tmp/ofpack",
	}
	cmd := exec.Command(engine, args...)
	cmd.Stdin = &srcTar
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("building the unpacker in %s: %w: %s", image, err, strings.TrimSpace(stderr.String()))
	}
	if out.Len() == 0 {
		return nil, fmt.Errorf("the builder container produced no binary: %s", strings.TrimSpace(stderr.String()))
	}
	return out.Bytes(), nil
}

// sourceTar writes the embedded source as a tar stream for the builder.
func (b UnpackerBuilder) sourceTar(w io.Writer) error {
	tw := tar.NewWriter(w)
	err := fs.WalkDir(b.Source, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || strings.HasSuffix(name, "_test.go") {
			return err
		}
		data, err := fs.ReadFile(b.Source, name)
		if err != nil {
			return err
		}
		hdr := &tar.Header{
			Typeflag: tar.TypeReg,
			Name:     name,
			Mode:     0o644,
			Size:     int64(len(data)),
			Format:   tar.FormatPAX,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		_, err = tw.Write(data)
		return err
	})
	if err != nil {
		return err
	}
	return tw.Close()
}

// elfMachines maps GOARCH to the ELF e_machine value we expect to see.
var elfMachines = map[string]uint16{
	"amd64": 0x3e, // EM_X86_64
	"arm64": 0xb7, // EM_AARCH64
}

// verifyELF checks that a built binary really is for the platform we asked
// for, rather than trusting the build environment to have honoured GOOS and
// GOARCH.
func verifyELF(bin []byte, goos, goarch string) error {
	if goos != "linux" {
		return fmt.Errorf("only linux targets can be verified, got %q", goos)
	}
	if len(bin) < 20 {
		return fmt.Errorf("too short to be an executable (%d bytes)", len(bin))
	}
	if !bytes.Equal(bin[:4], []byte{0x7f, 'E', 'L', 'F'}) {
		return fmt.Errorf("not an ELF file")
	}
	if bin[4] != 2 {
		return fmt.Errorf("not a 64-bit ELF file")
	}
	if bin[5] != 1 {
		return fmt.Errorf("not a little-endian ELF file")
	}
	want, ok := elfMachines[goarch]
	if !ok {
		return fmt.Errorf("no expected machine type known for %s", goarch)
	}
	// e_machine is a 16-bit little-endian field at offset 18.
	if got := binary.LittleEndian.Uint16(bin[18:20]); got != want {
		return fmt.Errorf("ELF machine is 0x%02x, want 0x%02x for %s", got, want, goarch)
	}
	return nil
}

// SelfBinary returns the running ofpack executable, for use as the bootstrap
// unpacker of a same-platform archive.
func SelfBinary() ([]byte, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(exe)
}

// HostTarget is this machine's platform in "<os>/<arch>" form.
func HostTarget() string { return runtime.GOOS + "/" + runtime.GOARCH }

// hostContainerPlatform is the platform of a container that runs natively
// here. Containers are Linux whatever the host OS is, so only the
// architecture carries over - and it must, because a Go toolchain emulating a
// foreign architecture crashes rather than building.
func hostContainerPlatform() string { return "linux/" + runtime.GOARCH }

// SourceComplete checks that an embedded source tree holds every Go file of
// the package it was taken from. Embedding a list of names meant a new file
// could be left out, and the omission only surfaced when the unpacker build
// failed; this makes it a test failure instead.
func SourceComplete(src fs.FS, want []string) []string {
	var missing []string
	for _, name := range want {
		if _, err := fs.Stat(src, name); err != nil {
			missing = append(missing, name)
		}
	}
	return missing
}
