package ofpack

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// A uv project is carried as a wheel closure rather than as a virtual
// environment. A .venv holds absolute paths and host-native binaries, so the
// one on the packing machine is never copied; instead the archive brings the
// target's Python, uv, and every wheel needed, and the environment is built
// fresh on the other side.
const (
	// UVPrefix is where the uv payload lives inside the archive.
	UVPrefix = "uv"
	// WheelhouseDir holds the downloaded wheels.
	WheelhouseDir = UVPrefix + "/wheelhouse"
	// RequirementsName is what to install: the project's declared
	// dependencies, not a lock file.
	RequirementsName = UVPrefix + "/requirements.txt"
	// ConstraintsName pins versions from uv.lock when there is one. It
	// constrains rather than requires, so lock entries for other platforms
	// do not force themselves into the environment.
	ConstraintsName = UVPrefix + "/constraints.txt"

	// VenvDir is the environment restore creates, relative to the project.
	VenvDir = ".venv"
)

// projectFiles are the dependency files ofpack opens in a uv project. When
// --python is absent it may also read the project's mise.toml, solely to
// select Python. Everything else - .credentials, .generated-passwords.yml,
// .envrc, inventories, roles - is neither read nor packed.
var projectFiles = struct {
	Pyproject    string
	Lock         string
	Requirements string
}{"pyproject.toml", "uv.lock", "requirements.txt"}

// FindUVProject finds the one project below the chosen pack root. Directory
// symlinks and generated trees are never followed, so a checkout or .venv
// cannot accidentally become another project.
func FindUVProject(root string) (string, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	var found []string
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if path != root {
				switch entry.Name() {
				case ".git", ".venv", ".ofpack", "node_modules", ".tox", ".nox", "__pycache__", "vendor", "build", "dist", ".cache":
					return filepath.SkipDir
				}
			}
			return nil
		}
		if entry.Name() == projectFiles.Pyproject && entry.Type().IsRegular() {
			found = append(found, filepath.Dir(path))
		}
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("finding pyproject.toml below %s: %w", root, err)
	}
	switch len(found) {
	case 0:
		return "", fmt.Errorf("no pyproject.toml found below %s; run from the project root or use -C", root)
	case 1:
		return found[0], nil
	default:
		return "", fmt.Errorf("multiple pyproject.toml files below %s (%s); use -C to select one project", root, strings.Join(found, ", "))
	}
}

// UVProject is the manifest record of a packed uv environment.
type UVProject struct {
	// PythonVersion and UvVersion are what the archive carries.
	PythonVersion string `json:"python_version"`
	UvVersion     string `json:"uv_version"`
	// Requirements are the project's declared dependencies, as packed.
	Requirements []string `json:"requirements"`
	// Constraints are exact pins taken from uv.lock, if there was one.
	Constraints []string `json:"constraints,omitempty"`
	HasLock     bool     `json:"has_lock"`
	// Wheels is how many wheels the closure holds.
	Wheels int `json:"wheels"`
	// PyprojectSHA256 and LockSHA256 let restore refuse to build an
	// environment for a project that has changed since it was packed.
	PyprojectSHA256 string `json:"pyproject_sha256"`
	LockSHA256      string `json:"lock_sha256,omitempty"`
	// VerifiedOffline records that the closure was installed with no network
	// at pack time, and how.
	VerifiedOffline string `json:"verified_offline,omitempty"`
}

// UVSource is what ofpack read from the project directory. Nothing outside
// these three files is opened.
type UVSource struct {
	Dir             string
	Pyproject       []byte
	PyprojectSHA    string
	Lock            []byte
	LockSHA         string
	HasLock         bool
	Requirements    []string
	Constraints     []string
	RequirementsTxt []string
}

// ReadUVProject reads a uv project's dependency declaration. It opens
// pyproject.toml, and uv.lock and requirements.txt if they exist, and nothing
// else: a project directory can hold credentials, and ofpack has no business
// in them.
func ReadUVProject(dir string) (*UVSource, error) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	s := &UVSource{Dir: dir}

	s.Pyproject, err = os.ReadFile(filepath.Join(dir, projectFiles.Pyproject))
	if err != nil {
		return nil, fmt.Errorf("%s is needed to pack a uv project: %w", projectFiles.Pyproject, err)
	}
	s.PyprojectSHA = sha256Hex(s.Pyproject)
	s.Requirements, err = pyprojectDependencies(s.Pyproject)
	if err != nil {
		return nil, err
	}
	if len(s.Requirements) == 0 {
		return nil, fmt.Errorf("%s declares no dependencies in [project].dependencies", projectFiles.Pyproject)
	}

	if lock, err := os.ReadFile(filepath.Join(dir, projectFiles.Lock)); err == nil {
		s.Lock, s.HasLock, s.LockSHA = lock, true, sha256Hex(lock)
		s.Constraints = lockPins(lock, projectName(s.Pyproject))
	} else if !os.IsNotExist(err) {
		return nil, err
	}

	// requirements.txt is advisory only. In this project it lists a subset of
	// what pyproject declares, so installing from it would quietly leave
	// Ansible out; it is read to report the difference, never to drive the
	// install.
	if req, err := os.ReadFile(filepath.Join(dir, projectFiles.Requirements)); err == nil {
		s.RequirementsTxt = requirementNames(string(req))
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	return s, nil
}

// ProjectMisePython reads the Python selector from the project's own mise
// configuration. A parent project's selection must not silently become this
// project's. read reports whether any config file was opened.
func ProjectMisePython(dir string) (selector string, read bool, err error) {
	selector, _, read, err = misePythonFromDir(dir)
	return selector, read, err
}

