package blueprint

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"regexp"
	"slices"
	"strings"

	"github.com/erstaples/comfyform/graph"
)

// Form is how a package says what its blueprint builds.
type Form int

const (
	// FormGroup is a node group: graph.json holds the nodes and the manifest
	// says where ports and settings go.
	FormGroup Form = iota
	// FormProgram is a Starlark program: main.star builds the nodes.
	FormProgram
)

// MaxPackageBytes is the most a package's files may add up to.
const MaxPackageBytes = 1 << 20

// The files of a package that Load reads by name.
const (
	manifestFile = "blueprint.json"
	programFile  = "main.star"
	groupFile    = "graph.json"
)

// Package is a blueprint package read from a directory or an archive.
type Package struct {
	Manifest *Manifest
	Form     Form
	Group    graph.Prompt      // graph.json, FormGroup only
	Program  []byte            // main.star, FormProgram only
	Files    map[string][]byte // every counted file by slash path
	Digest   string
}

// Load reads a package, computes its digest, parses its manifest and graph,
// and runs Check(nil). It returns a Problems error when the manifest or the
// form checks fail, and a plain error when the files cannot be read: a
// symlink or other special file, a package over MaxPackageBytes, a missing
// blueprint.json, JSON that does not parse. Callers get the list of problems
// with errors.As.
//
// A zip.Reader and os.DirFS both satisfy fs.FS, which is how uploads and
// repository directories load alike.
func Load(fsys fs.FS) (*Package, error) {
	files, err := readFiles(fsys)
	if err != nil {
		return nil, err
	}
	raw, ok := files[manifestFile]
	if !ok {
		return nil, errors.New("package has no " + manifestFile)
	}
	m, err := ParseManifest(raw)
	if err != nil {
		return nil, err
	}
	p := &Package{Manifest: m, Files: files, Digest: Digest(files)}
	if star, ok := files[programFile]; ok {
		p.Form = FormProgram
		p.Program = star
	}
	if raw, ok := files[groupFile]; ok {
		p.Form = FormGroup
		if p.Group, err = parseGroup(raw); err != nil {
			return nil, err
		}
	}
	if ps := p.Check(nil); len(ps) > 0 {
		return nil, ps
	}
	return p, nil
}

// readFiles reads every counted file of a package. Names that start with a
// dot, and anything under a directory that does, are left out: they are the
// debris of the machine a package was made on (.DS_Store, .git) and do not
// count against MaxPackageBytes.
func readFiles(fsys fs.FS) (map[string][]byte, error) {
	files := map[string][]byte{}
	left := MaxPackageBytes
	err := fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path != "." && strings.HasPrefix(d.Name(), ".") {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		switch typ := d.Type(); {
		case d.IsDir():
			return nil
		case typ&fs.ModeSymlink != 0:
			return fmt.Errorf("%s is a symlink; a package holds regular files only", path)
		case !typ.IsRegular():
			return fmt.Errorf("%s is not a regular file (%v); a package holds regular files only", path, typ)
		}
		b, err := readCapped(fsys, path, left)
		if err != nil {
			return err
		}
		left -= len(b)
		files[path] = b
		return nil
	})
	if err != nil {
		return nil, err
	}
	return files, nil
}

// readCapped reads a file that may have at most max bytes left to it, without
// trusting the size the file system reports.
func readCapped(fsys fs.FS, path string, max int) ([]byte, error) {
	f, err := fsys.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, int64(max)+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if len(b) > max {
		return nil, fmt.Errorf("%s takes the package over %d bytes", path, MaxPackageBytes)
	}
	return b, nil
}

