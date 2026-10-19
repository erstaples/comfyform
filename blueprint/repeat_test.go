package blueprint

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/erstaples/comfyform/graph"
)

const refinePassesDir = "testdata/packages/refine-passes"

func loadRefinePasses(t *testing.T) *Package { return mustLoad(t, os.DirFS(refinePassesDir)) }

// replaceIn replaces the first old in one file of fsys with with, failing
// when the file has no old.
func replaceIn(t *testing.T, fsys fstest.MapFS, file, old, with string) {
	t.Helper()
	data := fsys[file].Data
	if !bytes.Contains(data, []byte(old)) {
		t.Fatalf("%s has no %q", file, old)
	}
	fsys[file] = &fstest.MapFile{Data: bytes.Replace(data, []byte(old), []byte(with), 1)}
}

// loadUnchecked reads a package the way Load does but runs no check, so a
// test can call Check itself on a package Load would refuse.
func loadUnchecked(t *testing.T, fsys fstest.MapFS) *Package {
	t.Helper()
	files, err := readFiles(fsys)
	if err != nil {
		t.Fatal(err)
	}
	m, err := ParseManifest(files[manifestFile])
	if err != nil {
		t.Fatal(err)
	}
	g, err := parseGroup(files[groupFile])
	if err != nil {
		t.Fatal(err)
	}
	return &Package{Manifest: m, Form: FormGroup, Group: g, Files: files, Digest: Digest(files)}
}

func imageList(images []string) []any {
	l := make([]any, len(images))
	for i, im := range images {
		l[i] = im
	}
	return l
}

// refInstance is an instance of ref-group with every setting given.
func refInstance(ref *Package, conditioning []any, images []any) *graph.Node {
	return &graph.Node{
		Class: "bp:test/ref-group@1.0.0",
		Inputs: map[string]any{
			"conditioning": conditioning, "vae": lk("3", 0),
			"images": images, "megapixels": 1.0, "upscale_method": "lanczos",
		},
		Meta: &graph.Meta{Title: "Reference images", Blueprint: ref.Digest},
	}
}

// refGraph is a text to image graph whose conditioning passes through a
// ref-group instance "14" holding the given images.
func refGraph(t *testing.T, ref *Package, images ...string) graph.Prompt {
	t.Helper()
	return graph.Prompt{
		"3":  {Class: "VAELoader", Inputs: map[string]any{"vae_name": "ae.safetensors"}},
		"4":  {Class: "CheckpointLoaderSimple", Inputs: map[string]any{"ckpt_name": "model.safetensors"}},
		"6":  {Class: "CLIPTextEncode", Inputs: map[string]any{"clip": lk("4", 1), "text": "a harbor at dusk"}},
		"14": refInstance(ref, lk("6", 0), imageList(images)),
		"15": {Class: "FluxGuidance", Inputs: map[string]any{"conditioning": lk("14", 0), "guidance": 2.5}},
		"16": {Class: "EmptyLatentImage", Inputs: map[string]any{
			"width": 1024.0, "height": 1024.0, "batch_size": 1.0,
		}},
		"17": {Class: "KSampler", Inputs: map[string]any{
			"latent_image": lk("16", 0), "model": lk("4", 0), "positive": lk("15", 0), "negative": lk("15", 0),
			"seed": 1.0, "steps": 20.0, "cfg": 3.5, "sampler_name": "euler", "scheduler": "simple", "denoise": 1.0,
		}},
		"18": {Class: "VAEDecode", Inputs: map[string]any{"samples": lk("17", 0), "vae": lk("3", 0)}},
		"19": {Class: "SaveImage", Inputs: map[string]any{"images": lk("18", 0), "filename_prefix": "ref"}},
	}
}

// refineGraph is hiresGraph with a refine-passes instance "21" making the
// given number of passes between the first sampler and the decode.
func refineGraph(t *testing.T, rp *Package, passes any) graph.Prompt {
	t.Helper()
	g := hiresGraph(t, loadHires(t))
	g["21"] = &graph.Node{
		Class: "bp:test/refine-passes@1.0.0",
		Inputs: map[string]any{
			"model": lk("4", 0), "positive": lk("6", 0), "negative": lk("6", 0), "samples": lk("17", 0),
			"passes": passes, "scale_by": 1.5, "denoise": 0.45,
		},
		Meta: &graph.Meta{Title: "Refine passes", Blueprint: rp.Digest},
	}
	return g
}

type wantLink struct {
	node, input string
	want        ref
}

func checkLinks(t *testing.T, r *Result, links []wantLink) {
	t.Helper()
	for _, l := range links {
		if got := link(t, r, l.node, l.input); got != l.want {
			t.Errorf("%s.%s links %v, want %v", l.node, l.input, got, l.want)
		}
	}
}

func checkIDs(t *testing.T, r *Result, want []string) {
	t.Helper()
	want = append([]string(nil), want...)
	sort.Slice(want, func(i, j int) bool { return graph.NaturalLess(want[i], want[j]) })
	if got := r.Prompt.IDs(); !reflect.DeepEqual(got, want) {
		t.Fatalf("nodes %v, want %v", got, want)
	}
}