// ScopedMisePython merges only Python selectors along root..project. The
// nearest file wins; an ancestor above root is never considered.
func ScopedMisePython(root, project string) (selector, source string, read bool, err error) {
	root, err = filepath.Abs(root)
	if err != nil {
		return "", "", false, err
	}
	project, err = filepath.Abs(project)
	if err != nil {
		return "", "", false, err
	}
	rel, err := filepath.Rel(root, project)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", "", false, fmt.Errorf("project %s is outside pack root %s", project, root)
	}
	for dir := root; ; {
		value, file, opened, readErr := misePythonFromDir(dir)
		if readErr != nil {
			return "", "", read, readErr
		}
		read = read || opened
		if value != "" {
			selector, source = value, file
		}
		if dir == project {
			break
		}
		part, _, _ := strings.Cut(rel, string(filepath.Separator))
		dir = filepath.Join(dir, part)
		if rel == part {
			rel = ""
		} else {
			rel = strings.TrimPrefix(rel, part+string(filepath.Separator))
		}
	}
	return selector, source, read, nil
}

// GlobalMisePython reads only the global Python selector as a final fallback.
// An explicit global-config file takes precedence over MISE_CONFIG_DIR.
func GlobalMisePython() (selector, source string, err error) {
	if path := os.Getenv("MISE_GLOBAL_CONFIG_FILE"); path != "" {
		data, readErr := os.ReadFile(path)
		if os.IsNotExist(readErr) {
			return "", "", nil
		}
		if readErr != nil {
			return "", "", fmt.Errorf("reading %s: %w", path, readErr)
		}
		selector, err = tomlConfigPython(path, data)
		return selector, path, err
	}
	dir := DefaultGlobalConfigDir()
	for _, name := range []string{"config.toml", "mise.toml"} {
		path := filepath.Join(dir, name)
		data, readErr := os.ReadFile(path)
		if os.IsNotExist(readErr) {
			continue
		}
		if readErr != nil {
			return "", "", fmt.Errorf("reading %s: %w", path, readErr)
		}
		value, convErr := tomlConfigPython(path, data)
		if convErr != nil {
			return "", "", convErr
		}
		if value != "" {
			return value, path, nil
		}
	}
	return "", "", nil
}

// misePythonFromDir returns the Python selector the directory's mise
// configuration asks for, reading the same files mise does, in the order
// mise resolves them.
//
// A file that declares no Python is not an answer, so the next file down is
// tried - that is what mise does too. But a file that *does* declare one
// which cannot be interpreted is an error: falling through would quietly
// build on a different Python than the directory asks for, and the mismatch
// would only surface on the disconnected machine.
func misePythonFromDir(dir string) (selector, source string, read bool, err error) {
	for _, name := range miseConfigPrecedence {
		file := filepath.Join(dir, filepath.FromSlash(name))
		data, readErr := os.ReadFile(file)
		if os.IsNotExist(readErr) {
			continue
		}
		if readErr != nil {
			return "", "", read, fmt.Errorf("reading %s: %w", file, readErr)
		}
		read = true
		var value string
		if name == ".tool-versions" {
			// Not TOML: a plain "<tool> <version>" list, so it gets its own
			// reader rather than being fed to the TOML one.
			value, err = toolVersionsPython(file, data)
		} else {
			value, err = tomlConfigPython(file, data)
		}
		if err != nil {
			return "", "", read, err
		}
		if value != "" {
			return value, file, read, nil
		}
	}
	return "", "", read, nil
}

// tomlConfigPython reads [tools].python out of a mise TOML config.
func tomlConfigPython(file string, data []byte) (string, error) {
	body, ok := tomlTableBody(string(data), "tools")
	if !ok {
		return "", nil
	}
	idx := findKey(body, "python")
	if idx < 0 {
		return "", nil
	}
	line := body[idx:]
	if nl := strings.IndexByte(line, '\n'); nl >= 0 {
		line = line[:nl]
	}
	_, raw, ok := strings.Cut(line, "=")
	if !ok {
		return "", unreadablePython(file, "it is not a key = value assignment")
	}
	raw = strings.TrimSpace(raw)
	if i := strings.Index(raw, " #"); i >= 0 {
		raw = strings.TrimSpace(raw[:i])
	}
	switch {
	case raw == "":
		return "", unreadablePython(file, "it has no value")
	case strings.HasPrefix(raw, "["):
		// Several versions at once: ofpack builds one environment, so which
		// was meant is genuinely ambiguous.
		return "", unreadablePython(file, "it lists several versions")
	case strings.HasPrefix(raw, "{"):
		return "", unreadablePython(file, "it uses the table form")
	}
	if len(raw) < 3 || !(raw[0] == '"' && raw[len(raw)-1] == '"' || raw[0] == '\'' && raw[len(raw)-1] == '\'') {
		return "", unreadablePython(file, `it is not a single quoted version such as "3.12" or "latest"`)
	}
	selector := unquoteTOML(raw)
	if selector == "" {
		return "", unreadablePython(file, "it is empty")
	}
	return selector, nil
}

// toolVersionsPython reads a python entry out of a .tool-versions file.
func toolVersionsPython(file string, data []byte) (string, error) {
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 0 || normalizeDist(fields[0]) != "python" {
			continue
		}
		switch {
		case len(fields) == 1:
			return "", unreadablePython(file, "the python entry names no version")
		case len(fields) > 2:
			return "", unreadablePython(file, "the python entry lists several versions")
		}
		return fields[1], nil
	}
	return "", nil
}

