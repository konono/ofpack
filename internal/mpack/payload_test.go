package mpack

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// buildPayload writes a synthetic container payload: the metadata, a mise
// binary and a set of install entries rooted at a container path.
func buildPayload(t *testing.T, containerRoot string, tools map[string][]string, entries []payloadFixture) string {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	now := time.Unix(0, 0).UTC()

	listed := map[string][]map[string]any{}
	var binPaths []string
	for name, versions := range tools {
		for _, v := range versions {
			listed[name] = append(listed[name], map[string]any{
				"version": v, "requested_version": v,
				"install_path": containerRoot + "/" + name + "/" + v,
				"installed":    true, "active": true,
			})
			binPaths = append(binPaths, containerRoot+"/"+name+"/"+v+"/bin")
		}
	}
	toolsJSON, err := json.Marshal(listed)
	if err != nil {
		t.Fatal(err)
	}
	// A directory entry comes first, as GNU tar emits.
	if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: payloadMetaDir + "/", Mode: 0o755, ModTime: now}); err != nil {
		t.Fatal(err)
	}
	meta := map[string]string{
		payloadTools:       string(toolsJSON),
		payloadBinPaths:    strings.Join(binPaths, "\n"),
		payloadEnvNames:    "DATABASE_URL\nGITHUB_TOKEN\n",
		payloadMiseVersion: "2026.10.17",
		payloadRoot:        containerRoot,
	}
	for _, name := range []string{payloadTools, payloadBinPaths, payloadEnvNames, payloadMiseVersion, payloadRoot} {
		if err := writeBytes(tw, name, 0o644, []byte(meta[name]), now); err != nil {
			t.Fatal(err)
		}
	}
	if err := writeBytes(tw, MiseBinName, 0o755, []byte("#!/bin/sh\n"), now); err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		hdr := &tar.Header{Name: path.Join(InstallsPrefix, e.rel), Mode: int64(e.mode), ModTime: now, Format: tar.FormatPAX}
		switch {
		case e.link != "":
			hdr.Typeflag, hdr.Linkname = tar.TypeSymlink, e.link
			if err := tw.WriteHeader(hdr); err != nil {
				t.Fatal(err)
			}
		case e.dir:
			hdr.Typeflag, hdr.Name = tar.TypeDir, hdr.Name+"/"
			if err := tw.WriteHeader(hdr); err != nil {
				t.Fatal(err)
			}
		default:
			hdr.Typeflag, hdr.Size = tar.TypeReg, int64(len(e.body))
			if err := tw.WriteHeader(hdr); err != nil {
				t.Fatal(err)
			}
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "payload.tar.gz")
	if err := os.WriteFile(p, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

type payloadFixture struct {
	rel  string
	body string
	link string
	dir  bool
	mode os.FileMode
}

// The whole point of the payload design: an archive for another platform is
// built here, from files that were installed in a container, with the
// symlinks resolved against the container's paths rather than this machine's.
func TestBuildPlanFromPayloadRoundTrip(t *testing.T) {
	const root = "/root/.local/share/mise/installs"
	payload := buildPayload(t, root, map[string][]string{"jq": {"1.8.1"}, "node": {"22.1.0"}}, []payloadFixture{
		{rel: "jq/1.8.1", dir: true, mode: 0o755},
		{rel: "jq/1.8.1/jq", body: "binary", mode: 0o755},
		{rel: "node/22.1.0", dir: true, mode: 0o755},
		{rel: "node/22.1.0/README", body: "readme", mode: 0o644},
		// A relative link, and an absolute one into the container's root.
		{rel: "node/22.1.0/here", link: "README", mode: 0o777},
		{rel: "node/22.1.0/jq", link: root + "/jq/1.8.1/jq", mode: 0o777},
	})

	home := t.TempDir()
	t.Setenv("HOME", home)
	project := filepath.Join(home, "work", "myapp")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}

	target := "linux/amd64"
	plan, err := BuildPlanFromPayload(payload, target, PackOptions{WorkDir: project, Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Manifest.OS != "linux" || plan.Manifest.Arch != "amd64" {
		t.Fatalf("manifest platform = %s/%s, want the target's", plan.Manifest.OS, plan.Manifest.Arch)
	}
	if plan.Manifest.HomeRelativeProjectPath != "work/myapp" {
		t.Fatalf("project path = %q", plan.Manifest.HomeRelativeProjectPath)
	}
	if len(plan.Manifest.RewrittenLinks) != 1 {
		t.Fatalf("the absolute container link should be rewritten: %+v", plan.Manifest.RewrittenLinks)
	}
	plan.SetUnpacker([]byte("pretend-binary"))

	var archive bytes.Buffer
	if err := plan.Write(&archive); err != nil {
		t.Fatal(err)
	}

	// Unpacking has to be forced past the platform check: the archive is for
	// linux and the test may not be.
	dest := filepath.Join(t.TempDir(), "out")
	m, err := Unpack(bytes.NewReader(archive.Bytes()), UnpackOptions{Dest: dest, SkipArch: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Tools) != 2 {
		t.Fatalf("got %d tools", len(m.Tools))
	}
	if body, err := os.ReadFile(filepath.Join(dest, "installs/jq/1.8.1/jq")); err != nil || string(body) != "binary" {
		t.Fatalf("file content lost: %q %v", body, err)
	}
	if fi, err := os.Stat(filepath.Join(dest, "installs/jq/1.8.1/jq")); err != nil || fi.Mode().Perm()&0o111 == 0 {
		t.Fatalf("executable bit lost: %v %v", fi, err)
	}
	// The container-absolute link now points into the unpacked tree.
	got, err := os.Readlink(filepath.Join(dest, "installs/node/22.1.0/jq"))
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dest, "installs/jq/1.8.1/jq"); got != want {
		t.Fatalf("relocated link = %q, want %q", got, want)
	}
	if got, err := os.Readlink(filepath.Join(dest, "installs/node/22.1.0/here")); err != nil || got != "README" {
		t.Fatalf("relative link = %q, %v", got, err)
	}
	// The bundled unpacker is installed and executable.
	bin := filepath.Join(dest, filepath.FromSlash(UnpackerName))
	fi, err := os.Stat(bin)
	if err != nil || fi.Mode().Perm()&0o111 == 0 {
		t.Fatalf("%s missing or not executable: %v", UnpackerName, err)
	}
}

// A link that leaves the container's packed set must fail the pack just as it
// does on this machine.
func TestBuildPlanFromPayloadRejectsEscapingLink(t *testing.T) {
	const root = "/root/.local/share/mise/installs"
	payload := buildPayload(t, root, map[string][]string{"rust": {"1.0.0"}}, []payloadFixture{
		{rel: "rust/1.0.0", dir: true, mode: 0o755},
		{rel: "rust/1.0.0/cargo", link: "/root/.cargo/bin", mode: 0o777},
	})
	_, err := BuildPlanFromPayload(payload, "linux/amd64", PackOptions{WorkDir: t.TempDir(), Version: "t"})
	if err == nil || !strings.Contains(err.Error(), "outside the mise installs root") {
		t.Fatalf("expected the escaping link to fail the pack, got %v", err)
	}
	if _, err := BuildPlanFromPayload(payload, "linux/amd64", PackOptions{WorkDir: t.TempDir(), Version: "t", AllowUnresolved: true}); err != nil {
		t.Fatalf("--allow-unresolved should still pack: %v", err)
	}
}

// Multi-version precedence has to survive the container boundary too.
func TestBuildPlanFromPayloadKeepsVersionPrecedence(t *testing.T) {
	const root = "/root/.local/share/mise/installs"
	payload := buildPayload(t, root, map[string][]string{"node": {"24.16.0", "22.22.2"}}, []payloadFixture{
		{rel: "node/24.16.0", dir: true, mode: 0o755},
		{rel: "node/22.22.2", dir: true, mode: 0o755},
	})
	plan, err := BuildPlanFromPayload(payload, "linux/amd64", PackOptions{WorkDir: t.TempDir(), Version: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if got := plan.Manifest.Tools[0].Version; got != "24.16.0" {
		t.Fatalf("first version = %s, want the one bin-paths puts first", got)
	}
	if cfg := string(RenderConfig(plan.Manifest.Tools)); !strings.Contains(cfg, `["24.16.0", "22.22.2"]`) {
		t.Fatalf("precedence lost:\n%s", cfg)
	}
	if _, err := BuildPlanFromPayload(payload, "linux/amd64", PackOptions{WorkDir: t.TempDir(), Exclude: []string{"node"}}); err == nil || !strings.Contains(err.Error(), "nothing to pack") {
		t.Fatalf("--exclude node must remove both versions, got %v", err)
	}
}

func TestPayloadRejectsHostileEntryNames(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	_ = writeBytes(tw, payloadTools, 0o644, []byte(`{}`), time.Unix(0, 0).UTC())
	_ = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: "../escape", Mode: 0o644})
	_ = tw.Close()
	_ = gz.Close()
	p := filepath.Join(t.TempDir(), "p.tar.gz")
	if err := os.WriteFile(p, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := scanPayload(p, func(payloadEntry) error { return nil }); err == nil {
		t.Fatal("a payload with a traversal path should be rejected")
	}
}

// The container must hand over environment variable names only: mise config
// env entries can hold secrets, and a value in the payload would mean a
// secret in a temporary file on this machine.
func TestPayloadScriptEmitsEnvNamesButNeverValues(t *testing.T) {
	script := PayloadScript()
	if !strings.Contains(script, `sed -n 's/^export \([A-Za-z_][A-Za-z0-9_]*\)=.*/\1/p'`) {
		t.Errorf("the env extraction must keep names only:\n%s", script)
	}
	// Nothing may write a whole environment into the metadata.
	for _, forbidden := range []string{
		"mise env --json > ",
		"mise env --json >\"",
		"env > ",
		"printenv >",
	} {
		if strings.Contains(script, forbidden) {
			t.Errorf("the script emits environment values via %q:\n%s", forbidden, script)
		}
	}
	// bin-paths is used for precedence precisely because it carries no values.
	if !strings.Contains(script, "mise bin-paths") {
		t.Errorf("precedence should come from bin-paths, which has no values:\n%s", script)
	}
	// A single tar, since concatenated archives stop readers at the first one.
	if strings.Count(script, "tar -cf -") != 1 {
		t.Errorf("the payload must be one tar stream:\n%s", script)
	}
}

func TestVerifyELFChecksTheRealMachineType(t *testing.T) {
	elf := func(class, data byte, machine uint16) []byte {
		b := make([]byte, 64)
		copy(b, []byte{0x7f, 'E', 'L', 'F', class, data})
		b[18] = byte(machine)
		b[19] = byte(machine >> 8)
		return b
	}
	if err := verifyELF(elf(2, 1, 0x3e), "linux", "amd64"); err != nil {
		t.Errorf("a valid amd64 ELF was rejected: %v", err)
	}
	if err := verifyELF(elf(2, 1, 0xb7), "linux", "arm64"); err != nil {
		t.Errorf("a valid arm64 ELF was rejected: %v", err)
	}
	// An arm64 binary must not pass as amd64: that is the mistake worth
	// catching, since the build environment is what we are checking up on.
	if err := verifyELF(elf(2, 1, 0xb7), "linux", "amd64"); err == nil {
		t.Error("an arm64 binary was accepted as amd64")
	}
	for _, bad := range [][]byte{
		{}, []byte("MZ\x90\x00"), elf(1, 1, 0x3e), elf(2, 2, 0x3e),
	} {
		if err := verifyELF(bad, "linux", "amd64"); err == nil {
			t.Errorf("accepted a non-ELF or wrong-class binary: %q", bad)
		}
	}
}

func TestGoVersionAtLeast(t *testing.T) {
	cases := []struct {
		have string
		want bool
	}{
		{"go1.25", true}, {"go1.25.3", true}, {"go1.26.4", true},
		{"go2.0", true},
		// go1.24 builds the source fine but its output crashes on unpack
		// under an emulated amd64, so it must not qualify.
		{"go1.24", false}, {"go1.24.13", false},
		{"go1.23.9", false}, {"go1.9", false},
		{"", false}, {"devel", false},
	}
	for _, c := range cases {
		if got := goVersionAtLeast(c.have, UnpackerGoVersion); got != c.want {
			t.Errorf("goVersionAtLeast(%q, %q) = %v, want %v", c.have, UnpackerGoVersion, got, c.want)
		}
	}
}

// The builder container must run on this machine's architecture: a Go
// toolchain emulating a foreign one crashes instead of building.
func TestBuilderRunsOnTheHostArchitecture(t *testing.T) {
	if got, want := hostContainerPlatform(), "linux/"+runtime.GOARCH; got != want {
		t.Fatalf("hostContainerPlatform() = %q, want %q", got, want)
	}
	if strings.HasPrefix(hostContainerPlatform(), "darwin") {
		t.Fatal("a container platform can never be darwin")
	}
}

// The builder image must not be lowered back to a toolchain whose output is
// known to crash: a binary built by go1.24 segfaults in runtime.netpoll when
// run under an emulated amd64, which is exactly how a cross-built archive
// tends to get tried out first.
func TestDefaultBuilderImageIsAVerifiedTag(t *testing.T) {
	_, tag, ok := strings.Cut(DefaultBuilderImage, ":")
	if !ok {
		t.Fatalf("DefaultBuilderImage %q has no tag", DefaultBuilderImage)
	}
	major, minor, ok := parseGoVersion(strings.TrimSuffix(tag, "-bookworm"))
	if !ok {
		t.Fatalf("cannot read a Go version out of tag %q", tag)
	}
	if major != 1 || minor < minVerifiedBuilderMinor {
		t.Fatalf("builder image is go%d.%d, below the verified floor go1.%d", major, minor, minVerifiedBuilderMinor)
	}
	// It also has to satisfy the floor the unpacker build requires.
	if !goVersionAtLeast("go"+tag, UnpackerGoVersion) {
		t.Fatalf("builder image %q is older than %s", tag, UnpackerGoVersion)
	}
}

// Metadata can promise tools the payload does not carry - tar failing part
// way through produces exactly that. Writing the archive anyway would make a
// well formed file with a tool missing from it.
func TestBuildPlanFromPayloadRejectsMissingInstallTree(t *testing.T) {
	const root = "/root/.local/share/mise/installs"
	// jq is listed in the metadata but no installs/jq entry is present.
	payload := buildPayload(t, root, map[string][]string{"jq": {"1.8.1"}, "node": {"22.1.0"}}, []payloadFixture{
		{rel: "node/22.1.0", dir: true, mode: 0o755},
		{rel: "node/22.1.0/node", body: "x", mode: 0o755},
	})
	_, err := BuildPlanFromPayload(payload, "linux/amd64", PackOptions{WorkDir: t.TempDir(), Version: "t"})
	if err == nil {
		t.Fatal("a payload missing a tool's files must not produce an archive")
	}
	for _, want := range []string{"incomplete", "jq/1.8.1"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q: %v", want, err)
		}
	}

	// Excluding the tool whose files are absent makes the payload usable.
	plan, err := BuildPlanFromPayload(payload, "linux/amd64", PackOptions{WorkDir: t.TempDir(), Version: "t", Exclude: []string{"jq"}})
	if err != nil {
		t.Fatalf("excluding the missing tool should work: %v", err)
	}
	var buf bytes.Buffer
	if err := plan.Write(&buf); err != nil {
		t.Fatalf("writing the archive: %v", err)
	}
	m, err := ReadManifest(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Tools) != 1 || m.Tools[0].Name != "node" {
		t.Fatalf("unexpected tools: %+v", m.Tools)
	}
}

// The payload script must notice tar failing even though its status is
// swallowed by the pipeline into gzip.
func TestPayloadScriptDetectsTarFailure(t *testing.T) {
	script := PayloadScript()
	if !strings.Contains(script, "echo $? > \"$status_file\"") {
		t.Errorf("tar's exit status is not captured:\n%s", script)
	}
	if !strings.Contains(script, `if [ "$tar_status" -ne 0 ]; then`) {
		t.Errorf("tar's exit status is not acted on:\n%s", script)
	}

	// Run the guard with a tar that fails, and check the script exits non-zero.
	dir := t.TempDir()
	fake := filepath.Join(dir, "tar")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nexit 2\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, stub := range []string{"mise", "gzip"} {
		if err := os.WriteFile(filepath.Join(dir, stub), []byte("#!/bin/sh\ncat >/dev/null 2>&1 || true\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("sh", "-euc", PayloadScript())
	cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "MISE_DATA_DIR="+dir)
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if err := cmd.Run(); err == nil {
		t.Fatal("the script reported success even though tar failed")
	}
}

// A binary supplied with --mpack-bin has to be checked as closely as one
// mpack builds: bundling the wrong one breaks the single-file bootstrap.
func TestVerifyUnpackerRejectsWrongPlatformBinaries(t *testing.T) {
	elf := func(machine uint16) []byte {
		b := make([]byte, 64)
		copy(b, []byte{0x7f, 'E', 'L', 'F', 2, 1})
		b[18], b[19] = byte(machine), byte(machine>>8)
		return b
	}
	macho := func(cpu uint32) []byte {
		b := make([]byte, 32)
		binary.LittleEndian.PutUint32(b[:4], 0xfeedfacf)
		binary.LittleEndian.PutUint32(b[4:8], cpu)
		return b
	}
	if err := VerifyUnpacker(elf(0x3e), "linux/amd64"); err != nil {
		t.Errorf("a linux/amd64 ELF was rejected: %v", err)
	}
	// The case that matters: a host executable passed off as a Linux one.
	if err := VerifyUnpacker(macho(0x0100000c), "linux/amd64"); err == nil {
		t.Error("a macOS binary was accepted as the linux/amd64 unpacker")
	}
	if err := VerifyUnpacker(elf(0xb7), "linux/amd64"); err == nil {
		t.Error("a linux/arm64 binary was accepted as the linux/amd64 unpacker")
	}
	if err := VerifyUnpacker([]byte("#!/bin/sh\necho hi\n"), "linux/amd64"); err == nil {
		t.Error("a shell script was accepted as the unpacker")
	}
	if err := VerifyUnpacker(nil, "linux/amd64"); err == nil {
		t.Error("an empty file was accepted as the unpacker")
	}
	// Native targets are verified too, via Mach-O on macOS.
	if runtime.GOOS == "darwin" {
		if err := VerifyUnpacker(macho(machoCPUTypes[runtime.GOARCH]), HostTarget()); err != nil {
			t.Errorf("a valid host Mach-O was rejected: %v", err)
		}
		if err := VerifyUnpacker(elf(0x3e), HostTarget()); err == nil {
			t.Error("an ELF was accepted as a macOS unpacker")
		}
		fat := make([]byte, 16)
		binary.LittleEndian.PutUint32(fat[:4], 0xbebafeca)
		if err := VerifyUnpacker(fat, HostTarget()); err == nil {
			t.Error("a fat binary was accepted")
		}
	}
	// The real running binary must pass, since a native pack bundles it.
	self, err := SelfBinary()
	if err != nil {
		t.Skipf("cannot read the test binary: %v", err)
	}
	if err := VerifyUnpacker(self, HostTarget()); err != nil {
		t.Errorf("the running binary failed its own check: %v", err)
	}
}
