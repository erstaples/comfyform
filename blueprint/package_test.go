package blueprint

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/erstaples/comfyform/graph"
)

const (
	refGroupDir = "testdata/packages/ref-group"
	hiresDir    = "testdata/packages/hires"
)

// dirFiles reads a fixture directory into a MapFS, so a test can add, change
// or drop files before it loads the package.
func dirFiles(t *testing.T, dir string) fstest.MapFS {
	t.Helper()
	fsys := fstest.MapFS{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		fsys[e.Name()] = &fstest.MapFile{Data: readFile(t, dir+"/"+e.Name())}
	}
	return fsys
}

func mustLoad(t *testing.T, fsys fs.FS) *Package {
	t.Helper()
	p, err := Load(fsys)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return p
}

// programManifest is a minimal manifest for a program package.
const programManifest = `{
  "format": 1,
  "id": "test/prog",
  "version": "1.0.0",
  "title": "Program",
  "inputs": [{ "name": "latent", "type": "LATENT" }],
  "outputs": [{ "name": "latent", "type": "LATENT" }],
  "settings": [{ "name": "images", "kind": "image", "list": { "min": 0, "max": 4 } }],
  "requires": { "classes": {} }
}`

func programFS() fstest.MapFS {
	return fstest.MapFS{
		"blueprint.json": {Data: []byte(programManifest)},
		"main.star":      {Data: []byte("def build(ctx):\n    pass\n")},
	}
}

func TestDigestConstruction(t *testing.T) {
	got := Digest(map[string][]byte{"blueprint.json": []byte("{}"), "graph.json": []byte("{}")})
	if got != "sha256:c5b0042923b59657cd525c3a8405747bf2f3fd34603fdadf4fb804d8e6453fb2" {
		t.Fatal(got)
	}
}

func TestDigestIsTheDocumentedConstruction(t *testing.T) {
	// Spelled out from the rule, not from the code: sha256 over, for each
	// file by sorted path, path NUL hex(sha256(bytes)) newline.
	sum := func(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }
	outer := sha256.New()
	outer.Write([]byte("a.md\x00" + sum("one") + "\n"))
	outer.Write([]byte("docs/b.md\x00" + sum("two") + "\n"))
	outer.Write([]byte("z\x00" + sum("") + "\n"))
	want := "sha256:" + hex.EncodeToString(outer.Sum(nil))
	got := Digest(map[string][]byte{"z": nil, "docs/b.md": []byte("two"), "a.md": []byte("one")})
	if got != want {
		t.Fatalf("Digest = %s, want %s", got, want)
	}
}

func TestDigestOfNoFiles(t *testing.T) {
	// sha256 of the empty string.
	const want = "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if got := Digest(nil); got != want {
		t.Fatalf("Digest(nil) = %s", got)
	}
}

func TestDigestChangesWithPathAndContent(t *testing.T) {
	base := Digest(map[string][]byte{"a": []byte("x")})
	for name, files := range map[string]map[string][]byte{
		"content":   {"a": []byte("y")},
		"path":      {"b": []byte("x")},
		"extra":     {"a": []byte("x"), "b": nil},
		"directory": {"d/a": []byte("x")},
	} {
		if got := Digest(files); got == base {
			t.Errorf("%s change left the digest at %s", name, got)
		}
	}
}

func TestLoadIgnoresDotfiles(t *testing.T) {
	plain := dirFiles(t, refGroupDir)
	noisy := dirFiles(t, refGroupDir)
	noisy[".DS_Store"] = &fstest.MapFile{Data: []byte("junk")}
	noisy[".git/config"] = &fstest.MapFile{Data: []byte("[core]")}
	noisy[".hidden/deeper/file.txt"] = &fstest.MapFile{Data: []byte("x")}
	noisy["docs/.keep"] = &fstest.MapFile{}

	want := mustLoad(t, plain)
	got := mustLoad(t, noisy)
	if got.Digest != want.Digest {
		t.Fatalf("Digest = %s, want %s", got.Digest, want.Digest)
	}
	if len(got.Files) != len(want.Files) {
		t.Fatalf("Files = %v", fileNames(got.Files))
	}
}

func fileNames(files map[string][]byte) []string {
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	return names
}