// parseGroup decodes a graph.json, rejecting fields it does not know.
func parseGroup(b []byte) (graph.Prompt, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var g graph.Prompt
	if err := dec.Decode(&g); err != nil {
		return nil, fmt.Errorf("parse %s: %w", groupFile, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("parse %s: unexpected data after the graph", groupFile)
	}
	return g, nil
}

// Digest is the identity of a package's files: "sha256:" and the hex of a
// sha256 over, for each file in order of slash path, the path, a NUL byte,
// the hex of the sha256 of the file and a newline. It is the construction
// comfyform-helper uses for the content of a node pack.
func Digest(files map[string][]byte) string {
	h := sha256.New()
	for _, name := range slices.Sorted(maps.Keys(files)) {
		sum := sha256.Sum256(files[name])
		io.WriteString(h, name+"\x00"+hex.EncodeToString(sum[:])+"\n")
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// resultPattern is a program's result: the names of the children it made,
// then the name of the node, joined with dots.
var resultPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+(\.[A-Za-z0-9_-]+)*$`)

// inLinkID is the node id that a link in graph.json takes when a port fills
// the input it feeds: ["in", 0]. Compile replaces such a link.
const inLinkID = "in"

// Check returns every problem the package has that needs more than the
// manifest: how it uses its form, and whether the manifest and the files
// agree. The problems come in the order the manifest declares what they are
// about (inputs, outputs, settings, result), then the nodes of graph.json.
//
// classes is the server's node classes, for checks against them; with nil
// Check reads the package alone.
func (p *Package) Check(classes Classes) Problems {
	var ps Problems
	_, hasStar := p.Files[programFile]
	_, hasGroup := p.Files[groupFile]
	if hasStar == hasGroup {
		ps.add("package", "exactly one of %s and %s", programFile, groupFile)
		return ps
	}
	if p.Form == FormProgram {
		p.checkProgram(&ps)
	} else {
		p.checkGroup(&ps)
	}
	return ps
}

func (p *Package) checkProgram(ps *Problems) {
	m := p.Manifest
	for i, in := range m.Inputs {
		if len(in.To) > 0 {
			ps.add(label("inputs", i, in.Name), "only a node group has to targets")
		}
	}
	for i, out := range m.Outputs {
		if out.From != nil {
			ps.add(label("outputs", i, out.Name), "only a node group has from")
		}
	}
	for i, s := range m.Settings {
		if len(s.To) > 0 {
			ps.add(label("settings", i, s.Name), "only a node group has to targets")
		}
	}
	if len(m.Repeats) > 0 {
		ps.add("repeats", "only a node group can repeat")
	}
	if m.Result != "" && !resultPattern.MatchString(m.Result) {
		ps.add("result", "result must be a dot-separated path of names made of letters, digits, - and _ (got %q)", m.Result)
	}
}

func (p *Package) checkGroup(ps *Problems) {
	m := p.Manifest
	for i, in := range m.Inputs {
		where := label("inputs", i, in.Name)
		if len(in.To) == 0 {
			ps.add(where, "input port %s has no targets", in.Name)
		}
		for _, t := range in.To {
			p.checkNode(ps, where, t.NodeID)
		}
	}
	for i, out := range m.Outputs {
		where := label("outputs", i, out.Name)
		if out.From == nil {
			ps.add(where, "output port %s has no from", out.Name)
			continue
		}
		p.checkNode(ps, where, out.From.NodeID)
	}
	for i, s := range m.Settings {
		where := label("settings", i, s.Name)
		if s.List != nil && len(m.Repeats) == 0 {
			ps.add(where, "a list setting needs repeats to run over it, and this blueprint has none")
		}
		for _, t := range s.To {
			if !p.checkNode(ps, where, t.NodeID) {
				continue
			}
			if _, ok := p.Group[t.NodeID].Inputs[t.Input]; !ok {
				ps.add(where, "node %q has no input %q", t.NodeID, t.Input)
			}
		}
	}
	if m.Result != "" {
		if _, ok := p.Group[m.Result]; !ok {
			ps.add("result", "result %q is not in %s", m.Result, groupFile)
		}
	}
	for _, id := range p.Group.IDs() {
		p.checkGroupNode(ps, id, p.Group[id])
	}
}

// checkNode reports a target that names a node graph.json does not have, and
// returns whether the node is there.
func (p *Package) checkNode(ps *Problems, where, id string) bool {
	if p.Group[id] == nil {
		ps.add(where, "node %q is not in %s", id, groupFile)
		return false
	}
	return true
}

// checkGroupNode checks one node of graph.json against the manifest.
func (p *Package) checkGroupNode(ps *Problems, id string, n *graph.Node) {
	where := fmt.Sprintf("%s node %q", groupFile, id)
	if n == nil {
		ps.add(where, "node is null")
		return
	}
	if id == inLinkID {
		ps.add(where, "the id %q is reserved for links that a port fills", inLinkID)
	}
	switch {
	case n.Class == "":
		ps.add(where, "class_type is empty")
	case strings.HasPrefix(n.Class, classPrefix):
		p.checkInstance(ps, where, n)
	default:
		if _, ok := p.Manifest.Requires.Classes[n.Class]; !ok {
			ps.add(where, "%s is not in requires.classes", n.Class)
		}
	}
}

// checkInstance checks a node that is an instance of another blueprint: the
// manifest must pin that blueprint at the node's version, and the node must
// carry the digest the manifest pins.
func (p *Package) checkInstance(ps *Problems, where string, n *graph.Node) {
	id, version, ok := ParseClass(n.Class)
	if !ok {
		ps.add(where, "%q is not a well-formed blueprint class", n.Class)
		return
	}
	use, ok := p.Manifest.Uses[id]
	if !ok {
		ps.add(where, "%s is not in uses", id)
		return
	}
	if use.Version != version {
		ps.add(where, "uses pins %s at %s, not %s", id, use.Version, version)
	}
	var got string
	if n.Meta != nil {
		got = n.Meta.Blueprint
	}
	switch got {
	case use.Digest:
	case "":
		ps.add(where, "_meta.blueprint is empty; uses pins %s at %s", id, use.Digest)
	default:
		ps.add(where, "_meta.blueprint is %s; uses pins %s at %s", got, id, use.Digest)
	}
}