// copyIDs are the ids of copies 0 to n-1 of the given nodes under prefix.
func copyIDs(prefix string, n int, nodes ...string) []string {
	var ids []string
	for k := 0; k < n; k++ {
		for _, id := range nodes {
			ids = append(ids, fmt.Sprintf("%s.r%d.%s", prefix, k, id))
		}
	}
	return ids
}

var refOuterIDs = []string{"3", "4", "6", "15", "16", "17", "18", "19"}

func TestRepeatThreeImages(t *testing.T) {
	rg := loadRefGroup(t)
	groupBefore, _ := json.Marshal(rg.Group)
	r := mustCompile(t, refGraph(t, rg, "a.png", "b.png", "c.png"), rg)
	again := mustCompile(t, refGraph(t, rg, "a.png", "b.png", "c.png"), rg)
	a, _ := json.Marshal(r)
	b, _ := json.Marshal(again)
	if !bytes.Equal(a, b) {
		t.Fatalf("two compiles differ:\n%s\n%s", a, b)
	}
	if groupAfter, _ := json.Marshal(rg.Group); !bytes.Equal(groupBefore, groupAfter) {
		t.Fatalf("graph.json changed:\n%s\n%s", groupBefore, groupAfter)
	}
	checkIDs(t, r, append(copyIDs("14", 3, "1", "2", "3", "4"), refOuterIDs...))
	for k, im := range []string{"a.png", "b.png", "c.png"} {
		if got := value(t, r, fmt.Sprintf("14.r%d.1", k), "image"); got != im {
			t.Errorf("14.r%d.1 image = %v, want %s", k, got, im)
		}
	}
	checkLinks(t, r, []wantLink{
		{"14.r0.4", "conditioning", ref{"6", 0}},
		{"14.r1.4", "conditioning", ref{"14.r0.4", 0}},
		{"14.r2.4", "conditioning", ref{"14.r1.4", 0}},
		{"14.r0.3", "vae", ref{"3", 0}},
		{"14.r2.3", "vae", ref{"3", 0}},          // a shared input reaches every copy
		{"14.r1.4", "latent", ref{"14.r1.3", 0}}, // links inside a copy stay in that copy
		{"14.r1.3", "pixels", ref{"14.r1.2", 0}},
		{"14.r2.2", "image", ref{"14.r2.1", 0}},
		{"15", "conditioning", ref{"14.r2.4", 0}},
	})
	for _, k := range []string{"14.r0.2", "14.r2.2"} {
		if got := value(t, r, k, "megapixels"); got != 1.0 {
			t.Errorf("%s megapixels = %v, want every copy to get 1", k, got)
		}
		if got := value(t, r, k, "upscale_method"); got != "lanczos" {
			t.Errorf("%s upscale_method = %v", k, got)
		}
	}
	if len(r.Sources) != 12 {
		t.Fatalf("%d sources, want 12", len(r.Sources))
	}
	want := Source{Instances: []string{"14"}, Blueprint: "test/ref-group@1.0.0", Item: &Item{Setting: "images", Index: 1}}
	if got := r.Sources["14.r1.3"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("source of 14.r1.3 = %+v, want %+v", got, want)
	}
	if got := r.Sources["14.r2.1"].Item; !reflect.DeepEqual(got, &Item{Setting: "images", Index: 2}) {
		t.Fatalf("item of 14.r2.1 = %+v", got)
	}
}

func TestRepeatNoImages(t *testing.T) {
	rg := loadRefGroup(t)
	r := mustCompile(t, refGraph(t, rg), rg)
	checkIDs(t, r, refOuterIDs)
	checkLinks(t, r, []wantLink{{"15", "conditioning", ref{"6", 0}}})
	if len(r.Sources) != 0 {
		t.Fatalf("Sources = %v", r.Sources)
	}
}

func TestTwoEmptyRepeatsChain(t *testing.T) {
	rg := loadRefGroup(t)
	g := refGraph(t, rg)
	g["20"] = refInstance(rg, lk("14", 0), []any{})
	g["15"].Inputs["conditioning"] = lk("20", 0)
	r := mustCompile(t, g, rg)
	checkIDs(t, r, refOuterIDs)
	checkLinks(t, r, []wantLink{{"15", "conditioning", ref{"6", 0}}})

	// The second takes what the empty first passes through.
	g = refGraph(t, rg)
	g["20"] = refInstance(rg, lk("14", 0), []any{"a.png"})
	g["15"].Inputs["conditioning"] = lk("20", 0)
	r = mustCompile(t, g, rg)
	checkIDs(t, r, append(copyIDs("20", 1, "1", "2", "3", "4"), refOuterIDs...))
	checkLinks(t, r, []wantLink{
		{"20.r0.4", "conditioning", ref{"6", 0}},
		{"15", "conditioning", ref{"20.r0.4", 0}},
	})
}

