package mpack

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// A payload is the intermediate stream a container produces: the tools it
// installed for the target platform, plus just enough metadata for this
// machine to build the real archive from them. mpack itself never runs inside
// the container, so no target-platform mpack binary is needed to pack.
//
// Layout (gzipped tar):
//
//	mpack-meta/tools.json         `mise ls --current --json` from the container
//	mpack-meta/bin-paths.txt      `mise bin-paths`, for version precedence
//	mpack-meta/env-names.txt      names only, one per line; never values
//	mpack-meta/mise-version.txt
//	mpack-meta/installs-root.txt  the installs root *inside the container*
//	bin/mise
//	installs/<rel>/...
const (
	payloadMetaDir     = "mpack-meta"
	payloadTools       = payloadMetaDir + "/tools.json"
	payloadBinPaths    = payloadMetaDir + "/bin-paths.txt"
	payloadEnvNames    = payloadMetaDir + "/env-names.txt"
	payloadMiseVersion = payloadMetaDir + "/mise-version.txt"
	payloadRoot        = payloadMetaDir + "/installs-root.txt"
)

// payloadMeta is the metadata half of a payload.
type payloadMeta struct {
	tools        map[string][]miseToolEntry
	binPaths     []string
	envNames     []string
	miseVersion  string
	installsRoot string
	scopes       []payloadScopeMeta
}

type payloadScopeMeta struct {
	path     string
	tools    map[string][]miseToolEntry
	binPaths []string
}

// readPayloadMeta reads the metadata entries from the head of a payload. The
// container writes them first so this does not have to scan the whole stream.
// Every entry name is validated before use: the payload is produced by a
// container, which is not a trusted source.
func readPayloadMeta(r io.Reader) (*payloadMeta, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, fmt.Errorf("container payload is not gzip: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	m := &payloadMeta{}
	seen := map[string]bool{}
	scopeParts := map[int]*payloadScopeMeta{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("reading container payload: %w", err)
		}
		name, err := CheckArchiveName(hdr.Name)
		if err != nil {
			return nil, err
		}
		if hdr.Typeflag == tar.TypeDir {
			continue // directory entries carry nothing we need
		}
		if !strings.HasPrefix(name, payloadMetaDir+"/") {
			break // metadata is written first; the files start here
		}
		if hdr.Typeflag != tar.TypeReg {
			return nil, fmt.Errorf("payload metadata %q is not a regular file", name)
		}
		body, err := io.ReadAll(io.LimitReader(tr, 1<<24))
		if err != nil {
			return nil, err
		}
		if seen[name] {
			return nil, fmt.Errorf("duplicate payload metadata %s", name)
		}
		seen[name] = true
		switch name {
		case payloadTools:
			if err := json.Unmarshal(body, &m.tools); err != nil {
				return nil, fmt.Errorf("parsing %s: %w", name, err)
			}
		case payloadBinPaths:
			m.binPaths = splitLines(body)
		case payloadEnvNames:
			m.envNames = splitLines(body)
		case payloadMiseVersion:
			m.miseVersion = strings.TrimSpace(string(body))
		case payloadRoot:
			m.installsRoot = strings.TrimSpace(string(body))
		default:
			var index int
			var kind string
			if _, err := fmt.Sscanf(name, payloadMetaDir+"/scopes/%d/%s", &index, &kind); err == nil && index >= 0 {
				s := scopeParts[index]
				if s == nil {
					s = &payloadScopeMeta{}
					scopeParts[index] = s
				}
				switch kind {
				case "path.txt":
					s.path = strings.TrimSpace(string(body))
				case "tools.json":
					if err := json.Unmarshal(body, &s.tools); err != nil {
						return nil, fmt.Errorf("parsing %s: %w", name, err)
					}
				case "bin-paths.txt":
					s.binPaths = splitLines(body)
				}
			}
		}
	}
	if m.tools == nil {
		return nil, fmt.Errorf("container payload has no %s", payloadTools)
	}
	if m.installsRoot == "" || !strings.HasPrefix(m.installsRoot, "/") {
		return nil, fmt.Errorf("container payload reports an unusable installs root %q", m.installsRoot)
	}
	if m.miseVersion == "" {
		m.miseVersion = "unknown"
	}
	for i := 0; i < len(scopeParts); i++ {
		s := scopeParts[i]
		if s == nil || s.tools == nil || s.path == "" {
			return nil, fmt.Errorf("incomplete payload metadata for scope %d", i)
		}
		if s.path != "." {
			if _, err := CheckArchiveName(s.path); err != nil {
				return nil, fmt.Errorf("scope %d: %w", i, err)
			}
		}
		m.scopes = append(m.scopes, *s)
	}
	if len(m.scopes) > 0 && m.scopes[0].path != "." {
		return nil, fmt.Errorf("container payload has no root scope")
	}
	return m, nil
}

