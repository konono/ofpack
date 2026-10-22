package mpack

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

// PackOptions controls a pack run.
type PackOptions struct {
	// WorkDir is the directory whose effective (global + local) mise state is
	// packed. Defaults to the process working directory.
	WorkDir string
	// Exclude holds mise tool names to leave out.
	Exclude []string
	// AllowUnresolved downgrades "symlink leaves the packed set" from a fatal
	// error to a recorded warning. Off by default so a broken archive is never
	// produced silently.
	AllowUnresolved bool
	// Version is the mpack build version recorded in the manifest.
	Version string
	// Log receives human readable progress; may be nil.
	Log io.Writer
}

// Plan is the fully resolved description of what an archive will contain. It
// is computed before anything is written so that policy violations abort
// before any output exists.
type Plan struct {
	Manifest     Manifest
	Env          Env
	InstallsRoot string
	// Units are the top level subtrees to archive, relative to InstallsRoot.
	Units []string
	// entries is every archived item, in archive order.
	entries []entry

	// payloadPath, when set, is a gzipped tar from a target-platform
	// container holding the files instead of the local filesystem. The
	// symlink maths still uses InstallsRoot, which is then the root *inside
	// the container*, because that is what the links were written against.
	payloadPath string
	// byName indexes the payload entries that survived classification.
	byName map[string]entry
	// unpacker, when set, is an mpack binary that runs on the target; it is
	// archived as bin/mpack so the target needs nothing but tar to bootstrap.
	unpacker []byte
}

type entry struct {
	// name is the path inside the archive.
	name string
	// src is the absolute source path.
	src  string
	info os.FileInfo
	// linkTarget is the target to store for symlinks, already rewritten.
	linkTarget string
}

// BuildPlan queries mise, scans the install trees and classifies every symlink.
func BuildPlan(opts PackOptions) (*Plan, error) {
	workDir := opts.WorkDir
	if workDir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return nil, err
		}
		workDir = wd
	}
	workDir, err := filepath.Abs(workDir)
	if err != nil {
		return nil, err
	}

	env, err := LookupEnv()
	if err != nil {
		return nil, err
	}
	tools, installsRoot, err := CurrentTools(env, workDir)
	if err != nil {
		return nil, err
	}

	excluded := map[string]bool{}
	for _, name := range opts.Exclude {
		excluded[name] = true
	}
	kept := tools[:0:0]
	for _, t := range tools {
		if excluded[t.Name] {
			delete(excluded, t.Name)
			logf(opts.Log, "excluding %s@%s\n", t.Name, t.Version)
			continue
		}
		kept = append(kept, t)
	}
	for name := range excluded {
		return nil, fmt.Errorf("--exclude %s: no such active tool", name)
	}
	if len(kept) == 0 {
		return nil, fmt.Errorf("every active tool was excluded; nothing to pack")
	}

	units := make([]string, 0, len(kept)+1)
	for _, t := range kept {
		units = append(units, t.RelPath)
	}
	hasCatalog := false
	if fi, err := os.Lstat(filepath.Join(installsRoot, CatalogDir)); err == nil && fi.IsDir() {
		hasCatalog = true
		units = append(units, CatalogDir)
	}
	sort.Strings(units)

	envNames, err := EnvVarNames(env, workDir)
	if err != nil {
		return nil, err
	}

	plan := &Plan{
		Env:          env,
		InstallsRoot: installsRoot,
		Units:        units,
		Manifest: Manifest{
			Schema:       SchemaVersion,
			MpackVersion: opts.Version,
			CreatedAt:    time.Now().UTC().Format(time.RFC3339),
			OS:           runtime.GOOS,
			Arch:         runtime.GOARCH,
			MiseVersion:  env.Version,

			HomeRelativeProjectPath: relToHome(workDir),
			Tools:                   kept,
			HasCatalog:              hasCatalog,
			EnvVarNames:             envNames,
		},
	}

	if err := plan.scan(opts); err != nil {
		return nil, err
	}
	return plan, nil
}

// inPackedSet reports whether a path relative to the installs root is inside
// one of the subtrees this archive will contain. A symlink that stays inside
// the installs root but points at a tool we are not packing would dangle after
// relocation, so it has to be caught here too.
func (p *Plan) inPackedSet(rel string) bool {
	for _, u := range p.Units {
		if rel == u || strings.HasPrefix(rel, u+"/") {
			return true
		}
	}
	return false
}