func TestRepeatCount(t *testing.T) {
	rp := loadRefinePasses(t)
	outer := []string{"4", "6", "16", "17", "18", "19"}
	r := mustCompile(t, refineGraph(t, rp, 2.0), rp)
	checkIDs(t, r, append(copyIDs("21", 2, "1", "2"), outer...))
	checkLinks(t, r, []wantLink{
		{"21.r0.1", "samples", ref{"17", 0}},
		{"21.r1.1", "samples", ref{"21.r0.2", 0}},
		{"21.r0.2", "latent_image", ref{"21.r0.1", 0}},
		{"21.r1.2", "latent_image", ref{"21.r1.1", 0}},
		{"21.r0.2", "model", ref{"4", 0}},
		{"21.r1.2", "model", ref{"4", 0}},
		{"21.r1.2", "positive", ref{"6", 0}},
		{"18", "samples", ref{"21.r1.2", 0}},
	})
	if got := value(t, r, "21.r1.2", "denoise"); got != 0.45 {
		t.Errorf("21.r1.2 denoise = %v", got)
	}
	if got := r.Sources["21.r1.2"].Item; !reflect.DeepEqual(got, &Item{Setting: "passes", Index: 1}) {
		t.Fatalf("item of 21.r1.2 = %+v", got)
	}

	r = mustCompile(t, refineGraph(t, rp, 0.0), rp)
	checkIDs(t, r, outer)
	checkLinks(t, r, []wantLink{{"18", "samples", ref{"17", 0}}})

	// A count the instance leaves out takes the setting's default.
	g := refineGraph(t, rp, 0.0)
	delete(g["21"].Inputs, "passes")
	checkIDs(t, mustCompile(t, g, rp), append(copyIDs("21", 2, "1", "2"), outer...))
}

func TestRepeatCountValues(t *testing.T) {
	rp := loadRefinePasses(t)
	for _, c := range []struct {
		passes any
		want   string
	}{
		{5.0, "must be at most 4"},
		{1.5, "must be a whole number"},
		{-1.0, "must be at least 0"},
		{"2", "must be a number"},
	} {
		t.Run(fmt.Sprint(c.passes), func(t *testing.T) {
			e := compileError(t, refineGraph(t, rp, c.passes), rp)
			if e.Setting != "passes" {
				t.Fatalf("Setting = %q (%s)", e.Setting, e)
			}
			wantMsg(t, e, c.want)
		})
	}

	// A linkable count fed by a link has no value to count with.
	fsys := dirFiles(t, refinePassesDir)
	replaceIn(t, fsys, manifestFile, `"name": "passes", "kind": "int",`, `"name": "passes", "kind": "int", "linkable": true,`)
	linkable := mustLoad(t, fsys)
	g := refineGraph(t, linkable, lk("30", 0))
	g["30"] = &graph.Node{Class: "PrimitiveInt", Inputs: map[string]any{"value": 2.0}}
	e := compileError(t, g, linkable)
	if e.Setting != "passes" {
		t.Fatalf("Setting = %q (%s)", e.Setting, e)
	}
	wantMsg(t, e, "setting passes is linked; this blueprint needs a value")
}

// zipPackage is ref-group with megapixels a second list in the repeat's over.
func zipPackage(t *testing.T) *Package {
	t.Helper()
	fsys := dirFiles(t, refGroupDir)
	replaceIn(t, fsys, manifestFile, `"default": 1.0, "min": 0.25`, `"list": { "min": 0, "max": 8 }, "min": 0.25`)
	replaceIn(t, fsys, manifestFile, `"over": ["images"]`, `"over": ["images", "megapixels"]`)
	return mustLoad(t, fsys)
}

func TestRepeatZip(t *testing.T) {
	zip := zipPackage(t)
	g := refGraph(t, zip, "a.png", "b.png", "c.png")
	g["14"].Inputs["megapixels"] = []any{0.5, 1.0, 2.0}
	r := mustCompile(t, g, zip)
	for k, want := range []any{0.5, 1.0, 2.0} {
		if got := value(t, r, fmt.Sprintf("14.r%d.2", k), "megapixels"); got != want {
			t.Errorf("14.r%d.2 megapixels = %v, want %v", k, got, want)
		}
	}
	if got := value(t, r, "14.r2.1", "image"); got != "c.png" {
		t.Errorf("14.r2.1 image = %v", got)
	}
	// The item is named by the first over setting.
	if got := r.Sources["14.r2.2"].Item; !reflect.DeepEqual(got, &Item{Setting: "images", Index: 2}) {
		t.Fatalf("item of 14.r2.2 = %+v", got)
	}

	g["14"].Inputs["megapixels"] = []any{0.5, 1.0}
	e := compileError(t, g, zip)
	if e.Msg != "images has 3 items and megapixels has 2" {
		t.Fatalf("Msg = %q", e.Msg)
	}
	if e.Setting != "megapixels" {
		t.Fatalf("Setting = %q", e.Setting)
	}
}