// PythonSelectorLabel names the thing that was read, so a .tool-versions
// file is not reported as a [tools] table it does not have.
func PythonSelectorLabel(file string) string {
	if filepath.Base(file) == ".tool-versions" {
		return "python entry"
	}
	return "[tools].python"
}

func unreadablePython(file, why string) error {
	return fmt.Errorf("%s selects a Python that ofpack cannot interpret: %s.\nIt is not ignored, because that would silently build the environment on a different Python than this directory asks for. Use --python X.Y to choose one explicitly", file, why)
}

// MissingFromRequirementsTxt reports dependencies that pyproject declares but
// requirements.txt does not mention, so the discrepancy is visible rather
// than silently decided.
func (s *UVSource) MissingFromRequirementsTxt() []string {
	if len(s.RequirementsTxt) == 0 {
		return nil
	}
	have := map[string]bool{}
	for _, n := range s.RequirementsTxt {
		have[normalizeDist(n)] = true
	}
	var missing []string
	for _, r := range s.Requirements {
		if n := requirementName(r); n != "" && !have[normalizeDist(n)] {
			missing = append(missing, n)
		}
	}
	sort.Strings(missing)
	return missing
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// pyprojectDependencies pulls [project].dependencies out of pyproject.toml.
// Only that one array is read; the file is not otherwise interpreted.
func pyprojectDependencies(data []byte) ([]string, error) {
	arr, ok := tomlStringArray(string(data), "project", "dependencies")
	if !ok {
		return nil, fmt.Errorf("%s has no [project].dependencies array", projectFiles.Pyproject)
	}
	return arr, nil
}

// projectName reads [project].name, so the project itself can be left out of
// the constraints taken from the lock.
func projectName(data []byte) string {
	v, _ := tomlStringValue(string(data), "project", "name")
	return v
}

// lockPins turns a uv.lock into "name==version" constraint lines. The lock is
// parsed only for those two fields; resolution is left to the installer,
// which is why these are constraints and not requirements - a lock covers
// every platform, and the wheels for another one are neither wanted nor
// available.
func lockPins(lock []byte, skip string) []string {
	var pins []string
	var name, version string
	inPackage := false
	flush := func() {
		if inPackage && name != "" && version != "" && !strings.EqualFold(name, skip) {
			pins = append(pins, name+"=="+version)
		}
		name, version = "", ""
	}
	for _, line := range strings.Split(string(lock), "\n") {
		t := strings.TrimSpace(line)
		switch {
		case t == "[[package]]":
			flush()
			inPackage = true
		case strings.HasPrefix(t, "[") && t != "[[package]]":
			// Any other table ends the package header we were reading.
			if !strings.HasPrefix(t, "[package.") {
				flush()
				inPackage = false
			}
		case inPackage && name == "" && strings.HasPrefix(t, "name = "):
			name = unquoteTOML(strings.TrimPrefix(t, "name = "))
		case inPackage && version == "" && strings.HasPrefix(t, "version = "):
			version = unquoteTOML(strings.TrimPrefix(t, "version = "))
		}
	}
	flush()
	sort.Strings(pins)
	return pins
}

// requirementName extracts the distribution name from a requirement string
// such as `ansible-core>=2.17,<3.0`.
func requirementName(req string) string {
	req = strings.TrimSpace(req)
	if i := strings.IndexAny(req, " ;["); i >= 0 {
		req = req[:i]
	}
	for i, r := range req {
		if strings.ContainsRune("=<>!~@", r) {
			return strings.TrimSpace(req[:i])
		}
	}
	return req
}

func requirementNames(body string) []string {
	var out []string
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "-") {
			continue
		}
		if n := requirementName(line); n != "" {
			out = append(out, n)
		}
	}
	return out
}

// normalizeDist applies PEP 503 name normalisation, so Ansible_Core and
// ansible-core compare equal.
func normalizeDist(name string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(name) {
		if r == '-' || r == '_' || r == '.' {
			if !lastDash {
				b.WriteByte('-')
				lastDash = true
			}
			continue
		}
		lastDash = false
		b.WriteRune(r)
	}
	return b.String()
}

// tomlStringArray reads a single array of strings from a top-level table. It
// is deliberately small: ofpack needs two fields out of pyproject.toml and
// nothing else, and a full TOML parser is not worth the dependency.
func tomlStringArray(doc, table, key string) ([]string, bool) {
	body, ok := tomlTableBody(doc, table)
	if !ok {
		return nil, false
	}
	idx := findKey(body, key)
	if idx < 0 {
		return nil, false
	}
	rest := strings.TrimSpace(body[idx:])
	// The key may be quoted, so cut at the assignment rather than trimming a
	// bare name off the front.
	if _, after, ok := strings.Cut(rest, "="); ok {
		rest = strings.TrimSpace(after)
	}
	if !strings.HasPrefix(rest, "[") {
		return nil, false
	}
	end := strings.Index(rest, "]")
	if end < 0 {
		return nil, false
	}
	return splitTOMLArray(rest[1:end]), true
}

// splitTOMLArray splits array items on commas that are outside quotes. A
// requirement is very likely to contain a comma itself - "ansible-core>=2.17,
// <3.0" is one item, not two - so a plain Split would quietly truncate the
// version range and change what gets installed.
func splitTOMLArray(body string) []string {
	var out []string
	var cur strings.Builder
	var quote rune
	flush := func() {
		if v := unquoteTOML(strings.TrimSpace(cur.String())); v != "" {
			out = append(out, v)
		}
		cur.Reset()
	}
	for _, r := range body {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			}
			cur.WriteRune(r)
		case r == '"' || r == '\'':
			quote = r
			cur.WriteRune(r)
		case r == ',':
			flush()
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return out
}

