package blueprint

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/erstaples/comfyform/graph"
)

// resolver serves packages by digest.
type resolver map[string]*Package

func resolverOf(pkgs ...*Package) resolver {
	r := resolver{}
	for _, p := range pkgs {
		r[p.Digest] = p
	}
	return r
}

func (r resolver) Package(digest string) (*Package, error) {
	if p, ok := r[digest]; ok {
		return p, nil
	}
	return nil, fmt.Errorf("no package %s", digest)
}

// lk is a link as JSON decodes it.
func lk(node string, idx int) []any { return []any{node, float64(idx)} }

// hiresInstance is an instance of hires with every port and setting given.
func hiresInstance(hires *Package, samples []any) *graph.Node {
	return &graph.Node{
		Class: "bp:test/hires@1.0.0",
		Inputs: map[string]any{
			"model": lk("4", 0), "positive": lk("6", 0), "negative": lk("6", 0), "samples": samples,
			"scale_by": 1.5, "denoise": 0.45, "steps": 14.0, "seed": 7.0,
		},
		Meta: &graph.Meta{Title: "Hires pass", Blueprint: hires.Digest},
	}
}

// hiresGraph is a text to image graph with a hires instance "21" between the
// first sampler and the decode.
func hiresGraph(t *testing.T, hires *Package) graph.Prompt {
	t.Helper()
	return graph.Prompt{
		"4": {Class: "CheckpointLoaderSimple", Inputs: map[string]any{"ckpt_name": "model.safetensors"}},
		"6": {Class: "CLIPTextEncode", Inputs: map[string]any{"clip": lk("4", 1), "text": "a harbor at dusk"}},
		"16": {Class: "EmptyLatentImage", Inputs: map[string]any{
			"width": 1024.0, "height": 1024.0, "batch_size": 1.0,
		}},
		"17": {Class: "KSampler", Inputs: map[string]any{
			"latent_image": lk("16", 0), "model": lk("4", 0), "positive": lk("6", 0), "negative": lk("6", 0),
			"seed": 1.0, "steps": 20.0, "cfg": 3.5, "sampler_name": "euler", "scheduler": "simple", "denoise": 1.0,
		}},
		"21": hiresInstance(hires, lk("17", 0)),
		"18": {Class: "VAEDecode", Inputs: map[string]any{"samples": lk("21", 0), "vae": lk("4", 2)}},
		"19": {Class: "SaveImage", Inputs: map[string]any{"images": lk("18", 0), "filename_prefix": "hires"}},
	}
}

// hiresVariant loads the hires fixture with old replaced by with in one file.
func hiresVariant(t *testing.T, file, old, with string) *Package {
	t.Helper()
	fsys := dirFiles(t, hiresDir)
	data := fsys[file].Data
	if !bytes.Contains(data, []byte(old)) {
		t.Fatalf("%s has no %q", file, old)
	}
	fsys[file] = &fstest.MapFile{Data: bytes.Replace(data, []byte(old), []byte(with), 1)}
	return mustLoad(t, fsys)
}

func options(t *testing.T, pkgs ...*Package) Options {
	return Options{Classes: fixtureClasses(t), Packages: resolverOf(pkgs...)}
}

func mustCompile(t *testing.T, g graph.Prompt, pkgs ...*Package) *Result {
	t.Helper()
	r, err := Compile(g, options(t, pkgs...))
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	return r
}

func compileError(t *testing.T, g graph.Prompt, pkgs ...*Package) *Error {
	t.Helper()
	r, err := Compile(g, options(t, pkgs...))
	if err == nil {
		t.Fatalf("Compile succeeded with nodes %v", r.Prompt.IDs())
	}
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("error %T is not an *Error: %v", err, err)
	}
	return e
}

func value(t *testing.T, r *Result, node, input string) any {
	t.Helper()
	n := r.Prompt[node]
	if n == nil {
		t.Fatalf("no node %s; nodes are %v", node, r.Prompt.IDs())
	}
	v, ok := n.Inputs[input]
	if !ok {
		t.Fatalf("node %s has no input %s", node, input)
	}
	return v
}