func TestRepeatListTooLong(t *testing.T) {
	rg := loadRefGroup(t)
	images := make([]string, 9)
	for i := range images {
		images[i] = fmt.Sprintf("%d.png", i)
	}
	e := compileError(t, refGraph(t, rg, images...), rg)
	if e.Setting != "images" {
		t.Fatalf("Setting = %q (%s)", e.Setting, e)
	}
	wantMsg(t, e, "at most 8")
}

func TestRepeatImagesNotAList(t *testing.T) {
	rg := loadRefGroup(t)
	g := refGraph(t, rg)
	g["14"].Inputs["images"] = "a.png"
	e := compileError(t, g, rg)
	if e.Setting != "images" {
		t.Fatalf("Setting = %q (%s)", e.Setting, e)
	}
	wantMsg(t, e, "must be a list")
}

// optionalRef is ref-group with its conditioning port optional.
func optionalRef(t *testing.T) *Package {
	t.Helper()
	fsys := dirFiles(t, refGroupDir)
	replaceIn(t, fsys, manifestFile, `{ "name": "conditioning", "type": "CONDITIONING", "to"`,
		`{ "name": "conditioning", "type": "CONDITIONING", "optional": true, "to"`)
	return mustLoad(t, fsys)
}

func TestRepeatPassesThroughToNothing(t *testing.T) {
	opt := optionalRef(t)

	// The first copy's carried input drops with the port behind it.
	g := refGraph(t, opt, "a.png", "b.png")
	delete(g["14"].Inputs, "conditioning")
	r := mustCompile(t, g, opt)
	checkIDs(t, r, append(copyIDs("14", 2, "1", "2", "3", "4"), refOuterIDs...))
	if v, ok := r.Prompt["14.r0.4"].Inputs["conditioning"]; ok {
		t.Fatalf("14.r0.4 conditioning = %v, want it left out", v)
	}
	checkLinks(t, r, []wantLink{{"14.r1.4", "conditioning", ref{"14.r0.4", 0}}})

	// With no copies the output passes that nothing through.
	g = refGraph(t, opt)
	delete(g["14"].Inputs, "conditioning")
	e := compileError(t, g, opt)
	wantMsg(t, e, "node 15 input conditioning links output 0, which has nothing behind it")
}

// twoRefs is a node group of two reference chains, the second carrying on
// from the first: nodes 1 to 4 repeat over faces, 5 to 8 over styles, and
// node 8's first copy takes the first chain's conditioning.
func twoRefs(t *testing.T, optional bool) *Package {
	t.Helper()
	classes, err := json.Marshal(loadRefGroup(t).Manifest.Requires.Classes)
	if err != nil {
		t.Fatal(err)
	}
	manifest := fmt.Sprintf(`{
  "format": 1, "id": "test/two-refs", "version": "1.0.0", "title": "Two reference chains",
  "inputs": [
    { "name": "conditioning", "type": "CONDITIONING", "optional": %t, "to": [{ "nodeId": "4", "input": "conditioning" }] },
    { "name": "vae", "type": "VAE", "to": [{ "nodeId": "3", "input": "vae" }, { "nodeId": "7", "input": "vae" }] }
  ],
  "outputs": [{ "name": "conditioning", "type": "CONDITIONING", "from": { "nodeId": "8", "output": 0 } }],
  "settings": [
    { "name": "faces", "kind": "image", "list": { "min": 0, "max": 4 }, "to": [{ "nodeId": "1", "input": "image" }] },
    { "name": "styles", "kind": "image", "list": { "min": 0, "max": 4 }, "to": [{ "nodeId": "5", "input": "image" }] }
  ],
  "repeats": [
    { "nodes": ["1", "2", "3", "4"], "over": ["faces"],
      "carry": [{ "to": { "nodeId": "4", "input": "conditioning" }, "from": { "nodeId": "4", "output": 0 } }] },
    { "nodes": ["5", "6", "7", "8"], "over": ["styles"],
      "carry": [{ "to": { "nodeId": "8", "input": "conditioning" }, "from": { "nodeId": "8", "output": 0 } }] }
  ],
  "requires": { "classes": %s }
}`, optional, classes)
	group := `{
  "1": { "class_type": "LoadImage", "inputs": { "image": "" } },
  "2": { "class_type": "ImageScaleToTotalPixels",
         "inputs": { "image": ["1", 0], "upscale_method": "lanczos", "megapixels": 1.0, "resolution_steps": 1 } },
  "3": { "class_type": "VAEEncode", "inputs": { "pixels": ["2", 0], "vae": ["in", 0] } },
  "4": { "class_type": "ReferenceLatent", "inputs": { "conditioning": ["in", 0], "latent": ["3", 0] } },
  "5": { "class_type": "LoadImage", "inputs": { "image": "" } },
  "6": { "class_type": "ImageScaleToTotalPixels",
         "inputs": { "image": ["5", 0], "upscale_method": "lanczos", "megapixels": 1.0, "resolution_steps": 1 } },
  "7": { "class_type": "VAEEncode", "inputs": { "pixels": ["6", 0], "vae": ["in", 0] } },
  "8": { "class_type": "ReferenceLatent", "inputs": { "conditioning": ["4", 0], "latent": ["7", 0] } }
}`
	return mustLoad(t, fstest.MapFS{
		manifestFile: {Data: []byte(manifest)},
		groupFile:    {Data: []byte(group)},
	})
}