func tomlStringValue(doc, table, key string) (string, bool) {
	body, ok := tomlTableBody(doc, table)
	if !ok {
		return "", false
	}
	idx := findKey(body, key)
	if idx < 0 {
		return "", false
	}
	line := body[idx:]
	if nl := strings.IndexByte(line, '\n'); nl >= 0 {
		line = line[:nl]
	}
	_, after, ok := strings.Cut(line, "=")
	if !ok {
		return "", false
	}
	return unquoteTOML(strings.TrimSpace(after)), true
}

// tomlTableBody returns the lines belonging to [table].
func tomlTableBody(doc, table string) (string, bool) {
	header := "[" + table + "]"
	var collecting bool
	var b strings.Builder
	for _, line := range strings.Split(doc, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "[") {
			collecting = t == header
			continue
		}
		if collecting {
			b.WriteString(line)
			b.WriteByte('\n')
		}
	}
	if b.Len() == 0 {
		return "", false
	}
	return b.String(), true
}

// findKey locates a top-level key assignment in a table body.
func findKey(body, key string) int {
	// TOML allows a bare key or a quoted one, and mise honours both. ofpack
	// writes the quoted form itself, so matching only the bare one meant it
	// could not read back configuration in its own style.
	forms := []string{key, `"` + key + `"`, "'" + key + "'"}
	offset := 0
	for _, line := range strings.Split(body, "\n") {
		t := strings.TrimSpace(line)
		for _, form := range forms {
			if !strings.HasPrefix(t, form) {
				continue
			}
			rest := strings.TrimSpace(strings.TrimPrefix(t, form))
			if strings.HasPrefix(rest, "=") {
				return offset + strings.Index(line, form)
			}
		}
		offset += len(line) + 1
	}
	return -1
}

func unquoteTOML(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, " #"); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	s = strings.TrimSuffix(s, ",")
	s = strings.TrimSpace(s)
	if len(s) >= 2 && (s[0] == '"' && s[len(s)-1] == '"' || s[0] == '\'' && s[len(s)-1] == '\'') {
		return s[1 : len(s)-1]
	}
	return s
}

// UVBuildScript is what runs in the target-platform container: install a
// Python and a uv for that platform, then download every wheel the project
// needs. It uses a configuration written here, listing only Python and uv,
// so the packing machine's own mise setup - and the unrelocatable symlinks
// in it - stay out of this entirely.
//
// Nothing of the project but its dependency declaration is present: the
// requirements and constraints are passed in, not read from a mounted tree.
func UVBuildScript(s *UVSource, pythonSpec string) string {
	var b strings.Builder
	b.WriteString(`export DEBIAN_FRONTEND=noninteractive
apt-get update -qq >&2
apt-get install -y -qq --no-install-recommends ca-certificates curl xz-utils >/dev/null 2>&1

export MISE_DATA_DIR=/ofpack-out/mise
export MISE_CONFIG_DIR=/ofpack-out/mise/config
mkdir -p "$MISE_CONFIG_DIR" /ofpack-out/` + UVPrefix + `/wheelhouse
`)
	fmt.Fprintf(&b, `cat > "$MISE_CONFIG_DIR/config.toml" <<'OFPACK_CFG'
[tools]
"python" = %s
"uv" = "latest"
OFPACK_CFG
`, tomlQuote(pythonSpec))

	b.WriteString(`curl -fsSL https://mise.run | MISE_INSTALL_PATH=/ofpack-out/mise/bin/mise sh >/dev/null 2>&1
/ofpack-out/mise/bin/mise install --yes >&2
eval "$(/ofpack-out/mise/bin/mise -C / env -s bash)"
echo "--- toolchain ---" >&2
python --version >&2
`)

	// The requirement set comes from pyproject's dependencies. Using
	// requirements.txt instead would install a subset - in a real project it
	// was missing Ansible entirely - so it is never the source of truth.
	b.WriteString(heredoc("/ofpack-out/"+RequirementsName, s.Requirements))
	if len(s.Constraints) > 0 {
		b.WriteString(heredoc("/ofpack-out/"+ConstraintsName, s.Constraints))
	}

	// pip is what fetches the wheels, so its absence is fatal rather than
	// something to shrug at. Nothing here is allowed to fail silently.
	b.WriteString(`
python -m ensurepip --upgrade >/dev/null 2>&1 \
  || echo "ofpack: ensurepip reported a problem; checking for pip anyway" >&2
if ! python -m pip --version >/dev/null 2>&1; then
  echo "ofpack: the packed python has no pip, so the wheels cannot be downloaded" >&2
  exit 1
fi
python -m pip install --quiet --upgrade pip >/dev/null 2>&1 \
  || echo "ofpack: could not upgrade pip; continuing with the version that shipped" >&2

echo "--- downloading wheels ---" >&2
`)
	// No "|| true" anywhere: a wheel that cannot be fetched has to stop the
	// pack, not leave a hole that only shows up on the disconnected machine.
	constraintArg := ""
	if len(s.Constraints) > 0 {
		constraintArg = " -c /ofpack-out/" + ConstraintsName
	}
	fmt.Fprintf(&b, `python -m pip download \
  --dest /ofpack-out/%s \
  --only-binary=:all: \
  -r /ofpack-out/%s%s >&2
`, WheelhouseDir, RequirementsName, constraintArg)

	b.WriteString(`wheels=$(find /ofpack-out/` + WheelhouseDir + ` -name '*.whl' | wc -l)
echo "--- $wheels wheels ---" >&2
if [ "$wheels" -eq 0 ]; then
  echo "ofpack: no wheels were downloaded" >&2
  exit 1
fi

# The payload step shells out to mise, which lives in the tree being packed
# rather than on the image's PATH.
PATH="/ofpack-out/mise/bin:$PATH"
export PATH

` + PayloadScriptWith("/ofpack-out/"+UVPrefix) + `
`)
	return b.String()
}