// link reads a link from the compiled graph, which must be in its normalized
// form, []any{id, float64}.
func link(t *testing.T, r *Result, node, input string) ref {
	t.Helper()
	v := value(t, r, node, input)
	l, ok := v.([]any)
	if !ok || len(l) != 2 {
		t.Fatalf("%s.%s = %#v, not a normalized link", node, input, v)
	}
	id, idOK := l[0].(string)
	idx, idxOK := l[1].(float64)
	if !idOK || !idxOK {
		t.Fatalf("%s.%s = %#v, not a normalized link", node, input, v)
	}
	return ref{node: id, out: int(idx)}
}

func wantMsg(t *testing.T, e *Error, want string) {
	t.Helper()
	if !strings.Contains(e.Msg, want) {
		t.Fatalf("Msg = %q, want it to contain %q", e.Msg, want)
	}
}

func TestCompileWithoutInstances(t *testing.T) {
	g := hiresGraph(t, loadHires(t))
	delete(g, "21")
	g["18"].Inputs["samples"] = lk("17", 0)
	before, err := json.Marshal(g)
	if err != nil {
		t.Fatal(err)
	}
	r, err := Compile(g, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if reflect.ValueOf(r.Prompt).Pointer() != reflect.ValueOf(g).Pointer() {
		t.Fatal("Prompt is not the graph that was passed in")
	}
	after, err := json.Marshal(r.Prompt)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("bytes changed:\n%s\n%s", before, after)
	}
	if len(r.Sources) != 0 || len(r.Results) != 0 {
		t.Fatalf("Sources %v, Results %v", r.Sources, r.Results)
	}
}

func TestCompileGroup(t *testing.T) {
	r := mustCompile(t, hiresGraph(t, loadHires(t)), loadHires(t))
	if r.Prompt["21"] != nil {
		t.Fatal("the instance is still in the graph")
	}
	want := []string{"4", "6", "16", "17", "18", "19", "21.1", "21.2"}
	if got := r.Prompt.IDs(); !reflect.DeepEqual(got, want) {
		t.Fatalf("nodes %v, want %v", got, want)
	}
	if c := r.Prompt["21.1"].Class; c != "LatentUpscaleBy" {
		t.Fatalf("21.1 is %s", c)
	}
	if c := r.Prompt["21.2"].Class; c != "KSampler" {
		t.Fatalf("21.2 is %s", c)
	}
	links := []struct {
		node, input string
		want        ref
	}{
		{"21.1", "samples", ref{"17", 0}},
		{"21.2", "latent_image", ref{"21.1", 0}},
		{"21.2", "model", ref{"4", 0}},
		{"21.2", "positive", ref{"6", 0}},
		{"21.2", "negative", ref{"6", 0}},
		{"18", "samples", ref{"21.2", 0}},
		{"18", "vae", ref{"4", 2}},
	}
	for _, l := range links {
		if got := link(t, r, l.node, l.input); got != l.want {
			t.Errorf("%s.%s links %v, want %v", l.node, l.input, got, l.want)
		}
	}
	values := []struct {
		node, input string
		want        any
	}{
		{"21.2", "seed", 7.0},
		{"21.2", "steps", 14.0},
		{"21.2", "denoise", 0.45},
		{"21.2", "cfg", 3.5},
		{"21.1", "scale_by", 1.5},
		{"21.1", "upscale_method", "nearest-exact"},
	}
	for _, v := range values {
		if got := value(t, r, v.node, v.input); got != v.want {
			t.Errorf("%s.%s = %#v, want %#v", v.node, v.input, got, v.want)
		}
	}
	src := Source{Instances: []string{"21"}, Blueprint: "test/hires@1.0.0"}
	if len(r.Sources) != 2 || !reflect.DeepEqual(r.Sources["21.1"], src) || !reflect.DeepEqual(r.Sources["21.2"], src) {
		t.Fatalf("Sources = %+v, want 21.1 and 21.2 from %+v", r.Sources, src)
	}
	if len(r.Results) != 0 {
		t.Fatalf("Results = %v for a blueprint with no result", r.Results)
	}
}

