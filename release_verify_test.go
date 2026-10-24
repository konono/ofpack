package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The release workflow runs .github/scripts/verify-artifacts.sh over what
// was actually published. These exercise it against deliberately broken
// releases, because a verification step that passes everything is worse
// than none: it says the artifacts were checked.
func TestVerifyArtifactsScript(t *testing.T) {
	for _, tool := range []string{"sh", "sha256sum", "tar"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not available", tool)
		}
	}
	script, err := filepath.Abs(filepath.Join(".github", "scripts", "verify-artifacts.sh"))
	if err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(t.TempDir(), "ofpack")
	if out, err := exec.Command("go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("building ofpack: %v\n%s", err, out)
	}
	platforms := []string{"linux_amd64", "linux_arm64", "darwin_amd64", "darwin_arm64"}

	// dist builds a release directory, then lets a case damage it.
	dist := func(t *testing.T, damage func(dir string, names []string)) string {
		t.Helper()
		dir := t.TempDir()
		var names []string
		for _, p := range platforms {
			name := "ofpack_" + p + ".tar.gz"
			cmd := exec.Command("tar", "-czf", filepath.Join(dir, name), "-C", filepath.Dir(binary), "ofpack")
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("packing %s: %v\n%s", name, err, out)
			}
			names = append(names, name)
		}
		damage(dir, names)
		return dir
	}
	sums := func(t *testing.T, dir string, names []string) {
		t.Helper()
		args := append([]string{"-a", "256"}, names...)
		cmd := exec.Command("shasum", args...)
		cmd.Dir = dir
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("checksumming: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "checksums.txt"), out, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	cases := []struct {
		name    string
		damage  func(t *testing.T) string
		wantErr string // a substring of the complaint, "" for success
	}{
		{
			name: "a complete release passes",
			damage: func(t *testing.T) string {
				return dist(t, func(dir string, names []string) { sums(t, dir, names) })
			},
		},
		{
			// The case this script was written for: checksums.txt lists one
			// archive, and --ignore-missing said nothing about the rest.
			name: "an archive with no checksum line is caught",
			damage: func(t *testing.T) string {
				return dist(t, func(dir string, names []string) { sums(t, dir, names[:1]) })
			},
			wantErr: "expected exactly 1",
		},
		{
			// A name differing only where a dot is. Matched as a regular
			// expression the dot is a wildcard, so this would be accepted.
			name: "a near-miss name does not count as a match",
			damage: func(t *testing.T) string {
				return dist(t, func(dir string, names []string) {
					sums(t, dir, names)
					body, err := os.ReadFile(filepath.Join(dir, "checksums.txt"))
					if err != nil {
						t.Fatal(err)
					}
					swapped := strings.Replace(string(body),
						"ofpack_linux_arm64.tar.gz", "ofpack_linux_arm64Xtar.gz", 1)
					if err := os.WriteFile(filepath.Join(dir, "checksums.txt"), []byte(swapped), 0o644); err != nil {
						t.Fatal(err)
					}
				})
			},
			wantErr: "expected exactly 1",
		},
		{
			name: "a missing archive is caught",
			damage: func(t *testing.T) string {
				return dist(t, func(dir string, names []string) {
					sums(t, dir, names)
					if err := os.Remove(filepath.Join(dir, names[3])); err != nil {
						t.Fatal(err)
					}
				})
			},
			wantErr: "missing artifact",
		},
		{
			name: "a corrupted archive is caught",
			damage: func(t *testing.T) string {
				return dist(t, func(dir string, names []string) {
					sums(t, dir, names)
					f, err := os.OpenFile(filepath.Join(dir, names[1]), os.O_APPEND|os.O_WRONLY, 0o644)
					if err != nil {
						t.Fatal(err)
					}
					defer f.Close()
					if _, err := f.WriteString("x"); err != nil {
						t.Fatal(err)
					}
				})
			},
			wantErr: "FAILED",
		},
		{
			name: "a release with no checksums at all is caught",
			damage: func(t *testing.T) string {
				return dist(t, func(dir string, names []string) {})
			},
			wantErr: "checksums.txt is missing",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := c.damage(t)
			out, err := exec.Command("sh", script, dir, currentVersion()).CombinedOutput()
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("a complete release was rejected: %v\n%s", err, out)
				}
				return
			}
			if err == nil {
				t.Fatalf("the damaged release passed verification:\n%s", out)
			}
			if !strings.Contains(string(out), c.wantErr) {
				t.Errorf("the complaint should mention %q:\n%s", c.wantErr, out)
			}
		})
	}

	// And a binary from a different release is caught.
	dir := dist(t, func(dir string, names []string) { sums(t, dir, names) })
	out, err := exec.Command("sh", script, dir, "9.9.9").CombinedOutput()
	if err == nil || !strings.Contains(string(out), "reports") {
		t.Errorf("a version mismatch should be caught, got %v:\n%s", err, out)
	}
}

func currentVersion() string { return version }