func twoRefsGraph(t *testing.T, p *Package, faces, styles []string) graph.Prompt {
	t.Helper()
	g := refGraph(t, p)
	g["14"] = &graph.Node{
		Class: "bp:test/two-refs@1.0.0",
		Inputs: map[string]any{
			"conditioning": lk("6", 0), "vae": lk("3", 0), "faces": imageList(faces), "styles": imageList(styles),
		},
		Meta: &graph.Meta{Blueprint: p.Digest},
	}
	return g
}

func TestRepeatPassesThroughAnotherRepeat(t *testing.T) {
	p := twoRefs(t, false)
	if ps := p.Check(fixtureClasses(t)); len(ps) != 0 {
		t.Fatalf("two-refs has problems:\n%v", ps)
	}
	cases := []struct {
		name          string
		faces, styles []string
		ids           []string
		links         []wantLink
	}{
		{"both empty", nil, nil, nil, []wantLink{{"15", "conditioning", ref{"6", 0}}}},
		{"second empty", []string{"a.png", "b.png"}, nil, copyIDs("14", 2, "1", "2", "3", "4"),
			[]wantLink{{"15", "conditioning", ref{"14.r1.4", 0}}}},
		{"first empty", nil, []string{"s.png"}, copyIDs("14", 1, "5", "6", "7", "8"), []wantLink{
			{"14.r0.8", "conditioning", ref{"6", 0}},
			{"15", "conditioning", ref{"14.r0.8", 0}},
		}},
		{"neither empty", []string{"a.png"}, []string{"s.png", "t.png"},
			append(copyIDs("14", 1, "1", "2", "3", "4"), copyIDs("14", 2, "5", "6", "7", "8")...), []wantLink{
				{"14.r0.8", "conditioning", ref{"14.r0.4", 0}},
				{"14.r1.8", "conditioning", ref{"14.r0.8", 0}},
				{"14.r1.7", "vae", ref{"3", 0}},
				{"15", "conditioning", ref{"14.r1.8", 0}},
			}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := mustCompile(t, twoRefsGraph(t, p, c.faces, c.styles), p)
			checkIDs(t, r, append(c.ids, refOuterIDs...))
			checkLinks(t, r, c.links)
			if c.styles != nil {
				if got := r.Sources["14.r0.8"].Item; !reflect.DeepEqual(got, &Item{Setting: "styles", Index: 0}) {
					t.Fatalf("item of 14.r0.8 = %+v", got)
				}
			}
		})
	}

	// A pass-through to an optional port with nothing behind it leaves the
	// second chain's first copy with nothing.
	opt := twoRefs(t, true)
	g := twoRefsGraph(t, opt, nil, []string{"s.png"})
	delete(g["14"].Inputs, "conditioning")
	e := compileError(t, g, opt)
	wantMsg(t, e, "node 8 input conditioning links output 0 of node 4, which has nothing behind it")
	if !reflect.DeepEqual(e.Item, &Item{Setting: "styles", Index: 0}) {
		t.Fatalf("Item = %+v", e.Item)
	}
}

// TestRepeatCarriesLoop compiles a group whose two repeats each carry from
// the other: copies of both compile, and with no copies of either the
// pass-through has nowhere to end.
func TestRepeatCarriesLoop(t *testing.T) {
	hires := loadHires(t).Manifest.Requires.Classes
	manifest := fmt.Sprintf(`{
  "format": 1, "id": "test/loop", "version": "1.0.0", "title": "Loop",
  "inputs": [{ "name": "samples", "type": "LATENT", "to": [{ "nodeId": "2", "input": "samples" }, { "nodeId": "4", "input": "samples" }] }],
  "outputs": [{ "name": "latent", "type": "LATENT", "from": { "nodeId": "2", "output": 0 } }],
  "settings": [
    { "name": "a", "kind": "int", "default": 0, "min": 0, "max": 4 },
    { "name": "b", "kind": "int", "default": 0, "min": 0, "max": 4 }
  ],
  "repeats": [
    { "nodes": ["1", "2"], "count": "a",
      "carry": [{ "to": { "nodeId": "1", "input": "samples" }, "from": { "nodeId": "2", "output": 0 } }] },
    { "nodes": ["3", "4"], "count": "b",
      "carry": [{ "to": { "nodeId": "3", "input": "samples" }, "from": { "nodeId": "4", "output": 0 } }] }
  ],
  "requires": { "classes": { "LatentUpscaleBy": %q } }
}`, hires["LatentUpscaleBy"])
	node := func(samples string) string {
		return fmt.Sprintf(`{ "class_type": "LatentUpscaleBy", "inputs": { "samples": %s, "upscale_method": "nearest-exact", "scale_by": 1.5 } }`, samples)
	}
	group := fmt.Sprintf(`{ "1": %s, "2": %s, "3": %s, "4": %s }`,
		node(`["4", 0]`), node(`["in", 0]`), node(`["2", 0]`), node(`["in", 0]`))
	p := mustLoad(t, fstest.MapFS{manifestFile: {Data: []byte(manifest)}, groupFile: {Data: []byte(group)}})
	if ps := p.Check(fixtureClasses(t)); len(ps) != 0 {
		t.Fatalf("loop has problems:\n%v", ps)
	}
	graphWith := func(a, b float64) graph.Prompt {
		g := hiresGraph(t, loadHires(t))
		g["21"] = &graph.Node{
			Class:  "bp:test/loop@1.0.0",
			Inputs: map[string]any{"samples": lk("17", 0), "a": a, "b": b},
			Meta:   &graph.Meta{Blueprint: p.Digest},
		}
		return g
	}

	r := mustCompile(t, graphWith(1, 1), p)
	checkLinks(t, r, []wantLink{
		{"21.r0.1", "samples", ref{"21.r0.4", 0}},
		{"21.r0.3", "samples", ref{"21.r0.2", 0}},
		{"18", "samples", ref{"21.r0.2", 0}},
	})
	r = mustCompile(t, graphWith(0, 1), p)
	checkLinks(t, r, []wantLink{
		{"21.r0.3", "samples", ref{"21.r0.4", 0}},
		{"18", "samples", ref{"21.r0.4", 0}},
	})
	wantMsg(t, compileError(t, graphWith(0, 0), p), "pass through each other in a loop")
}