// UVVerifyScript installs the closure with no network available, into a
// throwaway environment, and runs the result. Packing an archive that cannot
// actually be restored is the failure worth catching here rather than on the
// disconnected machine.
func UVVerifyScript() string {
	return `
# Prove there is no network rather than assuming the container has none.
if curl -s --max-time 3 -o /dev/null https://pypi.org/simple/ 2>/dev/null; then
  echo "ofpack: this check must run without network access, but pypi.org is reachable" >&2
  exit 1
fi

# The payload carries the interpreter under installs/; it is used directly
# here because this check is about the wheel closure. The mise-driven path is
# what "ofpack uv restore" exercises on the target itself.
py=$(ls -d /payload/` + InstallsPrefix + `/python/*/bin/python3 2>/dev/null | head -1)
if [ -z "$py" ]; then
  echo "ofpack: the payload carries no python interpreter" >&2
  exit 1
fi
echo "python: $("$py" --version 2>&1)" >&2

cd /tmp
"$py" -m venv verify-venv
./verify-venv/bin/python -m pip install --quiet --no-index \
  --find-links /payload/` + WheelhouseDir + ` \
  -r /payload/` + RequirementsName + ` >&2
./verify-venv/bin/python -m pip check >&2
./verify-venv/bin/python -c 'import importlib.metadata as m; print(f"{len(list(m.distributions()))} distributions installed with no network")' >&2
`
}

// heredoc writes lines to a file inside a generated script, quoted so the
// shell does not touch their contents.
func heredoc(path string, lines []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "cat > %s <<'OFPACK_EOF'\n", shellQuoteIfNeeded(path))
	for _, l := range lines {
		b.WriteString(l)
		b.WriteByte('\n')
	}
	b.WriteString("OFPACK_EOF\n")
	return b.String()
}

// RestoreOptions controls `ofpack uv restore`.
type RestoreOptions struct {
	// Project is the directory holding pyproject.toml; .venv goes beside it.
	Project string
	// Dest is the unpacked ofpack tree. Empty means the conventional one.
	Dest string
	// Policy decides what happens when .venv already exists.
	Policy OverwritePolicy
	Log    io.Writer
}

// RestoreVenv builds the project's .venv from the archive's wheel closure,
// with no network access. Nothing outside .venv is written.
func RestoreVenv(opts RestoreOptions) (venvPath string, built bool, err error) {
	project, err := filepath.Abs(opts.Project)
	if err != nil {
		return "", false, err
	}
	dest := opts.Dest
	if dest == "" {
		dest = filepath.Join(project, OfpackSubdir)
	}
	dest, err = filepath.Abs(dest)
	if err != nil {
		return "", false, err
	}

	m, err := readInstalledManifest(dest)
	if err != nil {
		return "", false, err
	}
	if m.UVProject == nil {
		return "", false, fmt.Errorf("%s was not packed with --uv-project, so it carries no Python environment", dest)
	}
	if err := checkProjectMatches(project, m.UVProject); err != nil {
		return "", false, err
	}

	venv := filepath.Join(project, VenvDir)
	switch _, statErr := os.Lstat(venv); {
	case statErr == nil && opts.Policy == OverwriteFail:
		return "", false, fmt.Errorf("%s already exists.\nRe-run with --force to replace it (the current one is kept until the new one is built and checked), or --skip-existing to leave it alone", venv)
	case statErr == nil && opts.Policy == OverwriteSkip:
		logf(opts.Log, "%s already exists; leaving it alone\n", venv)
		return venv, false, nil
	case statErr != nil && !os.IsNotExist(statErr):
		return "", false, statErr
	}

	// Build beside the destination, on the same filesystem, so swapping it
	// into place is a rename and never a half-copied directory.
	staging, err := os.MkdirTemp(project, ".venv.ofpack-")
	if err != nil {
		return "", false, err
	}
	newVenv := filepath.Join(staging, "venv")
	defer os.RemoveAll(staging)

	if err := buildVenv(dest, newVenv, m.UVProject, opts.Log); err != nil {
		return "", false, err
	}

	// Only once the new environment is built and checked is the old one
	// touched at all.
	var backup string
	if _, statErr := os.Lstat(venv); statErr == nil {
		backup = filepath.Join(staging, "previous")
		if err := os.Rename(venv, backup); err != nil {
			return "", false, fmt.Errorf("setting the existing %s aside: %w", VenvDir, err)
		}
	}
	if err := os.Rename(newVenv, venv); err != nil {
		if backup != "" {
			if restoreErr := os.Rename(backup, venv); restoreErr != nil {
				return "", false, fmt.Errorf("%w (and the previous %s could not be put back: %v; it is in %s)", err, VenvDir, restoreErr, backup)
			}
		}
		return "", false, err
	}
	return venv, true, nil
}