func TestLoadCountsFilesInSubdirectoriesBySlashPath(t *testing.T) {
	fsys := dirFiles(t, refGroupDir)
	fsys["docs/notes/intro.md"] = &fstest.MapFile{Data: []byte("hi")}
	p := mustLoad(t, fsys)
	if got := string(p.Files["docs/notes/intro.md"]); got != "hi" {
		t.Fatalf("Files = %v", fileNames(p.Files))
	}
	if p.Digest == mustLoad(t, dirFiles(t, refGroupDir)).Digest {
		t.Fatal("a file in a subdirectory left the digest alone")
	}
}

func TestReadmeChangesDigest(t *testing.T) {
	before := mustLoad(t, dirFiles(t, refGroupDir))
	fsys := dirFiles(t, refGroupDir)
	fsys["README.md"] = &fstest.MapFile{Data: []byte("# Reference images\n")}
	after := mustLoad(t, fsys)
	if before.Digest == after.Digest {
		t.Fatal("adding README.md left the digest alone")
	}
	fsys["README.md"] = &fstest.MapFile{Data: []byte("# Reference images!\n")}
	if again := mustLoad(t, fsys); again.Digest == after.Digest {
		t.Fatal("editing README.md left the digest alone")
	}
}

func TestLoadRefusesSymlink(t *testing.T) {
	fsys := dirFiles(t, refGroupDir)
	fsys["cover.png"] = &fstest.MapFile{Data: []byte("/etc/passwd"), Mode: fs.ModeSymlink}
	p, err := Load(fsys)
	if p != nil || err == nil || !strings.Contains(err.Error(), "symlink") || !strings.Contains(err.Error(), "cover.png") {
		t.Fatalf("Load = %v, %v; want an error naming the symlink", p, err)
	}
}

func TestLoadRefusesSymlinkOnDisk(t *testing.T) {
	for name, target := range map[string]string{"file": "blueprint.json", "directory": "."} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			for file, data := range dirFiles(t, refGroupDir) {
				if err := os.WriteFile(dir+"/"+file, data.Data, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Symlink(target, dir+"/link"); err != nil {
				t.Skipf("no symlinks here: %v", err)
			}
			if _, err := Load(os.DirFS(dir)); err == nil || !strings.Contains(err.Error(), "symlink") {
				t.Fatalf("Load err = %v, want a refusal naming the symlink", err)
			}
		})
	}
}

func TestLoadRefusesOtherNonRegularFiles(t *testing.T) {
	fsys := dirFiles(t, refGroupDir)
	fsys["pipe"] = &fstest.MapFile{Mode: fs.ModeNamedPipe}
	if p, err := Load(fsys); p != nil || err == nil || !strings.Contains(err.Error(), "pipe") {
		t.Fatalf("Load = %v, %v; want an error naming the file", p, err)
	}
}

func TestLoadIgnoresSymlinkDotfile(t *testing.T) {
	fsys := dirFiles(t, refGroupDir)
	fsys[".link"] = &fstest.MapFile{Mode: fs.ModeSymlink}
	mustLoad(t, fsys)
}

func TestLoadRefusesLarge(t *testing.T) {
	fsys := dirFiles(t, refGroupDir)
	fsys["cover.png"] = &fstest.MapFile{Data: make([]byte, MaxPackageBytes+1)}
	p, err := Load(fsys)
	if p != nil || err == nil || !strings.Contains(err.Error(), "1048576") {
		t.Fatalf("Load = %v, %v; want an error naming the limit", p, err)
	}
}

func TestLoadAcceptsExactlyTheLimit(t *testing.T) {
	fsys := dirFiles(t, refGroupDir)
	used := 0
	for _, f := range fsys {
		used += len(f.Data)
	}
	fsys["cover.png"] = &fstest.MapFile{Data: make([]byte, MaxPackageBytes-used)}
	mustLoad(t, fsys)

	fsys["cover.png"] = &fstest.MapFile{Data: make([]byte, MaxPackageBytes-used+1)}
	if _, err := Load(fsys); err == nil || !strings.Contains(err.Error(), "1048576") {
		t.Fatalf("one byte over the limit: err = %v", err)
	}
}

func TestLoadCapsTheTotalNotEachFile(t *testing.T) {
	fsys := dirFiles(t, refGroupDir)
	fsys["a.bin"] = &fstest.MapFile{Data: make([]byte, MaxPackageBytes/2)}
	fsys["b.bin"] = &fstest.MapFile{Data: make([]byte, MaxPackageBytes/2)}
	if _, err := Load(fsys); err == nil || !strings.Contains(err.Error(), "1048576") {
		t.Fatalf("err = %v, want the total over the limit refused", err)
	}
}