func TestCompileLeavesInputsAlone(t *testing.T) {
	hires := loadHires(t)
	g := hiresGraph(t, hires)
	graphBefore, _ := json.Marshal(g)
	groupBefore, _ := json.Marshal(hires.Group)
	manifestBefore, _ := json.Marshal(hires.Manifest)

	first := mustCompile(t, g, hires)
	second := mustCompile(t, g, hires)

	graphAfter, _ := json.Marshal(g)
	groupAfter, _ := json.Marshal(hires.Group)
	manifestAfter, _ := json.Marshal(hires.Manifest)
	if !bytes.Equal(graphBefore, graphAfter) {
		t.Errorf("graph changed:\n%s\n%s", graphBefore, graphAfter)
	}
	if !bytes.Equal(groupBefore, groupAfter) {
		t.Errorf("graph.json changed:\n%s\n%s", groupBefore, groupAfter)
	}
	if !bytes.Equal(manifestBefore, manifestAfter) {
		t.Errorf("manifest changed:\n%s\n%s", manifestBefore, manifestAfter)
	}
	a, _ := json.Marshal(first)
	b, _ := json.Marshal(second)
	if !bytes.Equal(a, b) {
		t.Errorf("two compiles differ:\n%s\n%s", a, b)
	}
}

func TestSettingDefault(t *testing.T) {
	hires := loadHires(t)
	g := hiresGraph(t, hires)
	delete(g["21"].Inputs, "denoise")
	if got := value(t, mustCompile(t, g, hires), "21.2", "denoise"); got != 0.45 {
		t.Fatalf("denoise = %v, want 0.45", got)
	}

	// The default comes from the manifest, not from the literal in graph.json.
	other := hiresVariant(t, "graph.json", `"denoise": 0.45`, `"denoise": 0.99`)
	g = hiresGraph(t, other)
	delete(g["21"].Inputs, "denoise")
	if got := value(t, mustCompile(t, g, other), "21.2", "denoise"); got != 0.45 {
		t.Fatalf("denoise = %v, want the manifest default 0.45", got)
	}
}

func TestSettingOutOfRange(t *testing.T) {
	hires := loadHires(t)
	g := hiresGraph(t, hires)
	g["21"].Inputs["denoise"] = 1.5
	e := compileError(t, g, hires)
	if e.Setting != "denoise" {
		t.Fatalf("Setting = %q", e.Setting)
	}
	wantMsg(t, e, "at most 1")
	if got, want := e.Error(), "21 (test/hires@1.0.0): setting denoise: must be at most 1"; got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}

	g = hiresGraph(t, hires)
	g["21"].Inputs["scale_by"] = 0.5
	e = compileError(t, g, hires)
	if e.Setting != "scale_by" {
		t.Fatalf("Setting = %q", e.Setting)
	}
	wantMsg(t, e, "at least 1")
}

func TestSettingWrongType(t *testing.T) {
	cases := []struct {
		setting string
		value   any
		want    string
	}{
		{"steps", "20", "must be a number"},
		{"steps", 14.5, "whole number"},
		{"seed", 7.5, "whole number"},
		{"seed", -1.0, "0 or more"},
		{"scale_by", true, "must be a number"},
		{"denoise", nil, "must be a number"},
	}
	hires := loadHires(t)
	for _, c := range cases {
		t.Run(fmt.Sprintf("%s=%v", c.setting, c.value), func(t *testing.T) {
			g := hiresGraph(t, hires)
			g["21"].Inputs[c.setting] = c.value
			e := compileError(t, g, hires)
			if e.Setting != c.setting {
				t.Fatalf("Setting = %q, want %q (%s)", e.Setting, c.setting, e)
			}
			wantMsg(t, e, c.want)
		})
	}
}

func TestLinkedSeed(t *testing.T) {
	hires := loadHires(t)
	g := hiresGraph(t, hires)
	g["30"] = &graph.Node{Class: "SeedNode", Inputs: map[string]any{}}
	g["21"].Inputs["seed"] = lk("30", 0)
	if got := link(t, mustCompile(t, g, hires), "21.2", "seed"); got != (ref{"30", 0}) {
		t.Fatalf("seed links %v, want 30:0", got)
	}
}

func TestUnlinkableSetting(t *testing.T) {
	hires := loadHires(t)
	g := hiresGraph(t, hires)
	g["30"] = &graph.Node{Class: "SeedNode", Inputs: map[string]any{}}
	g["21"].Inputs["steps"] = lk("30", 0)
	e := compileError(t, g, hires)
	if e.Msg != "setting steps is not linkable" || e.Setting != "steps" {
		t.Fatalf("got %+v", e)
	}
	if got, want := e.Error(), "21 (test/hires@1.0.0): setting steps is not linkable"; got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}
}