// buildVenv creates the environment and installs the closure into it,
// offline. uv is used because that is what the project uses; the Python and
// uv both come from the archive.
func buildVenv(dest, venv string, p *UVProject, log io.Writer) error {
	env, err := snapshotEnv(dest)
	if err != nil {
		return err
	}
	uvBin, err := toolBin(dest, env, "uv", p.UvVersion)
	if err != nil {
		return err
	}
	pyBin, err := toolBin(dest, env, "python", p.PythonVersion)
	if err != nil {
		return err
	}

	run := func(what string, args ...string) error {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Env = env
		cmd.Dir = dest
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("%s: %w\n%s", what, err, strings.TrimSpace(string(out)))
		}
		logf(log, "  %s\n", what)
		return nil
	}

	// --relocatable is what makes building elsewhere and moving into place
	// safe. Without it uv writes the build-time path into every console
	// script's shebang, so an environment built in a staging directory has a
	// working python but a broken `ansible`, and the breakage only shows
	// when something is run.
	if err := run("creating "+VenvDir, uvBin, "venv", venv, "--relocatable", "--python", pyBin); err != nil {
		return err
	}
	args := []string{
		uvBin, "pip", "install",
		"--python", filepath.Join(venv, "bin", "python"),
		"--offline", "--no-index",
		"--find-links", filepath.Join(dest, filepath.FromSlash(WheelhouseDir)),
		"-r", filepath.Join(dest, filepath.FromSlash(RequirementsName)),
	}
	if p.HasLock {
		args = append(args, "-c", filepath.Join(dest, filepath.FromSlash(ConstraintsName)))
	}
	if err := run("installing "+fmt.Sprint(p.Wheels)+" wheels offline", args...); err != nil {
		return err
	}
	return run("checking the environment", uvBin, "pip", "check", "--python", filepath.Join(venv, "bin", "python"))
}

// toolBin finds an executable the archive carries, via the bundled mise, and
// checks that it really is the one the archive carries.
//
// mise reads configuration this cannot shut out - /etc/mise/config.toml, a
// root /mise.toml - so `which` can legitimately answer with the machine's own
// Python or uv. Building the environment with those while believing the
// archive's were used is the failure worth refusing: the result would be an
// environment nobody packed.
func toolBin(dest string, env []string, tool, wantVersion string) (string, error) {
	cmd := exec.Command(filepath.Join(dest, filepath.FromSlash(MiseBinName)), "-C", "/", "which", tool)
	cmd.Env = env
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("the archive does not provide %s: %w", tool, withStderr(err))
	}
	path := strings.TrimSpace(string(out))
	if path == "" {
		return "", fmt.Errorf("the archive does not provide %s", tool)
	}
	if err := checkToolProvenance(dest, tool, wantVersion, path); err != nil {
		return "", err
	}
	return path, nil
}

// checkToolProvenance confirms a resolved executable lives under the
// archive's own install directory for that tool and version.
func checkToolProvenance(dest, tool, wantVersion, resolved string) error {
	if wantVersion == "" {
		return fmt.Errorf("the archive records no version for %s, so the one that was resolved cannot be checked; re-pack with a current ofpack", tool)
	}
	want := filepath.Join(dest, InstallsPrefix, tool, wantVersion)
	abs, err := filepath.Abs(resolved)
	if err != nil {
		return err
	}
	if !isUnder(filepath.Clean(want), filepath.Clean(abs)) {
		return fmt.Errorf("%s resolved to %s, which is not the %s %s the archive carries (expected it under %s).\nSomething outside the archive - a system mise config, for instance - is providing it, and the environment would not be the one that was packed",
			tool, resolved, tool, wantVersion, want)
	}
	return nil
}

// checkProjectMatches refuses to build an environment for a project whose
// dependency declaration has changed since the archive was made.
func checkProjectMatches(project string, p *UVProject) error {
	// The manifest comes out of an archive, which is not a trusted input: a
	// hash that is empty or the wrong length must be a plain error, not a
	// slice out of range.
	if err := checkRecordedHash("pyproject_sha256", p.PyprojectSHA256, true); err != nil {
		return err
	}
	if err := checkRecordedHash("lock_sha256", p.LockSHA256, false); err != nil {
		return err
	}
	data, err := os.ReadFile(filepath.Join(project, projectFiles.Pyproject))
	if err != nil {
		return fmt.Errorf("%s: %w", projectFiles.Pyproject, err)
	}
	if got := sha256Hex(data); got != p.PyprojectSHA256 {
		return fmt.Errorf("%s has changed since the archive was packed (%s now, %s then).\nThe wheels in the archive are the ones the old declaration needed, so the environment would not match. Re-pack, or restore into a checkout that matches",
			projectFiles.Pyproject, short(got), short(p.PyprojectSHA256))
	}
	if p.LockSHA256 == "" {
		return nil
	}
	lock, err := os.ReadFile(filepath.Join(project, projectFiles.Lock))
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("the archive was packed with a %s, but this project has none", projectFiles.Lock)
		}
		return err
	}
	if got := sha256Hex(lock); got != p.LockSHA256 {
		return fmt.Errorf("%s has changed since the archive was packed (%s now, %s then); re-pack, or restore into a checkout that matches",
			projectFiles.Lock, short(got), short(p.LockSHA256))
	}
	return nil
}

// checkRecordedHash rejects a manifest whose recorded digest is not a
// sha-256. Required says whether its absence is itself an error.
func checkRecordedHash(field, value string, required bool) error {
	if value == "" {
		if required {
			return fmt.Errorf("the archive records no %s, so there is no way to tell whether this project still matches it; re-pack with a current ofpack", field)
		}
		return nil
	}
	if len(value) != 64 {
		return fmt.Errorf("the archive records a malformed %s (%d characters, expected 64); it was not written by ofpack or has been altered", field, len(value))
	}
	for _, r := range value {
		if !strings.ContainsRune("0123456789abcdef", r) {
			return fmt.Errorf("the archive records a malformed %s (not hexadecimal); it was not written by ofpack or has been altered", field)
		}
	}
	return nil
}

// short abbreviates a digest for a message without assuming its length.
func short(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	if h == "" {
		return "(none)"
	}
	return h
}