func TestLoadDoesNotCountDotfilesAgainstTheLimit(t *testing.T) {
	fsys := dirFiles(t, refGroupDir)
	fsys[".git/pack"] = &fstest.MapFile{Data: make([]byte, 3*MaxPackageBytes)}
	fsys[".big"] = &fstest.MapFile{Data: make([]byte, 3*MaxPackageBytes)}
	mustLoad(t, fsys)
}

func TestLoadFixtures(t *testing.T) {
	for _, dir := range []string{refGroupDir, hiresDir} {
		t.Run(dir, func(t *testing.T) {
			p := mustLoad(t, os.DirFS(dir))
			if p.Form != FormGroup {
				t.Fatalf("Form = %v, want FormGroup", p.Form)
			}
			if p.Program != nil {
				t.Fatalf("Program = %q on a node group", p.Program)
			}
			if len(p.Files) != 2 || p.Files["blueprint.json"] == nil || p.Files["graph.json"] == nil {
				t.Fatalf("Files = %v", fileNames(p.Files))
			}
			if p.Digest != Digest(p.Files) {
				t.Fatalf("Digest = %s, want %s", p.Digest, Digest(p.Files))
			}
			if len(p.Group) == 0 {
				t.Fatal("Group is empty")
			}
		})
	}
}

func TestLoadFixtureContents(t *testing.T) {
	ref := mustLoad(t, os.DirFS(refGroupDir))
	if ref.Manifest.ID != "test/ref-group" || len(ref.Group) != 4 {
		t.Fatalf("ref-group: id %q, %d nodes", ref.Manifest.ID, len(ref.Group))
	}
	if got := ref.Group["3"]; got.Class != "VAEEncode" {
		t.Fatalf("node 3 = %+v", got)
	}
	if n, i, ok := graph.AsLink(ref.Group["3"].Inputs["vae"]); !ok || n != "in" || i != 0 {
		t.Fatalf("node 3 vae = %v", ref.Group["3"].Inputs["vae"])
	}

	hires := mustLoad(t, os.DirFS(hiresDir))
	if hires.Manifest.ID != "test/hires" || len(hires.Group) != 2 {
		t.Fatalf("hires: id %q, %d nodes", hires.Manifest.ID, len(hires.Group))
	}
	if hires.Manifest.Class() != "bp:test/hires@1.0.0" {
		t.Fatal(hires.Manifest.Class())
	}
	if len(hires.Manifest.Repeats) != 0 {
		t.Fatalf("hires repeats = %+v", hires.Manifest.Repeats)
	}
	if got := hires.Group["2"].Inputs["steps"]; got != 14.0 {
		t.Fatalf("hires steps = %v", got)
	}
	if hires.Digest == ref.Digest {
		t.Fatal("two packages share a digest")
	}
}

func TestLoadProgram(t *testing.T) {
	p := mustLoad(t, programFS())
	if p.Form != FormProgram {
		t.Fatalf("Form = %v, want FormProgram", p.Form)
	}
	if string(p.Program) != "def build(ctx):\n    pass\n" {
		t.Fatalf("Program = %q", p.Program)
	}
	if p.Group != nil {
		t.Fatalf("Group = %v on a program", p.Group)
	}
}

func TestLoadFromZip(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, data := range map[string][]byte{
		"blueprint.json": readFile(t, hiresDir+"/blueprint.json"),
		"graph.json":     readFile(t, hiresDir+"/graph.json"),
		".DS_Store":      []byte("junk"),
	} {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		w.Write(data)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatal(err)
	}
	fromZip := mustLoad(t, zr)
	fromDir := mustLoad(t, os.DirFS(hiresDir))
	if fromZip.Digest != fromDir.Digest {
		t.Fatalf("zip digest %s, directory digest %s", fromZip.Digest, fromDir.Digest)
	}
}

// zipFS writes files into a zip archive and reads it back as an fs.FS, the
// way an upload loads.
func zipFS(t *testing.T, files map[string][]byte) fs.FS {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, name := range slices.Sorted(maps.Keys(files)) {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(files[name]); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatal(err)
	}
	return zr
}