func TestRepeatFixturesAreClean(t *testing.T) {
	for _, p := range []*Package{loadRefGroup(t), loadRefinePasses(t), zipPackage(t)} {
		if ps := p.Check(fixtureClasses(t)); len(ps) != 0 {
			t.Errorf("%s:\n%v", p.Manifest.ID, ps)
		}
	}
}

// refTail is the end of ref-group's graph.json, where a test adds a node.
const refTail = `"latent": ["3", 0] } }`

func TestRepeatRules(t *testing.T) {
	type edit struct{ file, old, with string }
	cases := []struct {
		name  string
		dir   string
		edits []edit
		where string
		want  string
	}{
		// The rules the spec names.
		{"uncarried output leaves", refGroupDir, []edit{
			{groupFile, refTail, refTail + `,
  "5": { "class_type": "ReferenceLatent", "inputs": { "conditioning": ["4", 0], "latent": ["3", 0] } }`},
		}, "repeats[0]", "only carried outputs leave a repeat: 5.latent links output 0 of node 3"},
		{"carry types differ", refGroupDir, []edit{
			{manifestFile, `"from": { "nodeId": "4", "output": 0 }`, `"from": { "nodeId": "3", "output": 0 }`},
			{manifestFile, `"from": { "nodeId": "4", "output": 0 }`, `"from": { "nodeId": "3", "output": 0 }`},
		}, "repeats[0]", "carry types differ: LATENT and CONDITIONING"},
		{"carry input not linked", refGroupDir, []edit{
			{manifestFile, `"to": [{ "nodeId": "4", "input": "conditioning" }]`, `"to": [{ "nodeId": "4", "input": "latent" }]`},
		}, "repeats[0]", "carry input 4.conditioning is not linked"},
		{"path comes back", refGroupDir, []edit{
			{groupFile, `"image": ["1", 0]`, `"image": ["5", 0]`},
			{groupFile, refTail, refTail + `,
  "5": { "class_type": "ReferenceLatent", "inputs": { "conditioning": ["4", 0] } }`},
		}, "repeats[0]", "leaves the repeat and comes back"},
		{"shared node", refGroupDir, []edit{
			{manifestFile, `"settings": [`, `"settings": [
    { "name": "times", "kind": "int", "default": 1, "min": 0, "max": 4 },`},
			{manifestFile, "  }],\n  \"requires\"", "  }, { \"nodes\": [\"2\"], \"count\": \"times\", \"carry\": [] }],\n  \"requires\""},
		}, "repeats[1]", "repeats share node 2"},
		{"over and count", refGroupDir, []edit{
			{manifestFile, `"over": ["images"],`, `"over": ["images"], "count": "megapixels",`},
		}, "repeats[0]", "needs over or count, not both"},
		{"list setting drives nothing", refGroupDir, []edit{
			{manifestFile, `"default": 1.0, "min": 0.25`, `"list": { "min": 0, "max": 8 }, "min": 0.25`},
		}, `settings "megapixels"`, "a list setting in a node group must drive a repeat"},
		{"over target outside", refGroupDir, []edit{
			{manifestFile, `"nodes": ["1", "2", "3", "4"]`, `"nodes": ["2", "3", "4"]`},
		}, "repeats[0]", "over setting images has a target outside the repeat"},
		{"result inside", refGroupDir, []edit{
			{manifestFile, `"requires": {`, `"result": "4",
  "requires": {`},
		}, "repeats[0]", "result is inside a repeat"},

		// The rest of the brief's rules.
		{"no nodes", refinePassesDir, []edit{
			{manifestFile, `"nodes": ["1", "2"]`, `"nodes": []`},
		}, "repeats[0]", "nodes is empty"},
		{"node missing", refGroupDir, []edit{
			{manifestFile, `"nodes": ["1", "2", "3", "4"]`, `"nodes": ["1", "2", "3", "4", "9"]`},
		}, "repeats[0]", `node "9" is not in graph.json`},
		{"node twice", refGroupDir, []edit{
			{manifestFile, `"nodes": ["1", "2", "3", "4"]`, `"nodes": ["1", "2", "3", "4", "2"]`},
		}, "repeats[0]", "node 2 appears twice"},
		{"neither over nor count", refGroupDir, []edit{
			{manifestFile, `"over": ["images"],`, ``},
		}, "repeats[0]", "needs over or count"},
		{"count names no setting", refinePassesDir, []edit{
			{manifestFile, `"count": "passes"`, `"count": "laps"`},
		}, "repeats[0]", "count names laps, which is not a setting"},
		{"count is a float", refinePassesDir, []edit{
			{manifestFile, `"name": "passes", "kind": "int"`, `"name": "passes", "kind": "float"`},
		}, "repeats[0]", "count setting passes is float; a count is an int"},
		{"count is a list", refinePassesDir, []edit{
			{manifestFile, `{ "name": "passes", "kind": "int", "default": 2, "min": 0, "max": 4 }`,
				`{ "name": "passes", "kind": "int", "list": { "min": 0, "max": 4 } }`},
		}, "repeats[0]", "count setting passes is a list; a count is one int"},
		{"over names no setting", refGroupDir, []edit{
			{manifestFile, `"over": ["images"]`, `"over": ["images", "pictures"]`},
		}, "repeats[0]", "over names pictures, which is not a setting"},
		{"over names a plain setting", refGroupDir, []edit{
			{manifestFile, `"over": ["images"]`, `"over": ["images", "megapixels"]`},
		}, "repeats[0]", "over setting megapixels is not a list"},
		{"over names a setting twice", refGroupDir, []edit{
			{manifestFile, `"over": ["images"]`, `"over": ["images", "images"]`},
		}, "repeats[0]", "over names images twice"},
		{"list setting drives two repeats", refGroupDir, []edit{
			{manifestFile, "  }],\n  \"requires\"", "  }, { \"nodes\": [], \"over\": [\"images\"], \"carry\": [] }],\n  \"requires\""},
		}, "repeats[1]", "over setting images already drives repeats[0]"},
		{"carry to a node outside", refinePassesDir, []edit{
			{manifestFile, `"nodes": ["1", "2"]`, `"nodes": ["2"]`},
		}, "repeats[0]", "carry input 1.samples is on node 1, which is not in the repeat"},
		{"carry from a node outside", refinePassesDir, []edit{
			{manifestFile, `"nodes": ["1", "2"]`, `"nodes": ["1"]`},
		}, "repeats[0]", "carry from output 0 of node 2, which is not in the repeat"},
		{"carry input linked inside", refGroupDir, []edit{
			{manifestFile, `"to": { "nodeId": "4", "input": "conditioning" }`, `"to": { "nodeId": "4", "input": "latent" }`},
		}, "repeats[0]", "carry input 4.latent links node 3, inside the repeat"},
		{"carry input twice", refinePassesDir, []edit{
			{manifestFile, `"carry": [{`, `"carry": [{ "to": { "nodeId": "1", "input": "samples" }, "from": { "nodeId": "2", "output": 0 } }, {`},
		}, "repeats[0]", "carry input 1.samples is carried twice"},
		{"carry from an output the class lacks", refinePassesDir, []edit{
			{manifestFile, `"from": { "nodeId": "2", "output": 0 } }]`, `"from": { "nodeId": "2", "output": 3 } }]`},
		}, "repeats[0]", "carry from output 3 of node 2, and KSampler has 1 output"},
		{"carry to an input the class lacks", refinePassesDir, []edit{
			{manifestFile, `"to": { "nodeId": "1", "input": "samples" }`, `"to": { "nodeId": "1", "input": "extra" }`},
			{groupFile, `"scale_by": 1.5 }`, `"scale_by": 1.5, "extra": ["in", 0] }`},
			{manifestFile, `"to": [{ "nodeId": "1", "input": "samples" }]`, `"to": [{ "nodeId": "1", "input": "samples" }, { "nodeId": "1", "input": "extra" }]`},
		}, "repeats[0]", "carry input 1.extra: LatentUpscaleBy has no input extra"},
		{"output port from an uncarried output", refGroupDir, []edit{
			{manifestFile, `"from": { "nodeId": "4", "output": 0 } }`, `"from": { "nodeId": "3", "output": 0 } }`},
		}, "repeats[0]", "only carried outputs leave a repeat: output port conditioning comes from output 0 of node 3"},
	}
	// The mutations for the spec's rules break that rule and no other.
	only := map[string]bool{
		"uncarried output leaves": true, "carry types differ": true, "carry input not linked": true,
		"path comes back": true, "shared node": true, "over and count": true,
		"list setting drives nothing": true, "over target outside": true, "result inside": true,
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fsys := dirFiles(t, c.dir)
			for _, e := range c.edits {
				replaceIn(t, fsys, e.file, e.old, e.with)
			}
			ps := loadUnchecked(t, fsys).Check(fixtureClasses(t))
			if only[c.name] && len(ps) != 1 {
				t.Fatalf("want one problem, got %d:\n%v", len(ps), ps)
			}
			for _, p := range ps {
				if strings.Contains(p.Msg, c.want) {
					if p.Where != c.where {
						t.Fatalf("%q is on %s, want %s", p.Msg, p.Where, c.where)
					}
					return
				}
			}
			t.Fatalf("no problem contains %q:\n%v", c.want, ps)
		})
	}
}