func TestUnknownInstanceInput(t *testing.T) {
	hires := loadHires(t)
	g := hiresGraph(t, hires)
	g["21"].Inputs["sharpness"] = 0.5
	wantMsg(t, compileError(t, g, hires), "input sharpness is not a port or setting")
}

func TestDigestProblems(t *testing.T) {
	hires := loadHires(t)
	newer := hiresVariant(t, "blueprint.json", `"version": "1.0.0"`, `"version": "1.0.1"`)
	other := hiresVariant(t, "blueprint.json", `"id": "test/hires"`, `"id": "test/other"`)
	cases := []struct {
		name string
		edit func(n *graph.Node)
		want string
	}{
		{"no _meta", func(n *graph.Node) { n.Meta = nil }, "has no blueprint digest"},
		{"no digest in _meta", func(n *graph.Node) { n.Meta = &graph.Meta{Title: "Hires pass"} }, "has no blueprint digest"},
		{"digest is another version", func(n *graph.Node) { n.Meta.Blueprint = newer.Digest },
			"digest is test/hires@1.0.1, class says @1.0.0"},
		{"digest is another blueprint", func(n *graph.Node) { n.Meta.Blueprint = other.Digest },
			"digest is test/other@1.0.0, class says test/hires@1.0.0"},
		{"digest unknown", func(n *graph.Node) { n.Meta.Blueprint = digest64 }, "no package " + digest64},
		{"malformed class", func(n *graph.Node) { n.Class = "bp:Hires" }, `"bp:Hires" is not a well-formed blueprint class`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g := hiresGraph(t, hires)
			c.edit(g["21"])
			e := compileError(t, g, hires, newer, other)
			wantMsg(t, e, c.want)
			if !reflect.DeepEqual(e.Instances, []string{"21"}) {
				t.Fatalf("Instances = %v", e.Instances)
			}
		})
	}
}

// liar is a resolver that serves one package whatever digest it is asked for.
type liar struct{ p *Package }

func (l liar) Package(string) (*Package, error) { return l.p, nil }

func TestResolverServesAnotherDigest(t *testing.T) {
	hires := loadHires(t)
	g := hiresGraph(t, hires)
	g["21"].Meta.Blueprint = digest64
	_, err := Compile(g, Options{Packages: liar{hires}})
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("err = %v", err)
	}
	wantMsg(t, e, "asked for "+digest64)
}

func TestUnconnectedPort(t *testing.T) {
	hires := loadHires(t)
	g := hiresGraph(t, hires)
	delete(g["21"].Inputs, "samples")
	e := compileError(t, g, hires)
	if e.Msg != "input samples is not connected" {
		t.Fatalf("Msg = %q", e.Msg)
	}

	g = hiresGraph(t, hires)
	g["21"].Inputs["samples"] = 3.0
	wantMsg(t, compileError(t, g, hires), "input samples must be a link")

	// An optional port with nothing behind it drops the inputs it feeds.
	opt := hiresVariant(t, "blueprint.json", `{ "name": "negative", "type": "CONDITIONING",`,
		`{ "name": "negative", "type": "CONDITIONING", "optional": true,`)
	g = hiresGraph(t, opt)
	delete(g["21"].Inputs, "negative")
	r := mustCompile(t, g, opt)
	if v, ok := r.Prompt["21.2"].Inputs["negative"]; ok {
		t.Fatalf("21.2 negative = %v, want it left out", v)
	}
}

func TestOutputIndexOutOfRange(t *testing.T) {
	hires := loadHires(t)
	cases := []struct {
		name     string
		consumer string
		input    string
		idx      int
	}{
		{"past the outputs", "18", "samples", 3},
		{"just past the outputs", "18", "samples", 1},
		{"negative", "18", "samples", -1},
		{"into another instance", "22", "samples", 3},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g := hiresGraph(t, hires)
			g["22"] = hiresInstance(hires, lk("21", 0))
			g["18"].Inputs["samples"] = lk("22", 0)
			g[c.consumer].Inputs[c.input] = lk("21", c.idx)
			e := compileError(t, g, hires)
			wantMsg(t, e, fmt.Sprintf("node %s input %s links output %d", c.consumer, c.input, c.idx))
			if !reflect.DeepEqual(e.Instances, []string{"21"}) || e.Blueprint != "test/hires@1.0.0" {
				t.Fatalf("got %+v", e)
			}
		})
	}
}