// TestLoadRefusesControlCharactersInPaths: a path that holds a NUL and a
// newline can hold the digest lines of other files, so two packages with
// different files could share a digest. These two would.
func TestLoadRefusesControlCharactersInPaths(t *testing.T) {
	hexOf := func(b []byte) string {
		sum := sha256.Sum256(b)
		return hex.EncodeToString(sum[:])
	}
	m1 := readFile(t, hiresDir+"/blueprint.json")
	m2 := bytes.Replace(m1, []byte(`"title": "Hires pass"`), []byte(`"title": "Another pass"`), 1)
	g := readFile(t, hiresDir+"/graph.json")
	x, y := []byte("x"), []byte("y")
	// a's crafted path holds b's lines for "a" and blueprint.json; b's holds
	// the rest of a's crafted line and a's blueprint.json line.
	aName := "a\x00" + hexOf(x) + "\nblueprint.json\x00" + hexOf(m2) + "\nc"
	bName := "c\x00" + hexOf(y) + "\nblueprint.json"
	a := map[string][]byte{aName: y, manifestFile: m1, groupFile: g}
	b := map[string][]byte{"a": x, manifestFile: m2, bName: m1, groupFile: g}
	if Digest(a) != Digest(b) {
		t.Fatalf("the crafted packages no longer collide: %s and %s", Digest(a), Digest(b))
	}

	for _, c := range []struct {
		files map[string][]byte
		bad   string
	}{{a, aName}, {b, bName}} {
		p, err := Load(zipFS(t, c.files))
		if err == nil {
			t.Fatalf("Load accepted %q, digest %s", c.bad, p.Digest)
		}
		if want := fmt.Sprintf("%q", c.bad); !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not name %s", err, want)
		}
	}

	// Any byte below 0x20, and 0x7f, is refused, in a file's name or a
	// directory's; other bytes are not.
	for name, refused := range map[string]string{
		"a\tb": "a\tb", "a\x1fb": "a\x1fb", "a\x7fb": "a\x7fb", "docs/a\nb.md": "docs/a\nb.md", "a\rb/c": "a\rb",
	} {
		fsys := dirFiles(t, hiresDir)
		fsys[name] = &fstest.MapFile{Data: []byte("x")}
		if _, err := Load(fsys); err == nil || !strings.Contains(err.Error(), fmt.Sprintf("%q", refused)) {
			t.Errorf("Load with %q: %v", name, err)
		}
	}
	fsys := dirFiles(t, hiresDir)
	fsys["notes é ~.md"] = &fstest.MapFile{Data: []byte("x")}
	mustLoad(t, fsys)
}

// TestFixtureDigests pins each fixture's digest, so a change to how Load
// reads files or how Digest hashes them shows here.
func TestFixtureDigests(t *testing.T) {
	want := map[string]string{
		"hires":          "sha256:54498c0e4e7d1245fb36527db1726a408e008c9ce95135ca5325838b9233d657",
		"nested-group":   "sha256:2a4574b8110197c8a2474014ee041f3a8bd1e9c8853c3807b353c4aa520e128b",
		"nested-program": "sha256:35c2d842d56c8688d18d456b318d5690092dadaf590b2ef7a6de7374e3f0d7f6",
		"ref-group":      "sha256:c6b9b46df0779a5ea88ca9847f64f2cf7995ff8d5269155ac72c635d9472c9b0",
		"ref-program":    "sha256:aa36c040a68436139b51cd4e06f3451f8616332971e1ed8aab49d73aebf8d541",
		"refine-passes":  "sha256:ff7b61fe72cc2663d14f4c11fdc45aac7e87a7b5fb5b49a8b5566fa313dbe2ec",
	}
	entries, err := os.ReadDir("testdata/packages")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if got := mustLoad(t, os.DirFS("testdata/packages/"+e.Name())).Digest; got != want[e.Name()] {
			t.Errorf("%s digest = %s, want %s", e.Name(), got, want[e.Name()])
		}
	}
	if len(entries) != len(want) {
		t.Errorf("%d fixtures, %d digests pinned", len(entries), len(want))
	}
}

func TestLoadNeedsAManifest(t *testing.T) {
	fsys := dirFiles(t, refGroupDir)
	delete(fsys, "blueprint.json")
	p, err := Load(fsys)
	if p != nil || err == nil || !strings.Contains(err.Error(), "blueprint.json") {
		t.Fatalf("Load = %v, %v", p, err)
	}
	var ps Problems
	if errors.As(err, &ps) {
		t.Fatalf("an unreadable package is an error, not Problems: %v", err)
	}
}