func splitLines(b []byte) []string {
	var out []string
	sc := bufio.NewScanner(strings.NewReader(string(b)))
	for sc.Scan() {
		if line := strings.TrimSpace(sc.Text()); line != "" {
			out = append(out, line)
		}
	}
	return out
}

// payloadEntry is one file carried by a payload.
type payloadEntry struct {
	hdr  *tar.Header
	name string
}

// scanPayload walks the file half of a payload, calling fn for each entry.
// Entry names are validated and unsupported types rejected before fn sees
// them.
func scanPayload(path string, fn func(payloadEntry) error) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		name, err := CheckArchiveName(hdr.Name)
		if err != nil {
			return err
		}
		if strings.HasPrefix(name, payloadMetaDir+"/") {
			continue
		}
		switch hdr.Typeflag {
		case tar.TypeReg, tar.TypeDir, tar.TypeSymlink:
		default:
			return fmt.Errorf("container payload entry %q has unsupported type %q", name, string(hdr.Typeflag))
		}
		if err := fn(payloadEntry{hdr: hdr, name: name}); err != nil {
			return err
		}
	}
}

// copyPayloadBodies streams the file half of a payload into tw, using the
// headers already classified in byName and skipping anything absent from it.
func copyPayloadBodies(payloadPath string, byName map[string]entry, tw *tar.Writer) error {
	f, err := os.Open(payloadPath)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	written := 0
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		name, err := CheckArchiveName(hdr.Name)
		if err != nil {
			return err
		}
		e, ok := byName[name]
		if !ok {
			continue // metadata, or a link we deliberately dropped
		}
		out := &tar.Header{
			Typeflag: hdr.Typeflag,
			Name:     name,
			Mode:     int64(os.FileMode(hdr.Mode).Perm()),
			ModTime:  hdr.ModTime,
			Format:   tar.FormatPAX,
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			out.Name += "/"
			if err := tw.WriteHeader(out); err != nil {
				return err
			}
		case tar.TypeSymlink:
			out.Linkname = e.linkTarget
			if err := tw.WriteHeader(out); err != nil {
				return err
			}
		default:
			out.Size = hdr.Size
			if err := tw.WriteHeader(out); err != nil {
				return err
			}
			if _, err := io.CopyN(tw, tr, hdr.Size); err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
		}
		written++
	}
	if written != len(byName) {
		return fmt.Errorf("container payload held %d of the %d expected entries", written, len(byName))
	}
	return nil
}

// PayloadScript is what runs inside the container once the tools are
// installed: it emits the metadata and the files on stdout as one gzipped
// tar. Everything else must already have gone to stderr.
//
// Only environment variable *names* are emitted. mise config env entries can
// hold secrets, so the values never leave the container - not even into this
// machine's memory or a temporary file.
func PayloadScript() string { return PayloadScriptWith("") }

// PayloadScriptWith is PayloadScript plus one more directory to carry, used
// by the uv mode to add the wheel closure to the same stream.
func PayloadScriptWith(extraDir string) string { return payloadScript(extraDir, nil) }

// ScopedPayloadScript records each directory's effective selection as well as
// the shared install tree. scopePaths are container-absolute, root first.
func ScopedPayloadScript(scopePaths []string) string { return payloadScript("", scopePaths) }