// readInstalledManifest reads the manifest unpack left in dest.
func readInstalledManifest(dest string) (*Manifest, error) {
	data, err := os.ReadFile(filepath.Join(dest, InstalledManifestName))
	if err != nil {
		return nil, fmt.Errorf("%s does not look like an unpacked ofpack directory: %w", dest, err)
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// snapshotEnv is the environment the archive's tools run in: the same pinned
// one the generated scripts build.
func snapshotEnv(dest string) ([]string, error) {
	if _, err := os.Stat(filepath.Join(dest, filepath.FromSlash(MiseBinName))); err != nil {
		return nil, fmt.Errorf("%s does not look like an unpacked ofpack directory: %w", dest, err)
	}
	return append(scrubbedEnv(),
		"MISE_DATA_DIR="+dest,
		"MISE_CONFIG_DIR="+filepath.Join(dest, "config"),
		"MISE_GLOBAL_CONFIG_FILE="+filepath.Join(dest, "config", "config.toml"),
		"MISE_OFFLINE=1",
	), nil
}

// PythonSpecFor selects the lowest Python minor admitted by requires-python.
// Dependency resolution remains authoritative: if packages require a newer
// interpreter, pack fails instead of silently changing this selection.
func PythonSpecFor(s *UVSource) (string, error) {
	requires := RequiresPython(s)
	if requires == "" {
		return "", fmt.Errorf("%s states no requires-python lower bound; pass --python X.Y or set [tools].python in a scoped or global mise config", projectFiles.Pyproject)
	}
	spec, err := PythonFloorForLatest(requires)
	if err != nil {
		return "", err
	}
	if spec == "" {
		return "", fmt.Errorf("requires-python = %q names no Python version to install (an upper bound alone says only which versions are excluded); pass --python X.Y", requires)
	}
	return spec, nil
}

// PythonFloorForLatest uses the lowest minor allowed by a declared lower
// bound when mise says "latest". With no lower bound, mise keeps its normal
// latest behavior. A contradictory range is rejected before any download.
func PythonFloorForLatest(requires string) (string, error) {
	var floor string
	for _, part := range strings.Split(requires, ",") {
		op, version := splitSpecifier(part)
		if op != ">=" && op != ">" && op != "==" && op != "~=" {
			continue
		}
		candidate, ok := majorMinor(version)
		if !ok {
			continue
		}
		if op == ">" {
			major, minor := versionParts(candidate)
			candidate = fmt.Sprintf("%d.%d", major, minor+1)
		}
		if floor == "" {
			floor = candidate
		} else {
			major, minor := versionParts(candidate)
			floorMajor, floorMinor := versionParts(floor)
			if compareMinor(major, minor, floorMajor, floorMinor) > 0 {
				floor = candidate
			}
		}
	}
	if floor != "" {
		if ok, why := PythonSatisfies(floor, requires); !ok {
			return "", fmt.Errorf("requires-python = %q has conflicting constraints at its Python floor %s: %s", requires, floor, why)
		}
	}
	return floor, nil
}

// pythonMinorPinned recognizes an exact minor or an upper bound that closes
// immediately after the candidate minor. It deliberately errs toward asking
// for --python when a specifier is more complicated than this.
func pythonMinorPinned(requires, candidate string) bool {
	major, minor := versionParts(candidate)
	next := fmt.Sprintf("%d.%d", major, minor+1)
	for _, part := range strings.Split(requires, ",") {
		op, version := splitSpecifier(part)
		bound, ok := majorMinor(version)
		if !ok {
			continue
		}
		switch {
		case op == "==" && bound == candidate:
			return true
		case op == "~=" && bound == candidate && strings.Count(strings.TrimSpace(version), ".") >= 2:
			return true
		case op == "<" && bound == next:
			return true
		case op == "<=" && bound == candidate:
			return true
		}
	}
	return false
}

// pythonSpecFromRequires derives a "X.Y" to install from a requires-python
// specifier set.
//
// Only constraints that name a usable floor are considered. An upper bound
// says which versions are *excluded*: reading "<3.12" as "3.12" picks the
// first version the project has ruled out, so such constraints are skipped
// and an unsatisfiable set yields nothing rather than a wrong guess.
func pythonSpecFromRequires(req string) string {
	for _, part := range strings.Split(req, ",") {
		op, version := splitSpecifier(part)
		switch op {
		case "==", ">=", "~=", "":
			if v, ok := majorMinor(version); ok {
				return v
			}
		}
	}
	return ""
}

// splitSpecifier separates "<=3.13" into "<=" and "3.13".
func splitSpecifier(part string) (op, version string) {
	part = strings.TrimSpace(part)
	i := 0
	for i < len(part) && strings.ContainsRune("=<>~!^", rune(part[i])) {
		i++
	}
	return part[:i], strings.TrimSpace(part[i:])
}

// majorMinor reduces "3.12.*" or "3.12.1" to "3.12".
func majorMinor(v string) (string, bool) {
	v = strings.TrimSuffix(strings.TrimSpace(v), ".*")
	fields := strings.Split(v, ".")
	if len(fields) >= 2 && isDigits(fields[0]) && isDigits(fields[1]) {
		return fields[0] + "." + fields[1], true
	}
	return "", false
}

// PythonSatisfies reports whether a chosen "X.Y" meets a requires-python
// specifier set, so a version picked by hand with --python cannot quietly
// contradict the project.
//
// Comparison is on major and minor only, which is the granularity ofpack
// installs at. A patch-level bound such as ">=3.12.4" is treated as its
// minor, which can accept a slightly older patch; that is a deliberate
// limitation of the granularity, not a silent mismatch of the minor version.
func PythonSatisfies(spec, requires string) (bool, string) {
	if strings.TrimSpace(requires) == "" {
		return true, ""
	}
	want, ok := majorMinor(spec)
	if !ok {
		return false, fmt.Sprintf("%q is not a Python X.Y version", spec)
	}
	wMaj, wMin := versionParts(want)
	for _, part := range strings.Split(requires, ",") {
		op, version := splitSpecifier(part)
		bound, ok := majorMinor(version)
		if !ok {
			continue
		}
		bMaj, bMin := versionParts(bound)
		cmp := compareMinor(wMaj, wMin, bMaj, bMin)
		bad := false
		switch op {
		case "", "==":
			bad = cmp != 0
		case ">=":
			bad = cmp < 0
		case ">":
			bad = cmp <= 0
		case "<=":
			bad = cmp > 0
		case "<":
			// An upper bound is exclusive at the minor it names.
			bad = cmp >= 0
		case "!=":
			bad = cmp == 0
		case "~=":
			bad = cmp < 0 || wMaj != bMaj
		}
		if bad {
			return false, fmt.Sprintf("python %s does not satisfy %q", spec, strings.TrimSpace(part))
		}
	}
	return true, ""
}

// ConcretePythonVersion distinguishes a numeric mise selector from aliases
// such as "latest", which can only be checked after target-side resolution.
func ConcretePythonVersion(spec string) bool {
	parts := strings.Split(spec, ".")
	if len(parts) != 2 && len(parts) != 3 {
		return false
	}
	for _, part := range parts {
		if !isDigits(part) {
			return false
		}
	}
	return true
}

func versionParts(v string) (int, int) {
	var maj, min int
	fields := strings.Split(v, ".")
	for i, f := range fields {
		n := 0
		for _, r := range f {
			n = n*10 + int(r-'0')
		}
		if i == 0 {
			maj = n
		} else if i == 1 {
			min = n
		}
	}
	return maj, min
}

func compareMinor(aMaj, aMin, bMaj, bMin int) int {
	switch {
	case aMaj != bMaj:
		if aMaj < bMaj {
			return -1
		}
		return 1
	case aMin != bMin:
		if aMin < bMin {
			return -1
		}
		return 1
	}
	return 0
}

// RequiresPython is the project's own statement of which Pythons it accepts.
func RequiresPython(s *UVSource) string {
	if v, ok := tomlStringValue(string(s.Pyproject), "project", "requires-python"); ok {
		return v
	}
	if s.HasLock {
		for _, line := range strings.Split(string(s.Lock), "\n") {
			if t := strings.TrimSpace(line); strings.HasPrefix(t, "requires-python = ") {
				return unquoteTOML(strings.TrimPrefix(t, "requires-python = "))
			}
		}
	}
	return ""
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// AttachUVProject records the packed environment in the manifest, taking the
// versions and the wheel count from the payload the container produced
// rather than from what was asked for.
func AttachUVProject(plan *Plan, s *UVSource, payloadPath string) (*UVProject, error) {
	rec := &UVProject{
		Requirements:    s.Requirements,
		Constraints:     s.Constraints,
		HasLock:         s.HasLock,
		PyprojectSHA256: s.PyprojectSHA,
		LockSHA256:      s.LockSHA,
	}
	for _, t := range plan.Manifest.Tools {
		switch t.Name {
		case "python":
			rec.PythonVersion = t.Version
		case "uv":
			rec.UvVersion = t.Version
		}
	}
	if rec.PythonVersion == "" {
		return nil, fmt.Errorf("the container produced no python; the archive would be unusable")
	}
	if rec.UvVersion == "" {
		return nil, fmt.Errorf("the container produced no uv; the archive could not restore the environment")
	}
	if err := scanPayload(payloadPath, func(pe payloadEntry) error {
		if strings.HasPrefix(pe.name, WheelhouseDir+"/") && strings.HasSuffix(pe.name, ".whl") {
			rec.Wheels++
		}
		return nil
	}); err != nil {
		return nil, err
	}
	if rec.Wheels == 0 {
		return nil, fmt.Errorf("the payload carries no wheels")
	}
	plan.Manifest.UVProject = rec
	// An older ofpack does not know about uv/ and would drop it while
	// reporting a successful unpack, so the archive declares a schema such
	// an ofpack refuses outright.
	plan.Manifest.Schema = SchemaVersionUV
	return rec, nil
}

// VerifyUVPayloadOffline installs the closure in a container with no network
// at all, so a missing wheel is found now rather than on the disconnected
// machine. It returns a short description of what was proven.
//
// The payload is extracted inside the container rather than on this machine:
// a Python distribution carries terminfo entries that differ only in case,
// which collide on a case-insensitive filesystem such as macOS's.
func VerifyUVPayloadOffline(payloadPath, engine, image, target string, logs io.Writer) (string, error) {
	if engine == "" {
		engine = "podman"
	}
	if image == "" {
		image = DefaultCrossImage
	}
	payloadPath, err := filepath.Abs(payloadPath)
	if err != nil {
		return "", err
	}
	script := "mkdir -p /payload && tar -xzf /payload.tar.gz -C /payload\n" + UVVerifyScript()
	cmd := exec.Command(engine, "run", "--rm", "-i",
		"--network", "none", // the whole point of the check
		"--platform", target,
		"-v", payloadPath+":/payload.tar.gz:ro",
		image, "sh", "-euc", script)
	cmd.Stdout, cmd.Stderr = logs, logs
	if err := cmd.Run(); err != nil {
		return "", err
	}
	return "pip install --no-index in a --network none " + target + " container", nil
}