func TestLoadReturnsManifestProblems(t *testing.T) {
	fsys := dirFiles(t, refGroupDir)
	fsys["blueprint.json"].Data = bytes.Replace(fsys["blueprint.json"].Data, []byte(`"format": 1`), []byte(`"format": 2`), 1)
	p, err := Load(fsys)
	var ps Problems
	if p != nil || !errors.As(err, &ps) || len(ps) != 1 || ps[0].Where != "format" {
		t.Fatalf("Load = %v, %v", p, err)
	}
}

func TestLoadReturnsFormProblems(t *testing.T) {
	fsys := dirFiles(t, refGroupDir)
	fsys["main.star"] = &fstest.MapFile{Data: []byte("x")}
	p, err := Load(fsys)
	var ps Problems
	if p != nil || !errors.As(err, &ps) || len(ps) != 1 || !strings.Contains(ps[0].Msg, "exactly one of main.star and graph.json") {
		t.Fatalf("Load = %v, %v", p, err)
	}
}

func TestLoadRefusesABadGraph(t *testing.T) {
	for name, graphJSON := range map[string]string{
		"not json":      `{"1": `,
		"trailing data": `{} {}`,
		"a list":        `[]`,
		"unknown field": `{"1": {"class_type": "VAEEncode", "inputs": {}, "colour": "red"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			fsys := dirFiles(t, refGroupDir)
			fsys["graph.json"].Data = []byte(graphJSON)
			p, err := Load(fsys)
			if p != nil || err == nil || !strings.Contains(err.Error(), "graph.json") {
				t.Fatalf("Load = %v, %v; want an error naming graph.json", p, err)
			}
		})
	}
}

// formCase mutates a freshly loaded package, then expects Check(nil) to
// return exactly one problem, with the given Where and a Msg containing want.
// An empty want expects no problems.
type formCase struct {
	name   string
	mutate func(p *Package)
	where  string
	want   string
}

func runFormCases(t *testing.T, load func(t *testing.T) *Package, cases []formCase) {
	t.Helper()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := load(t)
			c.mutate(p)
			ps := p.Check(nil)
			if c.want == "" {
				if len(ps) != 0 {
					t.Fatalf("want no problems, got:\n%v", ps)
				}
				return
			}
			if len(ps) != 1 {
				t.Fatalf("want one problem containing %q, got %d:\n%v", c.want, len(ps), ps)
			}
			if ps[0].Where != c.where {
				t.Errorf("Where = %q, want %q", ps[0].Where, c.where)
			}
			if !strings.Contains(ps[0].Msg, c.want) {
				t.Errorf("Msg = %q, want it to contain %q", ps[0].Msg, c.want)
			}
		})
	}
}

func loadRefGroup(t *testing.T) *Package { return mustLoad(t, os.DirFS(refGroupDir)) }
func loadHires(t *testing.T) *Package    { return mustLoad(t, os.DirFS(hiresDir)) }
func loadProgram(t *testing.T) *Package  { return mustLoad(t, programFS()) }

// addInstance puts a blueprint instance node "5" in the group and pins its
// blueprint in uses.
func addInstance(p *Package, class string, meta *graph.Meta, uses map[string]Use) {
	p.Group["5"] = &graph.Node{Class: class, Inputs: map[string]any{}, Meta: meta}
	p.Manifest.Uses = uses
}

func pinned(version string) map[string]Use {
	return map[string]Use{"test/hires": {Version: version, Digest: digest64}}
}

func TestFormProblems(t *testing.T) {
	runFormCases(t, loadRefGroup, []formCase{
		{"fixture is clean", func(p *Package) {}, "", ""},
		{"both forms", func(p *Package) { p.Files["main.star"] = []byte("x") }, "package", "exactly one of main.star and graph.json"},
		{"neither form", func(p *Package) { delete(p.Files, "graph.json") }, "package", "exactly one of main.star and graph.json"},

		// The vae port: moving the conditioning port's target would also leave
		// the repeat's carried input unlinked.
		{"input target node missing", func(p *Package) { p.Manifest.Inputs[1].To[0].NodeID = "9" },
			`inputs "vae"`, `node "9" is not in graph.json`},
		{"second input target node missing", func(p *Package) {
			p.Manifest.Inputs[0].To = append(p.Manifest.Inputs[0].To, Target{NodeID: "9", Input: "x"})
		}, `inputs "conditioning"`, `node "9" is not in graph.json`},
		{"input target input is not looked up", func(p *Package) { p.Manifest.Inputs[1].To[0].Input = "optional_extra" }, "", ""},
		{"carried input left without its port", func(p *Package) { p.Manifest.Inputs[0].To[0].Input = "optional_extra" },
			"repeats[0]", "carry input 4.conditioning is not linked"},
		{"input without targets", func(p *Package) { p.Manifest.Inputs[1].To = nil }, `inputs "vae"`, "input port vae has no targets"},
		{"input with an empty target list", func(p *Package) { p.Manifest.Inputs[1].To = []Target{} }, `inputs "vae"`, "input port vae has no targets"},

		{"output without from", func(p *Package) { p.Manifest.Outputs[0].From = nil }, `outputs "conditioning"`, "output port conditioning has no from"},
		{"output from a missing node", func(p *Package) { p.Manifest.Outputs[0].From.NodeID = "9" },
			`outputs "conditioning"`, `node "9" is not in graph.json`},

		{"setting target node missing", func(p *Package) { p.Manifest.Settings[1].To[0].NodeID = "9" },
			`settings "megapixels"`, `node "9" is not in graph.json`},
		{"setting target input missing", func(p *Package) { p.Manifest.Settings[1].To[0].Input = "nope" },
			`settings "megapixels"`, `node "2" has no input "nope"`},
		{"list setting without repeats", func(p *Package) { p.Manifest.Repeats = nil },
			`settings "images"`, "a list setting needs repeats"},

		{"result names a node", func(p *Package) {
			p.Group["5"] = &graph.Node{Class: "LoadImage", Inputs: map[string]any{"image": ""}}
			p.Manifest.Result = "5"
		}, "", ""},
		{"result names a repeated node", func(p *Package) { p.Manifest.Result = "4" }, "repeats[0]", "result is inside a repeat"},
		{"result names a missing node", func(p *Package) { p.Manifest.Result = "9" }, "result", `result "9" is not in graph.json`},

		{"class not in requires", func(p *Package) { delete(p.Manifest.Requires.Classes, "VAEEncode") },
			`graph.json node "3"`, "VAEEncode is not in requires.classes"},
		{"requires may list more than the graph uses", func(p *Package) { p.Manifest.Requires.Classes["KSampler"] = hash24 }, "", ""},
		{"class_type empty", func(p *Package) { p.Group["3"].Class = "" }, `graph.json node "3"`, "class_type is empty"},
		{"null node", func(p *Package) { p.Group["9"] = nil }, `graph.json node "9"`, "node is null"},
		{"node named in", func(p *Package) {
			p.Group["in"] = &graph.Node{Class: "LoadImage", Inputs: map[string]any{}}
		}, `graph.json node "in"`, `"in" is reserved`},

		{"instance pinned in uses", func(p *Package) {
			addInstance(p, "bp:test/hires@1.0.0", &graph.Meta{Blueprint: digest64}, pinned("1.0.0"))
		}, "", ""},
		{"instance not in uses", func(p *Package) {
			addInstance(p, "bp:test/hires@1.0.0", &graph.Meta{Blueprint: digest64}, nil)
		}, `graph.json node "5"`, "test/hires is not in uses"},
		{"instance at another version", func(p *Package) {
			addInstance(p, "bp:test/hires@1.1.0", &graph.Meta{Blueprint: digest64}, pinned("1.0.0"))
		}, `graph.json node "5"`, "uses pins test/hires at 1.0.0, not 1.1.0"},
		{"instance with another digest", func(p *Package) {
			addInstance(p, "bp:test/hires@1.0.0", &graph.Meta{Blueprint: "sha256:" + strings.Repeat("1", 64)}, pinned("1.0.0"))
		}, `graph.json node "5"`, "_meta.blueprint is"},
		{"instance without _meta", func(p *Package) {
			addInstance(p, "bp:test/hires@1.0.0", nil, pinned("1.0.0"))
		}, `graph.json node "5"`, "_meta.blueprint is empty"},
		{"instance with an empty digest", func(p *Package) {
			addInstance(p, "bp:test/hires@1.0.0", &graph.Meta{Title: "Hires"}, pinned("1.0.0"))
		}, `graph.json node "5"`, "_meta.blueprint is empty"},
		{"malformed blueprint class", func(p *Package) {
			addInstance(p, "bp:nope", &graph.Meta{Blueprint: digest64}, pinned("1.0.0"))
		}, `graph.json node "5"`, `"bp:nope" is not a well-formed blueprint class`},
	})
}

func TestProgramFormProblems(t *testing.T) {
	cases := []formCase{
		{"fixture is clean", func(p *Package) {}, "", ""},
		{"both forms", func(p *Package) { p.Files["graph.json"] = []byte("{}") }, "package", "exactly one of main.star and graph.json"},
		{"neither form", func(p *Package) { delete(p.Files, "main.star") }, "package", "exactly one of main.star and graph.json"},
		{"repeat", func(p *Package) { p.Manifest.Repeats = []Repeat{{Nodes: []string{"1"}}} }, "repeats", "only a node group can repeat"},
		{"input target", func(p *Package) { p.Manifest.Inputs[0].To = []Target{{NodeID: "1", Input: "samples"}} },
			`inputs "latent"`, "only a node group has to targets"},
		{"output source", func(p *Package) { p.Manifest.Outputs[0].From = &Socket{NodeID: "1"} },
			`outputs "latent"`, "only a node group has from"},
		{"setting target", func(p *Package) { p.Manifest.Settings[0].To = []Target{{NodeID: "1", Input: "image"}} },
			`settings "images"`, "only a node group has to targets"},
		{"requires are not read", func(p *Package) { p.Manifest.Requires.Classes["KSampler"] = hash24 }, "", ""},
	}
	for _, r := range []string{"a", "a.b", "left-arm.node_2.Z9", "9"} {
		cases = append(cases, formCase{"result " + r, func(p *Package) { p.Manifest.Result = r }, "", ""})
	}
	for _, r := range []string{"a b", "a..b", ".a", "a.", "a/b", "a\n", "a.b\n", "é", "a.-.b "} {
		cases = append(cases, formCase{fmt.Sprintf("result %q", r), func(p *Package) { p.Manifest.Result = r },
			"result", "result must be a dot-separated path"})
	}
	runFormCases(t, loadProgram, cases)
}

func TestCheckReportsEveryProblemInOrder(t *testing.T) {
	p := loadRefGroup(t)
	p.Manifest.Inputs[1].To = nil
	p.Manifest.Outputs[0].From = nil
	p.Manifest.Repeats = nil
	p.Manifest.Settings[1].To[0].Input = "nope"
	p.Manifest.Result = "9"
	delete(p.Manifest.Requires.Classes, "VAEEncode")
	delete(p.Manifest.Requires.Classes, "LoadImage")
	p.Group["10"] = &graph.Node{Class: "Mystery", Inputs: map[string]any{}}

	var got []string
	for _, pr := range p.Check(nil) {
		got = append(got, pr.Where)
	}
	want := []string{
		`inputs "vae"`, `outputs "conditioning"`, `settings "images"`, `settings "megapixels"`, "result",
		`graph.json node "1"`, `graph.json node "3"`, `graph.json node "10"`,
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("Where of each problem:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	first := p.Check(nil).Error()
	for i := 0; i < 20; i++ {
		if again := p.Check(nil).Error(); again != first {
			t.Fatalf("run %d differs:\n%s\n---\n%s", i, again, first)
		}
	}
}

func TestCheckTreatsANullNodeAsMissing(t *testing.T) {
	p := loadRefGroup(t)
	p.Group["2"] = nil
	var got []string
	for _, pr := range p.Check(nil) {
		got = append(got, pr.String())
	}
	want := []string{
		`settings "megapixels": node "2" is not in graph.json`,
		`settings "upscale_method": node "2" is not in graph.json`,
		`graph.json node "2": node is null`,
		`repeats[0]: node "2" is not in graph.json`,
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("problems:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestCheckAcceptsClasses(t *testing.T) {
	if ps := loadHires(t).Check(noClasses{}); len(ps) != 0 {
		t.Fatal(ps)
	}
}

type noClasses struct{}

func (noClasses) Class(string) (ClassInfo, bool) { return ClassInfo{}, false }