func payloadScript(extraDir string, scopePaths []string) string {
	extra := ""
	if extraDir != "" {
		extra = " -C " + shellQuoteIfNeeded(filepath.Dir(extraDir)) + " " + shellQuoteIfNeeded(filepath.Base(extraDir))
	}
	script := `
root="$MISE_DATA_DIR/installs"
meta="$(mktemp -d)"
mkdir -p "$meta/` + payloadMetaDir + `"
mise ls --current --json > "$meta/` + payloadTools + `"
mise bin-paths > "$meta/` + payloadBinPaths + `" 2>/dev/null || : > "$meta/` + payloadBinPaths + `"
# Names only. Values are deliberately not emitted: they can be secrets.
mise env -s bash 2>/dev/null \
  | sed -n 's/^export \([A-Za-z_][A-Za-z0-9_]*\)=.*/\1/p' \
  | grep -v '^PATH$' > "$meta/` + payloadEnvNames + `" || : > "$meta/` + payloadEnvNames + `"
mise --version | awk '{print $1}' > "$meta/` + payloadMiseVersion + `"
printf '%s' "$root" > "$meta/` + payloadRoot + `"
`
	for i, dir := range scopePaths {
		if strings.ContainsAny(dir, "\n\r") {
			continue
		}
		rel := "."
		if i > 0 {
			rel = strings.TrimPrefix(dir, scopePaths[0]+"/")
		}
		script += fmt.Sprintf("mkdir -p \"$meta/%s/scopes/%d\"\n", payloadMetaDir, i)
		script += "cd " + shellQuote(dir) + "\n"
		script += fmt.Sprintf("printf '%%s' %s > \"$meta/%s/scopes/%d/path.txt\"\n", shellQuote(rel), payloadMetaDir, i)
		script += fmt.Sprintf("mise ls --current --json > \"$meta/%s/scopes/%d/tools.json\"\n", payloadMetaDir, i)
		script += fmt.Sprintf("mise bin-paths > \"$meta/%s/scopes/%d/bin-paths.txt\"\n", payloadMetaDir, i)
		script += "mise env -s bash 2>/dev/null | sed -n 's/^export \\([A-Za-z_][A-Za-z0-9_]*\\)=.*/\\1/p' | grep -v '^PATH$' >> \"$meta/" + payloadEnvNames + "\" || :\n"
	}
	if len(scopePaths) > 0 {
		script += "sort -u \"$meta/" + payloadEnvNames + "\" -o \"$meta/" + payloadEnvNames + "\"\n"
	}
	script += `

# Metadata first so the reader can get it without scanning the whole stream,
# then the mise binary, then the install trees.
mkdir -p "$meta/bin"
cp "$(command -v mise)" "$meta/bin/mise"
chmod 0755 "$meta/bin/mise"

# One tar, not two concatenated ones: a single stream is what readers expect,
# and several -C options keep it to one archive.
#
# A pipeline reports only the exit status of its last command, so tar failing
# part way through would otherwise look like success and produce a payload
# with metadata but no install trees. Its status is stashed and checked.
status_file="$meta/.tar-status"
{
  tar -cf - -C "$meta" ` + payloadMetaDir + ` bin -C "$(dirname "$root")" installs` + extra + `
  echo $? > "$status_file"
} | gzip -1
tar_status="$(cat "$status_file" 2>/dev/null || echo 1)"
if [ "$tar_status" -ne 0 ]; then
  echo "mpack: tar failed with status $tar_status while collecting the payload" >&2
  exit "$tar_status"
fi
`
	return script
}

// payloadToolsFromMeta converts the container's `mise ls` output into the
// tool list, with paths made relative to the container's installs root.
func payloadToolsFromMeta(m *payloadMeta) ([]Tool, error) {
	return toolsFromPayloadScope(m.tools, m.binPaths, m.installsRoot, false)
}

func toolsFromPayloadScope(raw map[string][]miseToolEntry, binPaths []string, root string, allowEmpty bool) ([]Tool, error) {
	var tools []Tool
	for name, entries := range raw {
		for _, e := range entries {
			if !e.Installed || !e.Active {
				continue
			}
			rel, ok := strings.CutPrefix(path.Clean(e.InstallPath), root+"/")
			if !ok {
				return nil, fmt.Errorf("tool %s installed at %s, which is outside the container installs root %s", name, e.InstallPath, root)
			}
			if _, err := CheckArchiveName(rel); err != nil {
				return nil, fmt.Errorf("tool %s: %w", name, err)
			}
			tools = append(tools, Tool{
				Name:             name,
				Version:          e.Version,
				RequestedVersion: e.RequestedVersion,
				RelPath:          rel,
				// The config path is a container path; it says nothing about
				// this machine, so it is left out.
			})
		}
	}
	if len(tools) == 0 && !allowEmpty {
		return nil, fmt.Errorf("the container installed no active tools")
	}
	ranks := RanksFromBinPaths(binPaths, root)
	sortToolsByPrecedence(tools, ranks)
	if err := verifyPrecedenceKnown(tools, ranks, nil); err != nil {
		return nil, err
	}
	return tools, nil
}
