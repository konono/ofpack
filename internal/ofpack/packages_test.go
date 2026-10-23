package ofpack

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
)

func writeList(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, PackagesListName)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// packages.txt holds package names. Anything else - a version, a path, a
// flag - is refused, because these names are handed to a package manager
// running as root.
func TestPackagesListAcceptsOnlyNames(t *testing.T) {
	names, err := ReadPackagesList(writeList(t, "# packages\njq\n\n  yq  \n# trailing comment\njq\n"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(names, ",") != "jq,yq" {
		t.Fatalf("got %v; want jq,yq with blanks, comments and the duplicate dropped", names)
	}

	for _, bad := range []string{
		"jq-1.7", // fine actually: a name may contain a dash and digits
	} {
		if _, err := ReadPackagesList(writeList(t, bad+"\n")); err != nil {
			t.Errorf("%q should be a valid package name: %v", bad, err)
		}
	}
	for _, bad := range []string{
		"jq >= 1.7", "jq=1.7 extra", "--assumeyes", "/etc/passwd", "../jq",
		"jq;rm -rf /", "jq && echo", "$(whoami)", "`id`", "jq\ttab", "*",
	} {
		_, err := ReadPackagesList(writeList(t, bad+"\n"))
		if err == nil {
			t.Errorf("%q was accepted as a package name", bad)
			continue
		}
		if !strings.Contains(err.Error(), "line 1") {
			t.Errorf("%q: the error should say which line: %v", bad, err)
		}
	}
	if _, err := ReadPackagesList(writeList(t, "# only comments\n\n")); err == nil {
		t.Error("a file with no packages should be an error")
	}
}

func TestOSReleaseParsingAndMatching(t *testing.T) {
	o := parseOSRelease(`NAME="Red Hat Enterprise Linux"
ID="rhel"
VERSION_ID="10.2"
`)
	if o.ID != "rhel" || o.VersionID != "10.2" {
		t.Fatalf("got %+v", o)
	}
	if o.String() != "rhel 10.2" {
		t.Errorf("String() = %q", o.String())
	}
	// The minor version is part of the identity: a closure resolved against
	// 10.2 is not a closure for 10.3.
	if o.Matches(OSRelease{ID: "rhel", VersionID: "10.3"}) {
		t.Error("10.2 and 10.3 must not match")
	}
	if !o.Matches(OSRelease{ID: "rhel", VersionID: "10.2"}) {
		t.Error("identical releases must match")
	}
	for id, want := range map[string]string{
		"rhel": ManagerDNF, "centos": ManagerDNF, "rocky": ManagerDNF,
		"debian": ManagerAPT, "ubuntu": ManagerAPT,
	} {
		got, err := ManagerFor(OSRelease{ID: id})
		if err != nil || got != want {
			t.Errorf("ManagerFor(%s) = %q, %v; want %q", id, got, err, want)
		}
	}
	if _, err := ManagerFor(OSRelease{ID: "plan9"}); err == nil {
		t.Error("an unsupported distribution should be reported")
	}
	if _, err := ManagerFor(OSRelease{}); err == nil {
		t.Error("a missing ID should be reported")
	}
}

// The install command must keep the package manager off the network by its
// own configuration, not by trusting the machine to be disconnected.
func TestInstallCommandsAreOffline(t *testing.T) {
	name, args, err := installCommand(ManagerDNF, []string{"/d/a.rpm", "/d/b.rpm"})
	if err != nil {
		t.Fatal(err)
	}
	joined := name + " " + strings.Join(args, " ")
	for _, want := range []string{"dnf install", "--disablerepo *", "--cacheonly", "/d/a.rpm", "/d/b.rpm"} {
		if !strings.Contains(joined, want) {
			t.Errorf("dnf command missing %q: %s", want, joined)
		}
	}
	name, args, err = installCommand(ManagerAPT, []string{"/d/a.deb"})
	if err != nil {
		t.Fatal(err)
	}
	joined = name + " " + strings.Join(args, " ")
	for _, want := range []string{
		"apt-get install", "Dir::Etc::sourcelist=/dev/null",
		"Dir::Etc::sourceparts=/dev/null", "Acquire::Retries=0", "/d/a.deb",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("apt command missing %q: %s", want, joined)
		}
	}
	// --no-download looks right but breaks on a transitive dependency with
	// "Pathname to install is not absolute", so it must not come back.
	if strings.Contains(joined, "--no-download") {
		t.Errorf("apt command uses --no-download, which fails with local paths: %s", joined)
	}
	if _, _, err := installCommand("yum-ish", nil); err == nil {
		t.Error("an unknown manager should be refused")
	}
}

// installPrep builds an unpacked tree carrying packages.
func installPrep(t *testing.T, manager string, bodies map[string]string) (dest string, m Manifest) {
	t.Helper()
	dest = t.TempDir()
	dir := filepath.Join(dest, PackagesPrefix, manager)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	pkgs := &Packages{
		Manager:   manager,
		OS:        OSRelease{ID: "rhel", VersionID: "10.2"},
		Requested: []string{"jq"},
		Closure:   "test",
	}
	for name, body := range bodies {
		full := filepath.Join(dir, name)
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		sum, size, err := HashFile(full)
		if err != nil {
			t.Fatal(err)
		}
		pkgs.Files = append(pkgs.Files, PackageFile{Name: name, SHA256: sum, Size: size})
	}
	m = Manifest{Schema: SchemaVersionPackages, Packages: pkgs}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dest, InstalledManifestName), data, 0o644); err != nil {
		t.Fatal(err)
	}
	return dest, m
}