// refAround is ref-group with a node on each side of its repeat: a VAELoader
// "9" feeding every copy's VAE Encode in place of the vae port, and a
// FluxGuidance "5" taking the last copy's conditioning, which is the output.
func refAround(t *testing.T) *Package {
	t.Helper()
	s := fixtureSchema(t)
	fsys := dirFiles(t, refGroupDir)
	replaceIn(t, fsys, manifestFile, `,
    { "name": "vae", "type": "VAE", "to": [{ "nodeId": "3", "input": "vae" }] }`, ``)
	replaceIn(t, fsys, manifestFile, `"type": "CONDITIONING", "from": { "nodeId": "4", "output": 0 } }`,
		`"type": "CONDITIONING", "from": { "nodeId": "5", "output": 0 } }`)
	replaceIn(t, fsys, manifestFile, `"classes": {`, fmt.Sprintf(`"classes": {
      "FluxGuidance": %q, "VAELoader": %q,`, s.Classes["FluxGuidance"].Hash(), s.Classes["VAELoader"].Hash()))
	replaceIn(t, fsys, groupFile, `"vae": ["in", 0]`, `"vae": ["9", 0]`)
	replaceIn(t, fsys, groupFile, refTail, refTail+`,
  "5": { "class_type": "FluxGuidance", "inputs": { "conditioning": ["4", 0], "guidance": 2.5 } },
  "9": { "class_type": "VAELoader", "inputs": { "vae_name": "ae.safetensors" } }`)
	return mustLoad(t, fsys)
}

