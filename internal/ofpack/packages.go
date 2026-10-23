package ofpack

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Operating system packages are carried as the files themselves, with the
// dependency closure resolved on a machine that can reach the repositories.
// They are never installed by unpack: putting an RPM or a .deb onto a system
// is a privileged, system-wide act, so it waits for an explicit command.
const (
	// PackagesPrefix is where the package files live inside the archive.
	PackagesPrefix = "packages"
	// PackagesListName is the file a project lists its packages in. It is
	// read only when --packages is given.
	PackagesListName = "packages.txt"

	// ManagerDNF and ManagerAPT are the two package managers handled.
	ManagerDNF = "dnf"
	ManagerAPT = "apt"
)

// Packages is the manifest record of the carried package set.
type Packages struct {
	// Manager is "dnf" or "apt".
	Manager string `json:"manager"`
	// OS is the /etc/os-release of the machine the closure was resolved
	// for. Installing elsewhere is refused: a closure is only valid for the
	// release it was resolved against.
	OS OSRelease `json:"os"`
	// Repos are the repository identifiers the packages came from, chosen
	// explicitly rather than "whatever was enabled".
	Repos []string `json:"repos,omitempty"`
	// BaseImage is the container image an apt closure was resolved in. The
	// closure is only as complete as that image's view of the archive.
	BaseImage string `json:"base_image,omitempty"`
	// Requested is what packages.txt asked for, before resolution.
	Requested []string `json:"requested"`
	// Closure says how the dependency set was computed, so what the archive
	// guarantees is on the record rather than assumed.
	Closure string `json:"closure"`
	// EssentialExcluded is true for apt: its closure covers declared
	// dependencies but not Essential packages, which apt treats as always
	// present.
	EssentialExcluded bool `json:"essential_excluded,omitempty"`
	// Files are the package files, with digests checked before anything is
	// installed with root privileges.
	Files []PackageFile `json:"files"`
}

// PackageFile is one carried package.
type PackageFile struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// OSRelease identifies a distribution release.
type OSRelease struct {
	ID        string `json:"id"`
	VersionID string `json:"version_id"`
}

// String renders an OS release the way a person would write it.
func (o OSRelease) String() string {
	if o.VersionID == "" {
		return o.ID
	}
	return o.ID + " " + o.VersionID
}

// Matches reports whether two releases are the same. The comparison includes
// the minor version: RHEL 10.2 and 10.3 ship different builds, so a closure
// resolved against one is not a closure for the other.
func (o OSRelease) Matches(other OSRelease) bool {
	return o.ID == other.ID && o.VersionID == other.VersionID
}

// packageNamePattern is what a package name may look like. Anything else is
// refused rather than passed on: these names reach a package manager, and a
// name is all packages.txt is allowed to contain.
var packageNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]*$`)

// ReadPackagesList parses a project's packages.txt: one package name per
// line, blank lines and # comments ignored. Versions, globs, paths and
// anything resembling an option are refused, with the line number.
func ReadPackagesList(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var names []string
	seen := map[string]bool{}
	sc := bufio.NewScanner(f)
	for line := 1; sc.Scan(); line++ {
		text := strings.TrimSpace(sc.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		if strings.ContainsAny(text, " \t") {
			return nil, fmt.Errorf("%s line %d: %q has more than one field; %s holds package names only, without versions or options",
				path, line, text, PackagesListName)
		}
		if !packageNamePattern.MatchString(text) {
			return nil, fmt.Errorf("%s line %d: %q is not a package name (allowed: letters, digits, and . _ + - after the first character)",
				path, line, text)
		}
		if seen[text] {
			continue
		}
		seen[text] = true
		names = append(names, text)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("%s lists no packages", path)
	}
	return names, nil
}

// ReadOSRelease reads an /etc/os-release style file.
func ReadOSRelease(path string) (OSRelease, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return OSRelease{}, err
	}
	return parseOSRelease(string(data)), nil
}

func parseOSRelease(body string) OSRelease {
	var o OSRelease
	for _, line := range strings.Split(body, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		switch key {
		case "ID":
			o.ID = value
		case "VERSION_ID":
			o.VersionID = value
		}
	}
	return o
}

// ManagerFor picks the package manager a distribution uses.
func ManagerFor(o OSRelease) (string, error) {
	switch o.ID {
	case "rhel", "centos", "fedora", "rocky", "almalinux":
		return ManagerDNF, nil
	case "debian", "ubuntu":
		return ManagerAPT, nil
	case "":
		return "", fmt.Errorf("could not tell which distribution this is; /etc/os-release has no ID")
	default:
		return "", fmt.Errorf("packages are not supported on %q; ofpack handles RHEL-family (dnf) and Debian-family (apt) systems", o.ID)
	}
}

// HashFile returns a file's sha256 and size.
func HashFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// CollectPackageFiles records every package file in dir with its digest.
func CollectPackageFiles(dir, suffix string) ([]PackageFile, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var files []PackageFile
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), suffix) {
			continue
		}
		if _, err := CheckArchiveName(e.Name()); err != nil {
			return nil, fmt.Errorf("package file %q: %w", e.Name(), err)
		}
		sum, size, err := HashFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		files = append(files, PackageFile{Name: e.Name(), SHA256: sum, Size: size})
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no %s files were produced in %s", suffix, dir)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })
	return files, nil
}

// PackageSuffix is the file extension each manager's packages use.
func PackageSuffix(manager string) string {
	if manager == ManagerAPT {
		return ".deb"
	}
	return ".rpm"
}