func osReleaseFile(t *testing.T, id, version string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "os-release")
	if err := os.WriteFile(p, []byte("ID=\""+id+"\"\nVERSION_ID=\""+version+"\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// A package file that does not match the manifest must stop everything
// before a privileged package manager is run. These files are executable
// content and the archive is untrusted input.
func TestInstallRefusesATamperedPackageFile(t *testing.T) {
	dest, _ := installPrep(t, ManagerDNF, map[string]string{"jq-1.7.rpm": "original"})
	tampered := filepath.Join(dest, PackagesPrefix, ManagerDNF, "jq-1.7.rpm")
	if err := os.WriteFile(tampered, []byte("altered"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := InstallPackages(InstallOptions{
		Dest: dest, OSReleasePath: osReleaseFile(t, "rhel", "10.2"), DryRun: true,
	})
	if err == nil {
		t.Fatal("a tampered package file was accepted")
	}
	for _, want := range []string{"does not match the manifest", "nothing was installed"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q: %v", want, err)
		}
	}
}

// A package file the manifest does not list has no business being installed.
func TestInstallRefusesAnUnlistedPackageFile(t *testing.T) {
	dest, _ := installPrep(t, ManagerDNF, map[string]string{"jq-1.7.rpm": "a"})
	extra := filepath.Join(dest, PackagesPrefix, ManagerDNF, "sneaky-1.0.rpm")
	if err := os.WriteFile(extra, []byte("b"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := InstallPackages(InstallOptions{
		Dest: dest, OSReleasePath: osReleaseFile(t, "rhel", "10.2"), DryRun: true,
	})
	if err == nil || !strings.Contains(err.Error(), "sneaky-1.0.rpm") {
		t.Fatalf("an unlisted package file should be refused, got %v", err)
	}
}

// A closure is only valid for the release it was resolved against.
func TestInstallRefusesAnotherRelease(t *testing.T) {
	dest, _ := installPrep(t, ManagerDNF, map[string]string{"jq-1.7.rpm": "a"})
	err := InstallPackages(InstallOptions{
		Dest: dest, OSReleasePath: osReleaseFile(t, "rhel", "10.3"), DryRun: true,
	})
	if err == nil || !strings.Contains(err.Error(), "resolved for rhel 10.2") {
		t.Fatalf("a different minor release should be refused, got %v", err)
	}
	if !strings.Contains(err.Error(), "--no-os-check") {
		t.Errorf("the error should name the override: %v", err)
	}
	// Another distribution entirely, likewise.
	err = InstallPackages(InstallOptions{
		Dest: dest, OSReleasePath: osReleaseFile(t, "debian", "12"), DryRun: true,
	})
	if err == nil || !strings.Contains(err.Error(), "debian 12") {
		t.Fatalf("a different distribution should be refused, got %v", err)
	}
	// With the override it gets as far as the command.
	if err := InstallPackages(InstallOptions{
		Dest: dest, OSReleasePath: osReleaseFile(t, "rhel", "10.3"),
		SkipOSCheck: true, DryRun: true,
	}); err != nil {
		t.Errorf("--no-os-check should allow it: %v", err)
	}
}

// Installing is privileged, and ofpack does not escalate on its own.
func TestInstallRefusesWithoutRoot(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root")
	}
	dest, _ := installPrep(t, ManagerDNF, map[string]string{"jq-1.7.rpm": "a"})
	err := InstallPackages(InstallOptions{
		Dest: dest, OSReleasePath: osReleaseFile(t, "rhel", "10.2"),
	})
	if err == nil || !strings.Contains(err.Error(), "needs root") {
		t.Fatalf("a non-root install should be refused, got %v", err)
	}
	if !strings.Contains(err.Error(), "sudo ofpack packages install") {
		t.Errorf("the error should show how to re-run it: %v", err)
	}
}

func TestInstallRefusesAnArchiveWithoutPackages(t *testing.T) {
	dest := t.TempDir()
	data, err := json.Marshal(Manifest{Schema: SchemaVersion})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dest, InstalledManifestName), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := InstallPackages(InstallOptions{Dest: dest}); err == nil ||
		!strings.Contains(err.Error(), "--packages") {
		t.Fatalf("expected a clear error about the archive, got %v", err)
	}
}

// The apt fetch script must not carry package names: they come from a file,
// so nothing out of packages.txt is ever part of a command string.
func TestAptFetchScriptTakesNamesFromAFile(t *testing.T) {
	if !strings.Contains(aptFetchScript, "xargs -a /ofpack/"+PackagesListName) {
		t.Errorf("the script does not read the names from the mounted file:\n%s", aptFetchScript)
	}
	if !strings.Contains(aptFetchScript, "Dir::State::status=") {
		t.Errorf("the script does not empty the package status, so the closure would be partial:\n%s", aptFetchScript)
	}
	if !strings.Contains(aptFetchScript, "--download-only") {
		t.Errorf("the script should only download:\n%s", aptFetchScript)
	}
}

// Both fetchers insist on being told where packages come from.
func TestFetchRequiresAnExplicitSource(t *testing.T) {
	_, err := FetchPackages(ManagerDNF, OSRelease{ID: "rhel", VersionID: "10.2"}, FetchOptions{
		Names: []string{"jq"}, Dest: t.TempDir(),
	})
	if err == nil || !strings.Contains(err.Error(), "--package-repo is required") {
		t.Fatalf("dnf without repositories should be refused, got %v", err)
	}
	_, err = FetchPackages(ManagerAPT, OSRelease{ID: "debian", VersionID: "12"}, FetchOptions{
		Names: []string{"jq"}, Dest: t.TempDir(),
	})
	if err == nil || !strings.Contains(err.Error(), "--package-base is required") {
		t.Fatalf("apt without a base image should be refused, got %v", err)
	}
	if !strings.Contains(err.Error(), "ubuntu:24.04") {
		t.Errorf("the error should give examples: %v", err)
	}
}

// An archive may carry operating system packages and no toolchain. Unpack
// must not write an activate.sh or a config pointing at a mise that is not
// there: a file that promises a toolchain is worse than its absence.
func TestPackageOnlyArchiveHasNoFabricatedToolchain(t *testing.T) {
	stage := t.TempDir()
	dir := filepath.Join(stage, PackagesPrefix, ManagerDNF)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "jq-1.7.rpm"), []byte("rpm"), 0o644); err != nil {
		t.Fatal(err)
	}
	files, err := CollectPackageFiles(dir, ".rpm")
	if err != nil {
		t.Fatal(err)
	}
	plan, err := BuildPackagesOnlyPlan(PackagesOnlyOptions{
		WorkDir:  t.TempDir(),
		StageDir: stage,
		Version:  "test",
		Packages: &Packages{
			Manager: ManagerDNF, OS: OSRelease{ID: "rhel", VersionID: "10.2"},
			Requested: []string{"jq"}, Closure: "test", Files: files,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Manifest.CarriesMise() {
		t.Fatal("a package-only archive must not claim to carry mise")
	}
	if plan.Manifest.Schema != SchemaVersionPackages {
		t.Errorf("schema = %d, want %d", plan.Manifest.Schema, SchemaVersionPackages)
	}

	var buf bytes.Buffer
	if err := plan.Write(&buf); err != nil {
		t.Fatal(err)
	}
	// Nothing that implies a toolchain may be in the archive.
	names := archiveNames(t, buf.Bytes())
	for _, unwanted := range []string{MiseBinName, ConfigName, ActivateName, DirenvName} {
		if names[unwanted] {
			t.Errorf("a package-only archive contains %s", unwanted)
		}
	}
	if !names[path.Join(PackagesPrefix, ManagerDNF, "jq-1.7.rpm")] {
		t.Errorf("the package file is missing: %v", names)
	}

	dest := filepath.Join(t.TempDir(), "out")
	m, err := Unpack(bytes.NewReader(buf.Bytes()), UnpackOptions{Dest: dest, SkipArch: true})
	if err != nil {
		t.Fatal(err)
	}
	if m.Packages == nil || len(m.Packages.Files) != 1 {
		t.Fatalf("the packages record did not survive: %+v", m.Packages)
	}
	// And nothing fabricated on disk either.
	for _, unwanted := range []string{ActivateName, DirenvName, filepath.FromSlash(MiseBinName), filepath.FromSlash(ConfigName)} {
		if _, err := os.Stat(filepath.Join(dest, unwanted)); !os.IsNotExist(err) {
			t.Errorf("unpack wrote %s for an archive with no toolchain", unwanted)
		}
	}
	if _, err := os.Stat(filepath.Join(dest, PackagesPrefix, ManagerDNF, "jq-1.7.rpm")); err != nil {
		t.Errorf("the package file was not installed into the destination: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, InstalledManifestName)); err != nil {
		t.Errorf("the manifest should still be written: %v", err)
	}
}

// archiveNames lists the entries in an archive.
func archiveNames(t *testing.T, data []byte) map[string]bool {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	names := map[string]bool{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names[strings.TrimSuffix(hdr.Name, "/")] = true
	}
	return names
}

// Carrying packages is a layout an older ofpack does not know, so such an
// archive declares a schema it will refuse rather than unpack in part.
func TestPackagesBumpTheSchema(t *testing.T) {
	if SchemaVersionPackages <= SchemaVersionScoped {
		t.Fatalf("packages schema %d must be above the scoped one %d", SchemaVersionPackages, SchemaVersionScoped)
	}
	if err := checkSchema(SchemaVersionPackages); err != nil {
		t.Errorf("this ofpack must accept its own packages schema: %v", err)
	}
	// Scopes stay valid alongside packages: an archive can have both.
	m := &Manifest{
		Schema: SchemaVersionPackages,
		Tools:  []Tool{{Name: "go", Version: "1.25.6", RelPath: "go/1.25.6"}},
		Scopes: []Scope{{Path: ".", Tools: []Tool{{Name: "go", Version: "1.25.6", RelPath: "go/1.25.6"}}}},
	}
	if err := validateScopedManifest(m); err != nil {
		t.Errorf("scopes and packages must coexist: %v", err)
	}
	// A toolchain archive without scopes is fine too - uv archives are
	// exactly that - so the schema number must not be read as "has scopes".
	noScopes := &Manifest{Schema: SchemaVersionPackages, Tools: m.Tools}
	if err := validateScopedManifest(noScopes); err != nil {
		t.Errorf("an archive with tools and no scopes is valid: %v", err)
	}
}

// A package file replaced by a symlink after unpack must stop the install.
// Hashing through the link would check one file and hand the package
// manager another, as root.
func TestInstallRefusesSymlinkedPackageFiles(t *testing.T) {
	dest, _ := installPrep(t, ManagerDNF, map[string]string{"jq-1.7.rpm": "original"})
	dir := filepath.Join(dest, PackagesPrefix, ManagerDNF)
	elsewhere := filepath.Join(t.TempDir(), "evil.rpm")
	if err := os.WriteFile(elsewhere, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Same contents, so the digest would match if the link were followed.
	target := filepath.Join(dir, "jq-1.7.rpm")
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, target); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	err := InstallPackages(InstallOptions{
		Dest: dest, OSReleasePath: osReleaseFile(t, "rhel", "10.2"), DryRun: true,
	})
	if err == nil {
		t.Fatal("a symlinked package file was accepted even though its contents matched")
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Errorf("unhelpful error: %v", err)
	}
}

// Nor may the directory holding them be reached through a link.
func TestInstallRefusesASymlinkedPackageDirectory(t *testing.T) {
	dest, _ := installPrep(t, ManagerDNF, map[string]string{"jq-1.7.rpm": "a"})
	dir := filepath.Join(dest, PackagesPrefix, ManagerDNF)
	moved := filepath.Join(t.TempDir(), "real-dnf")
	if err := os.Rename(dir, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(moved, dir); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	err := InstallPackages(InstallOptions{
		Dest: dest, OSReleasePath: osReleaseFile(t, "rhel", "10.2"), DryRun: true,
	})
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("a symlinked package directory should be refused, got %v", err)
	}
}

// A manifest entry has to be one plain file name. A path in it would place
// the install outside the directory that was verified.
func TestInstallRefusesPathsAndDuplicatesInTheManifest(t *testing.T) {
	for _, bad := range []string{"../escape.rpm", "sub/jq.rpm", "/abs/jq.rpm", ".", "..", ""} {
		if err := checkPackageFileName(bad); err == nil {
			t.Errorf("manifest name %q was accepted", bad)
		}
	}
	if err := checkPackageFileName("jq-1.7.1-11.el10_2.2.x86_64.rpm"); err != nil {
		t.Errorf("a plain file name was rejected: %v", err)
	}

	// A repeated entry is refused rather than verified twice and installed
	// twice.
	dest, m := installPrep(t, ManagerDNF, map[string]string{"jq-1.7.rpm": "a"})
	m.Packages.Files = append(m.Packages.Files, m.Packages.Files[0])
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dest, InstalledManifestName), data, 0o644); err != nil {
		t.Fatal(err)
	}
	err = InstallPackages(InstallOptions{
		Dest: dest, OSReleasePath: osReleaseFile(t, "rhel", "10.2"), DryRun: true,
	})
	if err == nil || !strings.Contains(err.Error(), "more than once") {
		t.Fatalf("a duplicated manifest entry should be refused, got %v", err)
	}
}

// A destination that is itself a symlink must be refused: walking through
// it would hand a root package manager files from somewhere else.
func TestInstallRefusesASymlinkedDestination(t *testing.T) {
	real, _ := installPrep(t, ManagerDNF, map[string]string{"jq-1.7.rpm": "a"})
	link := filepath.Join(t.TempDir(), "dest-link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	err := InstallPackages(InstallOptions{
		Dest: link, OSReleasePath: osReleaseFile(t, "rhel", "10.2"), DryRun: true,
	})
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("a symlinked destination should be refused, got %v", err)
	}
	// The real one still works, so the check is not refusing everything.
	if err := InstallPackages(InstallOptions{
		Dest: real, OSReleasePath: osReleaseFile(t, "rhel", "10.2"), DryRun: true,
	}); err != nil {
		t.Errorf("the real destination should be accepted: %v", err)
	}
}

// RPMs are downloaded for the architecture dnf runs on, so fetching them
// natively while packing for another platform would mix architectures in
// one archive.
func TestNativeFetchRefusesAForeignTarget(t *testing.T) {
	foreign := "linux/amd64"
	if HostPlatform() == foreign {
		foreign = "linux/arm64"
	}
	_, err := FetchPackages(ManagerDNF, OSRelease{ID: "rhel", VersionID: "10.2"}, FetchOptions{
		Names: []string{"jq"}, Dest: t.TempDir(),
		Repos: []string{"baseos"}, Platform: foreign,
	})
	if err == nil || !strings.Contains(err.Error(), "cannot fetch RPMs for "+foreign) {
		t.Fatalf("a foreign target should be refused for a native fetch, got %v", err)
	}
	if !strings.Contains(err.Error(), HostPlatform()) {
		t.Errorf("the error should name this machine's platform: %v", err)
	}
}