func TestIDCollision(t *testing.T) {
	hires := loadHires(t)
	g := hiresGraph(t, hires)
	g["21.1"] = &graph.Node{Class: "VAEDecode", Inputs: map[string]any{"samples": lk("17", 0), "vae": lk("4", 2)}}
	wantMsg(t, compileError(t, g, hires), "21.1 is already a node in the graph")
}

func TestInstanceFeedsInstance(t *testing.T) {
	hires := loadHires(t)
	g := hiresGraph(t, hires)
	g["22"] = hiresInstance(hires, lk("21", 0))
	g["18"].Inputs["samples"] = lk("22", 0)
	r := mustCompile(t, g, hires)
	if got := link(t, r, "22.1", "samples"); got != (ref{"21.2", 0}) {
		t.Fatalf("22.1 samples links %v", got)
	}
	if got := link(t, r, "18", "samples"); got != (ref{"22.2", 0}) {
		t.Fatalf("18 samples links %v", got)
	}

	// The producer compiles first even when its id sorts after the consumer's.
	g = hiresGraph(t, hires)
	g["5"] = hiresInstance(hires, lk("21", 0))
	g["18"].Inputs["samples"] = lk("5", 0)
	r = mustCompile(t, g, hires)
	if got := link(t, r, "5.1", "samples"); got != (ref{"21.2", 0}) {
		t.Fatalf("5.1 samples links %v", got)
	}

	g = hiresGraph(t, hires)
	g["22"] = hiresInstance(hires, lk("21", 0))
	g["21"].Inputs["samples"] = lk("22", 0)
	wantMsg(t, compileError(t, g, hires), "cycle")
}

// savePackage is a node group around one SaveImage, which is its result.
func savePackage(t *testing.T) *Package {
	t.Helper()
	manifest := fmt.Sprintf(`{
  "format": 1, "id": "test/save", "version": "1.0.0", "title": "Save",
  "inputs": [{ "name": "images", "type": "IMAGE", "to": [{ "nodeId": "3", "input": "images" }] }],
  "outputs": [],
  "settings": [{ "name": "prefix", "kind": "string", "default": "bp",
                 "to": [{ "nodeId": "3", "input": "filename_prefix" }] }],
  "result": "3",
  "requires": { "classes": { "SaveImage": %q } }
}`, fixtureSchema(t).Classes["SaveImage"].Hash())
	group := `{ "3": { "class_type": "SaveImage", "inputs": { "images": ["in", 0], "filename_prefix": "bp" } } }`
	return mustLoad(t, fstest.MapFS{
		"blueprint.json": {Data: []byte(manifest)},
		"graph.json":     {Data: []byte(group)},
	})
}

func TestResultNode(t *testing.T) {
	hires, save := loadHires(t), savePackage(t)
	g := hiresGraph(t, hires)
	delete(g, "19")
	g["40"] = &graph.Node{
		Class:  "bp:test/save@1.0.0",
		Inputs: map[string]any{"images": lk("18", 0), "prefix": "harbor"},
		Meta:   &graph.Meta{Blueprint: save.Digest},
	}
	r := mustCompile(t, g, hires, save)
	if want := map[string]string{"40": "40.3"}; !reflect.DeepEqual(r.Results, want) {
		t.Fatalf("Results = %v, want %v", r.Results, want)
	}
	if got := value(t, r, "40.3", "filename_prefix"); got != "harbor" {
		t.Fatalf("filename_prefix = %v", got)
	}
	if got := link(t, r, "40.3", "images"); got != (ref{"18", 0}) {
		t.Fatalf("images links %v", got)
	}
}

func TestEmittedClassMustBeRequired(t *testing.T) {
	hires := loadHires(t)
	// A package that Load accepted, then lost a class from requires.
	trimmed := *hires
	m := *hires.Manifest
	m.Requires.Classes = map[string]string{"KSampler": hires.Manifest.Requires.Classes["KSampler"]}
	trimmed.Manifest = &m
	g := hiresGraph(t, hires)
	wantMsg(t, compileError(t, g, &trimmed), "LatentUpscaleBy is not in requires.classes")
}