func TestRepeatBetweenGroupNodes(t *testing.T) {
	p := refAround(t)
	if ps := p.Check(fixtureClasses(t)); len(ps) != 0 {
		t.Fatalf("problems:\n%v", ps)
	}
	graphWith := func(images ...string) graph.Prompt {
		g := refGraph(t, p, images...)
		delete(g["14"].Inputs, "vae")
		return g
	}

	r := mustCompile(t, graphWith("a.png", "b.png", "c.png"), p)
	checkIDs(t, r, append(copyIDs("14", 3, "1", "2", "3", "4"), append(refOuterIDs, "14.5", "14.9")...))
	checkLinks(t, r, []wantLink{
		{"14.r0.3", "vae", ref{"14.9", 0}}, // a node before the repeat feeds every copy
		{"14.r2.3", "vae", ref{"14.9", 0}},
		{"14.5", "conditioning", ref{"14.r2.4", 0}}, // a node after it takes the last copy
		{"15", "conditioning", ref{"14.5", 0}},
	})
	if got := r.Sources["14.5"]; got.Item != nil {
		t.Fatalf("14.5 is outside the repeat and has item %+v", got.Item)
	}

	r = mustCompile(t, graphWith(), p)
	checkIDs(t, r, append(refOuterIDs, "14.5", "14.9"))
	checkLinks(t, r, []wantLink{
		{"14.5", "conditioning", ref{"6", 0}},
		{"15", "conditioning", ref{"14.5", 0}},
	})
}

func TestRepeatCopiesCountAgainstTheNodeLimit(t *testing.T) {
	rg := loadRefGroup(t)
	opt := options(t, rg)
	opt.MaxNodes = 10
	if _, err := Compile(context.Background(), refGraph(t, rg, "a.png", "b.png"), opt); err != nil {
		t.Fatalf("8 nodes: %v", err)
	}
	_, err := Compile(context.Background(), refGraph(t, rg, "a.png", "b.png", "c.png"), opt)
	if err == nil || !strings.Contains(err.Error(), "compile makes more than 10 nodes") {
		t.Fatalf("12 nodes: err = %v", err)
	}

	// A count with no max stops at the limit before it makes a node.
	fsys := dirFiles(t, refinePassesDir)
	replaceIn(t, fsys, manifestFile, `"default": 2, "min": 0, "max": 4 }`, `"default": 2, "min": 0 }`)
	unbounded := mustLoad(t, fsys)
	e := compileError(t, refineGraph(t, unbounded, 1e12), unbounded)
	if e.Setting != "passes" {
		t.Fatalf("Setting = %q (%s)", e.Setting, e)
	}
	wantMsg(t, e, "compile makes more than 2000 nodes")
}