// BuildPlanFromPayload turns a container payload into a plan. The tools were
// installed for the target platform inside the container; everything from
// here on - classification, the manifest, the archive itself - runs on this
// machine, so no target-platform mpack is needed to pack.
func BuildPlanFromPayload(payloadPath string, target string, opts PackOptions) (*Plan, error) {
	goos, goarch, err := ResolveTarget(target)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(payloadPath)
	if err != nil {
		return nil, err
	}
	meta, err := readPayloadMeta(f)
	f.Close()
	if err != nil {
		return nil, err
	}
	tools, err := payloadToolsFromMeta(meta)
	if err != nil {
		return nil, err
	}

	excluded := map[string]bool{}
	for _, name := range opts.Exclude {
		excluded[name] = true
	}
	kept := tools[:0:0]
	for _, t := range tools {
		if excluded[t.Name] {
			delete(excluded, t.Name)
			continue
		}
		kept = append(kept, t)
	}
	for name := range excluded {
		return nil, fmt.Errorf("--exclude %s: the container did not install a tool by that name", name)
	}
	if len(kept) == 0 {
		return nil, fmt.Errorf("every tool the container installed was excluded; nothing to pack")
	}

	workDir := opts.WorkDir
	if workDir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return nil, err
		}
		workDir = wd
	}
	workDir, err = filepath.Abs(workDir)
	if err != nil {
		return nil, err
	}

	units := make([]string, 0, len(kept)+1)
	for _, t := range kept {
		units = append(units, t.RelPath)
	}
	sort.Strings(units)

	p := &Plan{
		InstallsRoot: meta.installsRoot,
		Units:        units,
		payloadPath:  payloadPath,
		Manifest: Manifest{
			Schema:       SchemaVersion,
			MpackVersion: opts.Version,
			CreatedAt:    time.Now().UTC().Format(time.RFC3339),
			// The archive describes the platform it was built for, which is
			// the container's, not this machine's.
			OS:                      goos,
			Arch:                    goarch,
			MiseVersion:             meta.miseVersion,
			Tools:                   kept,
			EnvVarNames:             meta.envNames,
			HomeRelativeProjectPath: relToHome(workDir),
		},
	}
	if err := p.scanPayloadEntries(opts); err != nil {
		return nil, err
	}
	return p, nil
}