func TestErrorString(t *testing.T) {
	cases := []struct {
		e    Error
		want string
	}{
		{Error{Msg: "node 7 is null"}, "node 7 is null"},
		{Error{Instances: []string{"14"}, Blueprint: "test/hires@1.0.0", Msg: "has no blueprint digest"},
			"14 (test/hires@1.0.0): has no blueprint digest"},
		{Error{Instances: []string{"14", "base"}, Blueprint: "test/hires@1.0.0", Setting: "denoise", Msg: "must be at most 1"},
			"14.base (test/hires@1.0.0): setting denoise: must be at most 1"},
		{Error{Instances: []string{"14"}, Blueprint: "test/ref-group@1.0.0", Item: &Item{Setting: "images", Index: 1}, Msg: "boom"},
			"14 (test/ref-group@1.0.0) images[1]: boom"},
	}
	for _, c := range cases {
		if got := c.e.Error(); got != c.want {
			t.Errorf("Error() = %q, want %q", got, c.want)
		}
	}
}

func TestSettingValues(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	cases := []struct {
		name string
		s    Setting
		v    any
		want string // "" when the value is good
	}{
		{"int", Setting{Kind: KindInt}, 3.0, ""},
		{"int built in Go", Setting{Kind: KindInt}, 3, ""},
		{"int not whole", Setting{Kind: KindInt}, 3.5, "must be a whole number"},
		{"int as string", Setting{Kind: KindInt}, "3", "must be a number"},
		{"int below min", Setting{Kind: KindInt, Min: f(1)}, 0.0, "must be at least 1"},
		{"float above max", Setting{Kind: KindFloat, Max: f(4)}, 4.5, "must be at most 4"},
		{"float at max", Setting{Kind: KindFloat, Max: f(4)}, 4.0, ""},
		{"seed", Setting{Kind: KindSeed}, 0.0, ""},
		{"seed negative", Setting{Kind: KindSeed}, -2.0, "must be a whole number of 0 or more"},
		{"string", Setting{Kind: KindString}, "x", ""},
		{"string as number", Setting{Kind: KindString}, 1.0, "must be a string"},
		{"bool", Setting{Kind: KindBool}, false, ""},
		{"bool as string", Setting{Kind: KindBool}, "true", "must be true or false"},
		{"combo option", Setting{Kind: KindCombo, Options: []string{"a", "b"}}, "b", ""},
		{"combo not an option", Setting{Kind: KindCombo, Options: []string{"a", "b"}}, "c", `must be one of "a", "b"`},
		{"combo from models", Setting{Kind: KindCombo, Source: &ModelSource{Kind: "models", Folder: "loras"}}, "any.safetensors", ""},
		{"combo from models as number", Setting{Kind: KindCombo, Source: &ModelSource{Kind: "models", Folder: "loras"}}, 2.0, "must be a string"},
		{"image", Setting{Kind: KindImage}, "a.png", ""},
		{"image as list", Setting{Kind: KindImage}, []any{"a.png"}, "must be a string"},
		{"list", Setting{Kind: KindImage, List: &ListBounds{Max: 2}}, []any{"a.png", "b.png"}, ""},
		{"list empty", Setting{Kind: KindImage, List: &ListBounds{Max: 2}}, []any{}, ""},
		{"list not a list", Setting{Kind: KindImage, List: &ListBounds{Max: 2}}, "a.png", "must be a list"},
		{"list too long", Setting{Kind: KindImage, List: &ListBounds{Max: 2}}, []any{"a", "b", "c"}, "at most 2 items"},
		{"list too short", Setting{Kind: KindImage, List: &ListBounds{Min: 1, Max: 2}}, []any{}, "at least 1 item"},
		{"list element", Setting{Kind: KindFloat, List: &ListBounds{Max: 4}, Max: f(1)}, []any{0.5, 2.0}, "item 1 must be at most 1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := settingProblem(c.s, c.v)
			switch {
			case c.want == "" && got != "":
				t.Fatalf("problem %q, want none", got)
			case c.want != "" && !strings.Contains(got, c.want):
				t.Fatalf("problem %q, want it to contain %q", got, c.want)
			}
		})
	}
}

func TestDefaultsFillOptions(t *testing.T) {
	got := Options{}.withDefaults()
	if got.MaxNodes != 2000 || got.MaxDepth != 8 || got.Steps != 10_000_000 || got.Timeout != 2*time.Second {
		t.Fatalf("defaults = %+v", got)
	}
}