// scanPayloadEntries classifies everything the container sent.
func (p *Plan) scanPayloadEntries(opts PackOptions) error {
	var fixed, unresolved []LinkNote
	p.byName = map[string]entry{}
	hasMise := false

	err := scanPayload(p.payloadPath, func(pe payloadEntry) error {
		switch {
		case pe.name == MiseBinName:
			hasMise = true
			p.byName[pe.name] = entry{name: pe.name}
			return nil
		case pe.name == InstallsPrefix || strings.HasPrefix(pe.name, InstallsPrefix+"/"):
		default:
			return nil // nothing else from the container is archived
		}
		rel, ok := strings.CutPrefix(pe.name, InstallsPrefix+"/")
		if !ok {
			return nil // the installs directory entry itself
		}
		if rel == CatalogDir || strings.HasPrefix(rel, CatalogDir+"/") {
			if !p.Manifest.HasCatalog {
				p.Manifest.HasCatalog = true
				p.Units = append(p.Units, CatalogDir)
				sort.Strings(p.Units)
			}
		} else if !p.inPackedSet(rel) {
			return nil // an excluded tool, or one mise did not activate
		}

		e := entry{name: pe.name}
		switch pe.hdr.Typeflag {
		case tar.TypeSymlink:
			// Resolve against the container path the link was written at.
			src := path.Join(p.InstallsRoot, rel)
			stored, note, kind, err := p.classifyLink(src, rel, pe.hdr.Linkname)
			if err != nil {
				return err
			}
			switch kind {
			case linkFixed:
				fixed = append(fixed, note)
			case linkUnresolved:
				unresolved = append(unresolved, note)
				return nil
			}
			e.linkTarget = stored
			p.Manifest.Stats.Symlinks++
		case tar.TypeDir:
			p.Manifest.Stats.Dirs++
		default:
			p.Manifest.Stats.Files++
			p.Manifest.Stats.Bytes += pe.hdr.Size
		}
		p.byName[pe.name] = e
		return nil
	})
	if err != nil {
		return err
	}
	if !hasMise {
		return fmt.Errorf("the container payload has no %s", MiseBinName)
	}
	// Metadata can name a tool the payload does not actually carry, for
	// instance if tar failed part way through. Writing that out would make a
	// well formed archive with a tool directory missing from it, so the gap
	// is caught here instead.
	var missing []string
	for _, u := range p.Units {
		if _, ok := p.byName[path.Join(InstallsPrefix, u)]; !ok {
			missing = append(missing, u)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("the container payload is incomplete: no files for %s", strings.Join(missing, ", "))
	}
	p.Manifest.RewrittenLinks = fixed
	p.Manifest.UnresolvedLinks = unresolved
	return p.reportUnresolved(unresolved, opts)
}

func (p *Plan) scan(opts PackOptions) error {
	var fixed, unresolved, dangling []LinkNote

	for _, unit := range p.Units {
		unitAbs := filepath.Join(p.InstallsRoot, unit)
		err := filepath.WalkDir(unitAbs, func(src string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, ok := RelUnder(p.InstallsRoot, src)
			if !ok {
				return fmt.Errorf("%s is outside the installs root", src)
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			e := entry{name: path.Join(InstallsPrefix, rel), src: src, info: info}

			switch {
			case info.Mode()&os.ModeSymlink != 0:
				target, err := os.Readlink(src)
				if err != nil {
					return err
				}
				stored, note, kind, err := p.classifyLink(src, rel, target)
				if err != nil {
					return err
				}
				switch kind {
				case linkFixed:
					fixed = append(fixed, note)
				case linkUnresolved:
					unresolved = append(unresolved, note)
					return nil // not archived; recorded instead
				}
				if _, err := os.Stat(src); err != nil {
					dangling = append(dangling, LinkNote{Path: rel, Target: scrubHome(target), Reason: "target does not exist on the source machine"})
				}
				e.linkTarget = stored
				p.Manifest.Stats.Symlinks++
			case info.IsDir():
				p.Manifest.Stats.Dirs++
			case info.Mode().IsRegular():
				p.Manifest.Stats.Files++
				p.Manifest.Stats.Bytes += info.Size()
			default:
				return fmt.Errorf("%s: unsupported file type %s (mpack archives regular files, directories and symlinks only)", src, info.Mode().Type())
			}
			p.entries = append(p.entries, e)
			return nil
		})
		if err != nil {
			return err
		}
	}

	sort.Slice(p.entries, func(i, j int) bool { return p.entries[i].name < p.entries[j].name })
	p.Manifest.RewrittenLinks = fixed
	p.Manifest.DanglingLinks = dangling
	p.Manifest.UnresolvedLinks = unresolved

	return p.reportUnresolved(unresolved, opts)
}

// reportUnresolved turns links that cannot survive relocation into an error,
// unless the caller opted in to packing without them.
func (p *Plan) reportUnresolved(unresolved []LinkNote, opts PackOptions) error {
	if len(unresolved) == 0 {
		return nil
	}
	if !opts.AllowUnresolved {
		var b strings.Builder
		fmt.Fprintf(&b, "%d symlink(s) cannot be relocated:\n", len(unresolved))
		for _, n := range unresolved {
			target := n.Target
			if target == "" {
				target = "(target outside the home directory, omitted from the manifest)"
			}
			fmt.Fprintf(&b, "  %s -> %s (%s)\n", n.Path, target, n.Reason)
		}
		b.WriteString("re-run with --exclude <tool> to drop the affected tool, or --allow-unresolved to pack anyway (the links will be missing on the target)")
		return fmt.Errorf("%s", b.String())
	}
	logf(opts.Log, "warning: packing without %d unrelocatable symlink(s); see the manifest\n", len(unresolved))
	return nil
}

type linkKind int

const (
	linkKept linkKind = iota
	linkFixed
	linkUnresolved
)

// classifyLink decides what to store for one symlink.
//
//	relative, stays inside the packed set -> stored verbatim
//	absolute, inside the packed set       -> stored as @mpack-root@/<rel>
//	anything else                         -> unresolved
func (p *Plan) classifyLink(src, rel, target string) (stored string, note LinkNote, kind linkKind, err error) {
	if target == "" {
		return "", LinkNote{Path: rel, Reason: "empty symlink target"}, linkUnresolved, nil
	}
	abs := target
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(filepath.Dir(src), target)
	}
	abs = filepath.Clean(abs)

	targetRel, ok := RelUnder(p.InstallsRoot, abs)
	if !ok {
		return "", LinkNote{Path: rel, Target: scrubHome(target), Reason: "points outside the mise installs root"}, linkUnresolved, nil
	}
	if !p.inPackedSet(targetRel) {
		return "", LinkNote{Path: rel, Target: scrubHome(target), Reason: "points to " + targetRel + ", which is not part of this archive"}, linkUnresolved, nil
	}
	if filepath.IsAbs(target) {
		stored = RootSentinel + "/" + targetRel
		return stored, LinkNote{Path: rel, Target: scrubHome(target), Reason: "absolute target rewritten to be relocatable"}, linkFixed, nil
	}
	return target, LinkNote{}, linkKept, nil
}

// Write serialises the plan as a gzip compressed tar stream.
func (p *Plan) Write(w io.Writer) error {
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)

	manifestJSON, err := json.MarshalIndent(p.Manifest, "", "  ")
	if err != nil {
		return err
	}
	manifestJSON = append(manifestJSON, '\n')
	now := time.Now().UTC()

	if err := writeBytes(tw, ManifestName, 0o644, manifestJSON, now); err != nil {
		return err
	}
	if len(p.unpacker) > 0 {
		if err := writeBytes(tw, UnpackerName, 0o755, p.unpacker, now); err != nil {
			return err
		}
	}
	if err := writeBytes(tw, ConfigName, 0o644, RenderConfig(p.Manifest.Tools), now); err != nil {
		return err
	}

	if p.payloadPath != "" {
		// The files came from a container; stream them straight through,
		// using the headers classification already approved.
		if err := copyPayloadBodies(p.payloadPath, p.byName, tw); err != nil {
			return err
		}
	} else {
		miseBin, err := os.ReadFile(p.Env.Bin)
		if err != nil {
			return err
		}
		if err := writeBytes(tw, MiseBinName, 0o755, miseBin, now); err != nil {
			return err
		}
		for _, e := range p.entries {
			if err := writeEntry(tw, e); err != nil {
				return fmt.Errorf("%s: %w", e.src, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

// SetUnpacker attaches an mpack binary that runs on the archive's target
// platform. It is stored as bin/mpack so the receiving machine can bootstrap
// with nothing but tar.
func (p *Plan) SetUnpacker(b []byte) {
	p.unpacker = b
	p.Manifest.HasUnpacker = len(b) > 0
}

// HasUnpacker reports whether a bootstrap binary is attached.
func (p *Plan) HasUnpacker() bool { return len(p.unpacker) > 0 }

func writeBytes(tw *tar.Writer, name string, mode int64, data []byte, mod time.Time) error {
	hdr := &tar.Header{
		Typeflag: tar.TypeReg,
		Name:     name,
		Mode:     mode,
		Size:     int64(len(data)),
		ModTime:  mod,
		Format:   tar.FormatPAX,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	_, err := tw.Write(data)
	return err
}

func writeEntry(tw *tar.Writer, e entry) error {
	hdr, err := tar.FileInfoHeader(e.info, e.linkTarget)
	if err != nil {
		return err
	}
	hdr.Name = e.name
	if e.info.IsDir() {
		hdr.Name += "/"
	}
	// Ownership from the source machine is meaningless on the target and would
	// leak the packer's account details, so it is zeroed out.
	hdr.Uid, hdr.Gid = 0, 0
	hdr.Uname, hdr.Gname = "", ""
	// Keep only the permission bits; setuid/setgid/sticky are dropped.
	hdr.Mode = int64(e.info.Mode().Perm())
	hdr.Format = tar.FormatPAX

	if e.info.Mode()&os.ModeSymlink != 0 {
		hdr.Typeflag = tar.TypeSymlink
		hdr.Linkname = e.linkTarget
		hdr.Size = 0
		return tw.WriteHeader(hdr)
	}
	if e.info.IsDir() {
		return tw.WriteHeader(hdr)
	}

	f, err := os.Open(e.src)
	if err != nil {
		return err
	}
	defer f.Close()
	// Re-stat through the open handle so the archived size matches the bytes
	// we actually copy even if the file changed between walk and read.
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	hdr.Size = fi.Size()
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	n, err := io.Copy(tw, io.LimitReader(f, fi.Size()))
	if err != nil {
		return err
	}
	if n < fi.Size() {
		// The file shrank mid-read; pad so the stream stays well formed.
		if _, err := tw.Write(make([]byte, fi.Size()-n)); err != nil {
			return err
		}
	}
	return nil
}

// relToHome expresses dir relative to the current user's home directory, or
// returns "" when dir is not under it. The home prefix itself is not recorded:
// it differs between machines and reveals the packer's account name.
func relToHome(dir string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	home, err = filepath.Abs(home)
	if err != nil {
		return ""
	}
	rel, ok := RelUnder(filepath.Clean(home), dir)
	if !ok || rel == "." {
		return ""
	}
	// Only record a value unpack will be willing to trust.
	if _, err := CheckArchiveName(rel); err != nil {
		return ""
	}
	return rel
}

func logf(w io.Writer, format string, args ...any) {
	if w == nil {
		return
	}
	fmt.Fprintf(w, format, args...)
}
