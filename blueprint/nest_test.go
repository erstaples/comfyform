package blueprint

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/erstaples/comfyform/graph"
)

const (
	nestedProgramDir = "testdata/packages/nested-program"
	nestedGroupDir   = "testdata/packages/nested-group"
)

func loadNestedProgram(t *testing.T) *Package { return mustLoad(t, os.DirFS(nestedProgramDir)) }
func loadNestedGroup(t *testing.T) *Package   { return mustLoad(t, os.DirFS(nestedGroupDir)) }

// hiresOuterIDs are the nodes of nestedGraph that are not an instance.
var hiresOuterIDs = []string{"4", "6", "16", "17", "18", "19"}

// latentLinks is what nestedGraph links each port it knows to.
var latentLinks = map[string][]any{
	"model": lk("4", 0), "positive": lk("6", 0), "negative": lk("6", 0),
	"samples": lk("17", 0), "latent": lk("17", 0), "vae": lk("4", 2),
}

// nestedGraph is hiresGraph with an instance "14" of p in place of the hires
// instance: each of p's input ports linked as latentLinks says, the given
// settings, and the decode taking its first output.
func nestedGraph(t *testing.T, p *Package, settings map[string]any) graph.Prompt {
	t.Helper()
	g := hiresGraph(t, loadHires(t))
	delete(g, "21")
	inputs := map[string]any{}
	for _, port := range p.Manifest.Inputs {
		l, ok := latentLinks[port.Name]
		if !ok {
			t.Fatalf("nestedGraph has no link for port %s", port.Name)
		}
		inputs[port.Name] = l
	}
	for k, v := range settings {
		inputs[k] = v
	}
	g["14"] = &graph.Node{Class: p.Manifest.Class(), Inputs: inputs, Meta: &graph.Meta{Blueprint: p.Digest}}
	g["18"].Inputs["samples"] = lk("14", 0)
	return g
}

type wantValue struct {
	node, input string
	want        any
}

func checkValues(t *testing.T, r *Result, values []wantValue) {
	t.Helper()
	for _, v := range values {
		if got := value(t, r, v.node, v.input); !reflect.DeepEqual(got, v.want) {
			t.Errorf("%s.%s = %#v, want %#v", v.node, v.input, got, v.want)
		}
	}
}

func checkSources(t *testing.T, r *Result, want Source, ids ...string) {
	t.Helper()
	for _, id := range ids {
		if got := r.Sources[id]; !reflect.DeepEqual(got, want) {
			g, _ := json.Marshal(got)
			w, _ := json.Marshal(want)
			t.Errorf("Sources[%s] = %s, want %s", id, g, w)
		}
	}
}

func seedNode() *graph.Node { return &graph.Node{Class: "SeedNode", Inputs: map[string]any{}} }

// simpleProgram is a program package id at 1.0.0 with main.star star, the
// given ports and settings, the given classes in requires, and uses pinning
// each of pkgs at its own version and digest.
func simpleProgram(t *testing.T, id string, inputs, outputs []Port, settings []Setting, classes []string, star string, pkgs ...*Package) *Package {
	t.Helper()
	s := fixtureSchema(t)
	req := map[string]string{}
	for _, c := range classes {
		req[c] = s.Classes[c].Hash()
	}
	m := Manifest{
		Format: Format, ID: id, Version: "1.0.0", Title: id,
		Inputs:   append([]Port{}, inputs...),
		Outputs:  append([]Port{}, outputs...),
		Settings: append([]Setting{}, settings...),
		Requires: Requires{Classes: req},
		Uses:     map[string]Use{},
	}
	for _, p := range pkgs {
		m.Uses[p.Manifest.ID] = Use{Version: p.Manifest.Version, Digest: p.Digest}
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return mustLoad(t, fstest.MapFS{
		"blueprint.json": {Data: b},
		"main.star":      {Data: []byte(star)},
	})
}

var (
	latentPort   = []Port{{Name: "latent", Type: "LATENT"}}
	userInputs   = []Port{{Name: "model", Type: "MODEL"}, {Name: "positive", Type: "CONDITIONING"}, {Name: "negative", Type: "CONDITIONING"}, {Name: "samples", Type: "LATENT"}, {Name: "vae", Type: "VAE", Optional: true}}
	hiresClasses = []string{"LatentUpscaleBy", "KSampler"}
)

// userPkg is a program test/user with main.star star: input ports model,
// positive, negative, samples and an optional vae, an output latent, the
// given settings, and uses pinning each of pkgs.
func userPkg(t *testing.T, star string, settings []Setting, pkgs ...*Package) *Package {
	t.Helper()
	return simpleProgram(t, "test/user", userInputs, latentPort, settings, hiresClasses, star, pkgs...)
}

// hiresPorts are the inputs of a ctx.use of hires that pass the user's own.
const hiresPorts = `{"model": inputs.model, "positive": inputs.positive, "negative": inputs.negative, "samples": inputs.samples}`

// useHires is a line that makes h, a ctx.use of hires named name with the
// user's ports and the settings dict given.
func useHires(name, settings string) string {
	return fmt.Sprintf(`h = ctx.use("test/hires", %q, inputs = %s, settings = %s)`, name, hiresPorts, settings)
}

const returnH = `return {"latent": h.latent}`

func TestUseInProgram(t *testing.T) {
	np, hires := loadNestedProgram(t), loadHires(t)
	r := mustCompile(t, nestedGraph(t, np, map[string]any{"denoise": 0.3, "seed": 7.0}), np, hires)
	checkIDs(t, r, append([]string{"14.base.1", "14.base.2"}, hiresOuterIDs...))
	checkSources(t, r, Source{Instances: []string{"14", "base"}, Blueprint: "test/hires@1.0.0"}, "14.base.1", "14.base.2")
	checkLinks(t, r, []wantLink{
		{"14.base.1", "samples", ref{"17", 0}},
		{"14.base.2", "latent_image", ref{"14.base.1", 0}},
		{"14.base.2", "model", ref{"4", 0}},
		{"14.base.2", "positive", ref{"6", 0}},
		{"14.base.2", "negative", ref{"6", 0}},
		{"18", "samples", ref{"14.base.2", 0}},
	})
	// The settings the program passes reach hires; the rest take hires's
	// defaults.
	checkValues(t, r, []wantValue{
		{"14.base.2", "denoise", 0.3},
		{"14.base.2", "seed", 7.0},
		{"14.base.2", "steps", 14.0},
		{"14.base.1", "scale_by", 1.5},
	})

	// A linked seed passes through ctx.use into hires's seed, as a link.
	g := nestedGraph(t, np, map[string]any{"seed": lk("30", 0)})
	g["30"] = seedNode()
	if got := link(t, mustCompile(t, g, np, hires), "14.base.2", "seed"); got != (ref{"30", 0}) {
		t.Fatalf("seed links %v, want 30:0", got)
	}
}

func TestGroupHoldsInstance(t *testing.T) {
	ng, hires := loadNestedGroup(t), loadHires(t)
	g := nestedGraph(t, ng, map[string]any{"denoise": 0.3, "seed": lk("30", 0)})
	g["30"] = seedNode()
	r := mustCompile(t, g, ng, hires)
	checkIDs(t, r, append([]string{"14.5.1", "14.5.2", "30"}, hiresOuterIDs...))
	checkSources(t, r, Source{Instances: []string{"14", "5"}, Blueprint: "test/hires@1.0.0"}, "14.5.1", "14.5.2")
	checkLinks(t, r, []wantLink{
		{"14.5.1", "samples", ref{"17", 0}},
		{"14.5.2", "latent_image", ref{"14.5.1", 0}},
		{"14.5.2", "model", ref{"4", 0}},
		{"14.5.2", "seed", ref{"30", 0}},
		{"18", "samples", ref{"14.5.2", 0}},
	})
	checkValues(t, r, []wantValue{
		{"14.5.1", "scale_by", 2.0}, // graph.json's literal, a setting of the nested instance
		{"14.5.2", "denoise", 0.3},  // the outer setting bound to it
		{"14.5.2", "steps", 14.0},
	})
}

// nestedGroupVariant loads nested-group with each edit applied: a file, the
// text to replace everywhere it appears in that file, and its replacement.
func nestedGroupVariant(t *testing.T, edits ...[3]string) *Package {
	t.Helper()
	fsys := dirFiles(t, nestedGroupDir)
	for _, e := range edits {
		file, old, with := e[0], e[1], e[2]
		if !bytes.Contains(fsys[file].Data, []byte(old)) {
			t.Fatalf("%s has no %q", file, old)
		}
		fsys[file] = &fstest.MapFile{Data: bytes.ReplaceAll(fsys[file].Data, []byte(old), []byte(with))}
	}
	return mustLoad(t, fsys)
}

// groupTail is the end of nested-group's node 5, where a test adds nodes.
const groupTail = `" } }`

// hiresNode is a graph.json node id, an instance of hires, with its ports
// from "in" and samples from the given link.
func hiresNode(t *testing.T, id, samples string) string {
	return fmt.Sprintf(`,
  %q: { "class_type": "bp:test/hires@1.0.0",
         "inputs": { "model": ["in", 0], "positive": ["in", 0], "negative": ["in", 0], "samples": %s,
                     "scale_by": 1.5, "denoise": 0.45, "steps": 14, "seed": 0 },
         "_meta": { "blueprint": %q } }`, id, samples, loadHires(t).Digest)
}

// alsoTo makes each of the hires ports of nested-group feed node id too.
func alsoTo(id string) [][3]string {
	var edits [][3]string
	for _, port := range []string{"model", "positive", "negative"} {
		target := fmt.Sprintf(`{ "nodeId": "5", "input": %q }`, port)
		edits = append(edits, [3]string{manifestFile, target, target + fmt.Sprintf(`, { "nodeId": %q, "input": %q }`, id, port)})
	}
	return edits
}

func TestGroupNestedLinks(t *testing.T) {
	hires := loadHires(t)
	fromNode := func(id string) [3]string {
		return [3]string{manifestFile, `"from": { "nodeId": "5", "output": 0 }`, fmt.Sprintf(`"from": { "nodeId": %q, "output": 0 }`, id)}
	}

	t.Run("a node before the instance takes its output", func(t *testing.T) {
		p := nestedGroupVariant(t, fromNode("3"), [3]string{groupFile, groupTail, groupTail + `,
  "3": { "class_type": "LatentUpscaleBy", "inputs": { "samples": ["5", 0], "upscale_method": "nearest-exact", "scale_by": 1.5 } }`})
		r := mustCompile(t, nestedGraph(t, p, nil), p, hires)
		checkIDs(t, r, append([]string{"14.3", "14.5.1", "14.5.2"}, hiresOuterIDs...))
		checkLinks(t, r, []wantLink{
			{"14.3", "samples", ref{"14.5.2", 0}},
			{"18", "samples", ref{"14.3", 0}},
		})
		if got := r.Sources["14.3"]; !reflect.DeepEqual(got.Instances, []string{"14"}) || got.Blueprint != "test/nested-group@1.0.0" {
			t.Fatalf("Sources[14.3] = %+v", got)
		}
	})

	t.Run("an instance takes another's output", func(t *testing.T) {
		p := nestedGroupVariant(t, append(alsoTo("2"), fromNode("2"), [3]string{groupFile, groupTail, groupTail + hiresNode(t, "2", `["5", 0]`)})...)
		r := mustCompile(t, nestedGraph(t, p, nil), p, hires)
		checkIDs(t, r, append([]string{"14.2.1", "14.2.2", "14.5.1", "14.5.2"}, hiresOuterIDs...))
		checkLinks(t, r, []wantLink{
			{"14.5.1", "samples", ref{"17", 0}},
			{"14.2.1", "samples", ref{"14.5.2", 0}},
			{"14.2.2", "model", ref{"4", 0}},
			{"18", "samples", ref{"14.2.2", 0}},
		})
		checkSources(t, r, Source{Instances: []string{"14", "2"}, Blueprint: "test/hires@1.0.0"}, "14.2.1", "14.2.2")
	})

	t.Run("instances that link in a cycle", func(t *testing.T) {
		p := nestedGroupVariant(t, append(alsoTo("2"),
			[3]string{manifestFile, `,
    { "name": "samples", "type": "LATENT", "to": [{ "nodeId": "5", "input": "samples" }] }`, ``},
			[3]string{groupFile, `"samples": ["in", 0]`, `"samples": ["2", 0]`},
			[3]string{groupFile, groupTail, groupTail + hiresNode(t, "2", `["5", 0]`)})...)
		e := compileError(t, nestedGraph(t, p, nil), p, hires)
		wantMsg(t, e, "link in a cycle: 14.2 -> 14.5 -> 14.2")
	})

	t.Run("a link past the instance's outputs", func(t *testing.T) {
		p := nestedGroupVariant(t, fromNode("3"), [3]string{groupFile, groupTail, groupTail + `,
  "3": { "class_type": "LatentUpscaleBy", "inputs": { "samples": ["5", 1], "upscale_method": "nearest-exact", "scale_by": 1.5 } }`})
		e := compileError(t, nestedGraph(t, p, nil), p, hires)
		wantMsg(t, e, "output 1 of node 5")
		wantMsg(t, e, "test/hires@1.0.0 has 1 output")
		if !reflect.DeepEqual(e.Instances, []string{"14"}) {
			t.Fatalf("Instances = %v", e.Instances)
		}
	})
}

func TestGroupNestedErrors(t *testing.T) {
	hires := loadHires(t)
	cases := []struct {
		name     string
		edits    [][3]string
		settings map[string]any
		drop     string // an input of the outer instance to leave out
		setting  string
		want     string
	}{
		{"a setting the outer instance allows and the nested one does not",
			[][3]string{{manifestFile, `"default": 0.45, "min": 0, "max": 1,`, `"default": 0.45, "min": 0, "max": 2,`}},
			map[string]any{"denoise": 1.5}, "", "denoise", "must be at most 1"},
		{"an input the nested blueprint does not have",
			[][3]string{{groupFile, `"scale_by": 2.0,`, `"scale_by": 2.0, "sharpness": 1,`}},
			nil, "", "", "input sharpness is not a port or setting of the blueprint"},
		{"a port that nothing feeds",
			[][3]string{{manifestFile, `{ "name": "samples", "type": "LATENT",`, `{ "name": "samples", "type": "LATENT", "optional": true,`}},
			nil, "samples", "", "input samples is not connected"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := nestedGroupVariant(t, c.edits...)
			g := nestedGraph(t, p, c.settings)
			delete(g["14"].Inputs, c.drop)
			e := compileError(t, g, p, hires)
			wantMsg(t, e, c.want)
			if !reflect.DeepEqual(e.Instances, []string{"14", "5"}) || e.Blueprint != "test/hires@1.0.0" || e.Setting != c.setting {
				t.Fatalf("got %+v", e)
			}
		})
	}
}

// TestGroupNestedClass covers a group node whose class does not agree with
// uses, which Load refuses, so compile meets it only in a package it did not
// load.
func TestGroupNestedClass(t *testing.T) {
	hires := loadHires(t)
	cases := []struct {
		name, file, old, with, want string
	}{
		{"another version", groupFile, `"bp:test/hires@1.0.0"`, `"bp:test/hires@1.0.1"`, "node 5: uses pins test/hires at 1.0.0, not 1.0.1"},
		{"not in uses", manifestFile, `"test/hires": {`, `"test/other": {`, "test/hires is not in uses"},
		{"malformed", groupFile, `"bp:test/hires@1.0.0"`, `"bp:Hires"`, `node 5: "bp:Hires" is not a well-formed blueprint class`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fsys := dirFiles(t, nestedGroupDir)
			replaceIn(t, fsys, c.file, c.old, c.with)
			p := loadUnchecked(t, fsys)
			e := compileError(t, nestedGraph(t, p, nil), p, hires)
			wantMsg(t, e, c.want)
			if !reflect.DeepEqual(e.Instances, []string{"14"}) || e.Blueprint != "test/nested-group@1.0.0" {
				t.Fatalf("got %+v", e)
			}
		})
	}
}

func TestUseUnknown(t *testing.T) {
	p := userPkg(t, prog(`ctx.use("test/x", "x")`, `return {"latent": inputs.samples}`), nil, loadHires(t))
	e := compileError(t, nestedGraph(t, p, nil), p, loadHires(t))
	if e.Msg != "main.star:2: test/x is not in uses" {
		t.Fatalf("Msg = %q", e.Msg)
	}
	if !reflect.DeepEqual(e.Instances, []string{"14"}) || e.Blueprint != "test/user@1.0.0" {
		t.Fatalf("got %+v", e)
	}
}

func TestUseErrors(t *testing.T) {
	hires := loadHires(t)
	ports := func(samples string) string {
		return `{"model": inputs.model, "positive": inputs.positive, "negative": inputs.negative, "samples": ` + samples + `}`
	}
	use := func(inputs, settings string) string {
		return fmt.Sprintf(`h = ctx.use("test/hires", "h", inputs = %s, settings = %s)`, inputs, settings)
	}
	upscale := `up = ctx.node("up", "LatentUpscaleBy", samples = inputs.samples, upscale_method = "nearest-exact", scale_by = 1.5)`
	outer, nested := []string{"14"}, []string{"14", "h"}
	cases := []struct {
		name      string
		lines     []string
		want      string
		instances []string
		setting   string
	}{
		{"bad name", []string{`h = ctx.use("test/hires", "a.b", inputs = ` + hiresPorts + `)`}, `main.star:2: invalid use name "a.b"`, outer, ""},
		{"inputs not a dict", []string{`h = ctx.use("test/hires", "h", inputs = [1])`}, `for parameter "inputs": got list, want dict`, outer, ""},
		{"a key that is not a string", []string{use(`{1: inputs.model}`, `{}`)}, "main.star:2: inputs has key 1, which is not a string", outer, ""},
		{"unknown input", []string{use(`{"latent": inputs.samples}`, `{}`)}, "inputs has latent, which is not an input port of the blueprint", nested, ""},
		{"unknown setting", []string{use(hiresPorts, `{"sharpness": 1}`)}, "settings has sharpness, which is not a setting of the blueprint", nested, ""},
		{"port given a literal", []string{use(ports(`3`), `{}`)}, "input samples must be a socket reference, got int", nested, ""},
		{"port given a node", []string{upscale, use(ports(`up`), `{}`)}, "is a node, not a socket; use .OUTPUT or .out(i)", nested, ""},
		{"port of another type", []string{use(ports(`inputs.model`), `{}`)}, "input samples takes LATENT, got MODEL", nested, ""},
		{"port left out", []string{use(`{"model": inputs.model, "positive": inputs.positive, "negative": inputs.negative}`, `{}`)}, "input samples is not connected", nested, ""},
		{"port given None", []string{use(ports(`None`), `{}`)}, "input samples is not connected", nested, ""},
		{"setting out of range", []string{use(hiresPorts, `{"denoise": 1.5}`)}, "must be at most 1", nested, "denoise"},
		{"setting of the wrong kind", []string{use(hiresPorts, `{"steps": "20"}`)}, "must be a number", nested, "steps"},
		{"setting given a dict", []string{use(hiresPorts, `{"steps": {}}`)}, "cannot take a dict", nested, "steps"},
		{"socket for a setting that is not linkable", []string{use(hiresPorts, `{"steps": inputs.samples}`)}, "setting steps is not linkable", nested, "steps"},
		{"a literal that reads as a link", []string{use(hiresPorts, `{"seed": ["6", 0]}`)}, "reads as a link; pass a socket reference", nested, "seed"},
		{"the same name twice", []string{use(hiresPorts, `{}`), use(hiresPorts, `{}`)}, "duplicate node id 14.h.1", nested, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := userPkg(t, prog(append(c.lines, `return {"latent": inputs.samples}`)...), nil, hires)
			e := compileError(t, nestedGraph(t, p, nil), p, hires)
			wantMsg(t, e, c.want)
			if !reflect.DeepEqual(e.Instances, c.instances) || e.Setting != c.setting {
				t.Fatalf("got %+v", e)
			}
			// An error about the nested instance says where it is by its
			// chain, not by a line of the outer program.
			if nestedErr := len(c.instances) > 1; nestedErr == strings.HasPrefix(e.Msg, "main.star:") {
				t.Fatalf("Msg = %q", e.Msg)
			}
		})
	}

	// Validated like an outer instance's setting, and written the same way.
	p := userPkg(t, prog(use(hiresPorts, `{"denoise": 1.5}`), returnH), nil, hires)
	e := compileError(t, nestedGraph(t, p, nil), p, hires)
	if got, want := e.Error(), "14.h (test/hires@1.0.0): setting denoise: must be at most 1"; got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}
}

// TestUseLinkedSettingAsPort: a linked setting's reference passes into
// ctx.node, or into a linkable setting of a nested instance, and nothing
// else, so not into a nested instance's port.
func TestUseLinkedSettingAsPort(t *testing.T) {
	optional := []Port{{Name: "samples", Type: "LATENT", Optional: true}}
	passes := simpleProgram(t, "test/passes", optional, latentPort, nil, nil, prog(`return {"latent": inputs.samples}`))
	seed := Setting{Name: "seed", Kind: KindSeed, Default: 0.0}
	p := userPkg(t, prog(`o = ctx.use("test/passes", "o", inputs = {"samples": settings.seed})`, `return {"latent": o.latent}`),
		[]Setting{seed}, passes)
	g := nestedGraph(t, p, map[string]any{"seed": lk("30", 0)})
	g["30"] = seedNode()
	e := compileError(t, g, p, passes)
	if e.Msg != "main.star:2: setting seed is linked; this blueprint needs a value" || e.Setting != "seed" ||
		!reflect.DeepEqual(e.Instances, []string{"14"}) {
		t.Fatalf("got %+v", e)
	}
}

func TestUseOutputs(t *testing.T) {
	hires := loadHires(t)
	optional := []Port{{Name: "samples", Type: "LATENT", Optional: true}}
	passes := simpleProgram(t, "test/passes", optional, latentPort, nil, nil, prog(`return {"latent": inputs.samples}`))
	p := userPkg(t, prog(
		useHires("h", `{}`),
		`if type(h.latent) != "ref" or not hasattr(h, "latent") or hasattr(h, "nope"):`,
		`    fail("h is %s" % h)`,
		`o = ctx.use("test/passes", "o")`,
		`if o.latent != None:`,
		`    fail("o.latent is %s" % o.latent)`,
		`ctx.node("up", "LatentUpscaleBy", samples = h.latent, upscale_method = "nearest-exact", scale_by = 1.5)`,
		returnH), nil, hires, passes)
	r := mustCompile(t, nestedGraph(t, p, nil), p, hires, passes)
	checkLinks(t, r, []wantLink{
		{"14.up", "samples", ref{"14.h.2", 0}},
		{"18", "samples", ref{"14.h.2", 0}},
	})

	// An error inside a nested program passes out as it is, with that
	// program's line and the nested chain.
	bad := simpleProgram(t, "test/bad", latentPort, latentPort, nil, nil, prog(`fail("boom")`))
	p = userPkg(t, prog(`b = ctx.use("test/bad", "b", inputs = {"latent": inputs.samples})`, `return {"latent": b.latent}`), nil, bad)
	e := compileError(t, nestedGraph(t, p, nil), p, bad)
	if e.Msg != "main.star:2: fail: boom" || !reflect.DeepEqual(e.Instances, []string{"14", "b"}) || e.Blueprint != "test/bad@1.0.0" {
		t.Fatalf("got %+v", e)
	}
}

func TestUseDigestMismatch(t *testing.T) {
	np, ng, hires := loadNestedProgram(t), loadNestedGroup(t), loadHires(t)
	// posing is p served as hires: the digest hires's uses pins, p's files.
	posing := func(p *Package) *Package {
		c := *p
		c.Digest = hires.Digest
		return &c
	}
	newer := hiresVariant(t, "blueprint.json", `"version": "1.0.0"`, `"version": "1.0.1"`)
	other := hiresVariant(t, "blueprint.json", `"id": "test/hires"`, `"id": "test/other"`)
	cases := []struct {
		name   string
		served resolver
		want   string
	}{
		{"another version", resolver{hires.Digest: posing(newer)}, "uses says test/hires@1.0.0, digest is @1.0.1"},
		{"another blueprint", resolver{hires.Digest: posing(other)}, "uses says test/hires@1.0.0, digest is test/other@1.0.0"},
		{"another digest", resolver{hires.Digest: newer}, "the resolver served " + newer.Digest + " when asked for " + hires.Digest},
		{"no package", resolver{}, "cannot load " + hires.Digest},
	}
	for _, outer := range []struct {
		p    *Package
		name string
	}{{np, "base"}, {ng, "5"}} {
		for _, c := range cases {
			t.Run(outer.p.Manifest.ID+"/"+c.name, func(t *testing.T) {
				served := resolver{outer.p.Digest: outer.p}
				for k, v := range c.served {
					served[k] = v
				}
				_, err := Compile(context.Background(), nestedGraph(t, outer.p, nil), Options{Classes: fixtureClasses(t), Packages: served})
				e := asError(t, err)
				wantMsg(t, e, c.want)
				if !reflect.DeepEqual(e.Instances, []string{"14", outer.name}) {
					t.Fatalf("Instances = %v", e.Instances)
				}
			})
		}
	}
}

// chain is n programs test/p0 to test/p<n-1>, top first. Each but the last
// passes its latent through a ctx.use of the next, named n; the last makes a
// LatentUpscaleBy named up. Their digests are worked out from the bottom up.
func chain(t *testing.T, n int) []*Package {
	t.Helper()
	pkgs := make([]*Package, n)
	for i := n - 1; i >= 0; i-- {
		id := fmt.Sprintf("test/p%d", i)
		if i == n-1 {
			pkgs[i] = simpleProgram(t, id, latentPort, latentPort, nil, []string{"LatentUpscaleBy"}, prog(
				`return {"latent": ctx.node("up", "LatentUpscaleBy", samples = inputs.latent, upscale_method = "nearest-exact", scale_by = 1.5).LATENT}`))
			continue
		}
		pkgs[i] = simpleProgram(t, id, latentPort, latentPort, nil, nil, prog(
			fmt.Sprintf(`return {"latent": ctx.use("test/p%d", "n", inputs = {"latent": inputs.latent}).latent}`, i+1)), pkgs[i+1])
	}
	return pkgs
}

// outerGroup is a node group test/outer-group holding a nested-group
// instance as node 7, with nested-group's interface.
func outerGroup(t *testing.T, ng *Package) *Package {
	t.Helper()
	fsys := dirFiles(t, nestedGroupDir)
	fsys[manifestFile].Data = bytes.ReplaceAll(fsys[manifestFile].Data, []byte(`"nodeId": "5"`), []byte(`"nodeId": "7"`))
	replaceIn(t, fsys, manifestFile, `"id": "test/nested-group"`, `"id": "test/outer-group"`)
	replaceIn(t, fsys, manifestFile, `"test/hires": { "version": "1.0.0", "digest": "`+loadHires(t).Digest+`" }`,
		`"test/nested-group": { "version": "1.0.0", "digest": "`+ng.Digest+`" }`)
	fsys[groupFile] = &fstest.MapFile{Data: []byte(fmt.Sprintf(`{
  "7": { "class_type": "bp:test/nested-group@1.0.0",
         "inputs": { "model": ["in", 0], "positive": ["in", 0], "negative": ["in", 0], "samples": ["in", 0],
                     "denoise": 0.45, "seed": 0 },
         "_meta": { "blueprint": %q } }
}`, ng.Digest))}
	return mustLoad(t, fsys)
}

func TestDepthLimit(t *testing.T) {
	compile := func(pkgs []*Package, maxDepth int) (*Result, error) {
		opt := options(t, pkgs...)
		opt.MaxDepth = maxDepth
		return Compile(context.Background(), nestedGraph(t, pkgs[0], nil), opt)
	}

	// The outer instance and 8 levels below it compile.
	pkgs := chain(t, 9)
	r, err := compile(pkgs, 0)
	if err != nil {
		t.Fatalf("9 packages: %v", err)
	}
	inner := "14" + strings.Repeat(".n", 8) + ".up"
	checkIDs(t, r, append([]string{inner}, hiresOuterIDs...))
	checkLinks(t, r, []wantLink{{inner, "samples", ref{"17", 0}}, {"18", "samples", ref{inner, 0}}})
	want := Source{Instances: append([]string{"14"}, slices.Repeat([]string{"n"}, 8)...), Blueprint: "test/p8@1.0.0"}
	checkSources(t, r, want, inner)

	// A ninth level is one too many.
	pkgs = chain(t, 10)
	_, err = compile(pkgs, 0)
	e := asError(t, err)
	if e.Msg != "nested deeper than 8" || len(e.Instances) != 10 || e.Blueprint != "test/p9@1.0.0" {
		t.Fatalf("10 packages: got %+v", e)
	}

	// Options.MaxDepth sets the limit.
	if _, err := compile(chain(t, 3), 2); err != nil {
		t.Fatalf("3 packages, MaxDepth 2: %v", err)
	}
	_, err = compile(chain(t, 4), 2)
	if e := asError(t, err); e.Msg != "nested deeper than 2" {
		t.Fatalf("4 packages, MaxDepth 2: got %+v", e)
	}

	// Nodes of a group nest the same way.
	hires, ng := loadHires(t), loadNestedGroup(t)
	og := outerGroup(t, ng)
	r, err = compile([]*Package{og, ng, hires}, 2)
	if err != nil {
		t.Fatalf("outer-group, MaxDepth 2: %v", err)
	}
	checkIDs(t, r, append([]string{"14.7.5.1", "14.7.5.2"}, hiresOuterIDs...))
	checkSources(t, r, Source{Instances: []string{"14", "7", "5"}, Blueprint: "test/hires@1.0.0"}, "14.7.5.1", "14.7.5.2")
	checkLinks(t, r, []wantLink{{"14.7.5.1", "samples", ref{"17", 0}}, {"18", "samples", ref{"14.7.5.2", 0}}})
	_, err = compile([]*Package{og, ng, hires}, 1)
	e = asError(t, err)
	if e.Msg != "nested deeper than 1" || !reflect.DeepEqual(e.Instances, []string{"14", "7", "5"}) {
		t.Fatalf("outer-group, MaxDepth 1: got %+v", e)
	}
}

// hiresEach is a node group test/hires-each that repeats a hires instance,
// node 5, once per element of denoises, each copy refining the latent of
// the copy before.
func hiresEach(t *testing.T, hires *Package) *Package {
	t.Helper()
	manifest := fmt.Sprintf(`{
  "format": 1, "id": "test/hires-each", "version": "1.0.0", "title": "Hires per denoise",
  "inputs": [
    { "name": "model", "type": "MODEL", "to": [{ "nodeId": "5", "input": "model" }] },
    { "name": "positive", "type": "CONDITIONING", "to": [{ "nodeId": "5", "input": "positive" }] },
    { "name": "negative", "type": "CONDITIONING", "to": [{ "nodeId": "5", "input": "negative" }] },
    { "name": "samples", "type": "LATENT", "to": [{ "nodeId": "5", "input": "samples" }] }
  ],
  "outputs": [{ "name": "latent", "type": "LATENT", "from": { "nodeId": "5", "output": 0 } }],
  "settings": [{ "name": "denoises", "kind": "float", "min": 0, "max": 1, "list": { "min": 0, "max": 4 },
                 "to": [{ "nodeId": "5", "input": "denoise" }] }],
  "repeats": [{ "nodes": ["5"], "over": ["denoises"],
                "carry": [{ "to": { "nodeId": "5", "input": "samples" }, "from": { "nodeId": "5", "output": 0 } }] }],
  "requires": { "classes": {} },
  "uses": { "test/hires": { "version": "1.0.0", "digest": %q } }
}`, hires.Digest)
	group := fmt.Sprintf(`{
  "5": { "class_type": "bp:test/hires@1.0.0",
         "inputs": { "model": ["in", 0], "positive": ["in", 0], "negative": ["in", 0], "samples": ["in", 0],
                     "denoise": 0.45 },
         "_meta": { "blueprint": %q } }
}`, hires.Digest)
	return mustLoad(t, fstest.MapFS{
		"blueprint.json": {Data: []byte(manifest)},
		"graph.json":     {Data: []byte(group)},
	})
}

func TestItemInherited(t *testing.T) {
	hires := loadHires(t)

	t.Run("a repeat copy", func(t *testing.T) {
		each := hiresEach(t, hires)
		if ps := each.Check(fixtureClasses(t)); len(ps) != 0 {
			t.Fatalf("problems:\n%v", ps)
		}
		r := mustCompile(t, nestedGraph(t, each, map[string]any{"denoises": []any{0.3, 0.5}}), each, hires)
		checkIDs(t, r, append(copyIDs("14", 2, "5.1", "5.2"), hiresOuterIDs...))
		for k := range 2 {
			want := Source{Instances: []string{"14", "5"}, Blueprint: "test/hires@1.0.0", Item: &Item{Setting: "denoises", Index: k}}
			checkSources(t, r, want, fmt.Sprintf("14.r%d.5.1", k), fmt.Sprintf("14.r%d.5.2", k))
		}
		checkLinks(t, r, []wantLink{
			{"14.r0.5.1", "samples", ref{"17", 0}},
			{"14.r1.5.1", "samples", ref{"14.r0.5.2", 0}}, // the carry
			{"18", "samples", ref{"14.r1.5.2", 0}},
		})
		checkValues(t, r, []wantValue{{"14.r0.5.2", "denoise", 0.3}, {"14.r1.5.2", "denoise", 0.5}})

		// With no copies the latent passes straight through.
		r = mustCompile(t, nestedGraph(t, each, map[string]any{"denoises": []any{}}), each, hires)
		checkIDs(t, r, hiresOuterIDs)
		checkLinks(t, r, []wantLink{{"18", "samples", ref{"17", 0}}})
	})

	t.Run("a program's child, whatever the nested blueprint tags", func(t *testing.T) {
		rg := loadRefGroup(t)
		denoises := Setting{Name: "denoises", Kind: KindFloat, List: &ListBounds{Max: 4}}
		p := userPkg(t, prog(
			`lat = inputs.samples`,
			`for i, d in enumerate(settings.denoises):`,
			`    c = ctx.child("p%d" % i, item = ("denoises", i))`,
			`    lat = c.use("test/hires", "h", settings = {"denoise": d}, inputs = {`,
			`        "model": inputs.model, "positive": inputs.positive, "negative": inputs.negative, "samples": lat}).latent`,
			`ctx.child("tagged", item = ("denoises", 0)).use("test/ref-group", "refs",`,
			`    inputs = {"conditioning": inputs.positive, "vae": inputs.vae}, settings = {"images": ["a.png", "b.png"]})`,
			`return {"latent": lat}`), []Setting{denoises}, hires, rg)
		r := mustCompile(t, nestedGraph(t, p, map[string]any{"denoises": []any{0.3, 0.5}}), p, hires, rg)
		for k := range 2 {
			want := Source{Instances: []string{"14", "h"}, Blueprint: "test/hires@1.0.0", Item: &Item{Setting: "denoises", Index: k}}
			checkSources(t, r, want, fmt.Sprintf("14.p%d.h.1", k), fmt.Sprintf("14.p%d.h.2", k))
			// ref-group's copies are its images, but Source.Item names a
			// setting of the instance in the graph: the child's item.
			want = Source{Instances: []string{"14", "refs"}, Blueprint: "test/ref-group@1.0.0", Item: &Item{Setting: "denoises", Index: 0}}
			checkSources(t, r, want, fmt.Sprintf("14.tagged.refs.r%d.1", k), fmt.Sprintf("14.tagged.refs.r%d.4", k))
		}
		checkLinks(t, r, []wantLink{
			{"14.p1.h.1", "samples", ref{"14.p0.h.2", 0}},
			{"18", "samples", ref{"14.p1.h.2", 0}},
			{"14.tagged.refs.r0.4", "conditioning", ref{"6", 0}},
			{"14.tagged.refs.r0.3", "vae", ref{"4", 2}},
		})
		checkValues(t, r, []wantValue{{"14.p1.h.2", "denoise", 0.5}, {"14.tagged.refs.r1.1", "image", "b.png"}})

		// Untagged at the top, a nested blueprint's copies have no item.
		p = userPkg(t, prog(`ctx.use("test/ref-group", "refs",`,
			`    inputs = {"conditioning": inputs.positive, "vae": inputs.vae}, settings = {"images": ["a.png"]})`,
			`return {"latent": inputs.samples}`), nil, rg)
		r = mustCompile(t, nestedGraph(t, p, nil), p, rg)
		checkSources(t, r, Source{Instances: []string{"14", "refs"}, Blueprint: "test/ref-group@1.0.0"}, "14.refs.r0.1", "14.refs.r0.4")
	})

	t.Run("a repeat copy holding a program that tags its own", func(t *testing.T) {
		names := Setting{Name: "names", Kind: KindString, List: &ListBounds{Max: 4}}
		scale := Setting{Name: "scale", Kind: KindFloat, Default: 1.5}
		tagger := simpleProgram(t, "test/tagger", latentPort, latentPort, []Setting{names, scale}, []string{"LatentUpscaleBy"}, prog(
			`lat = inputs.latent`,
			`for i, n in enumerate(settings.names):`,
			`    c = ctx.child("t%d" % i, item = ("names", i))`,
			`    lat = c.node("up", "LatentUpscaleBy", samples = lat, upscale_method = "nearest-exact", scale_by = settings.scale).LATENT`,
			`return {"latent": lat}`))
		each := eachTagger(t, tagger)
		if ps := each.Check(fixtureClasses(t)); len(ps) != 0 {
			t.Fatalf("problems:\n%v", ps)
		}
		r := mustCompile(t, nestedGraph(t, each, map[string]any{"scales": []any{2.0, 3.0}}), each, tagger)
		var ids []string
		for k := range 2 {
			want := Source{Instances: []string{"14", "5"}, Blueprint: "test/tagger@1.0.0", Item: &Item{Setting: "scales", Index: k}}
			for j := range 2 {
				id := fmt.Sprintf("14.r%d.5.t%d.up", k, j)
				ids = append(ids, id)
				checkSources(t, r, want, id)
			}
		}
		checkIDs(t, r, append(ids, hiresOuterIDs...))
		checkLinks(t, r, []wantLink{
			{"14.r0.5.t0.up", "samples", ref{"17", 0}},
			{"14.r0.5.t1.up", "samples", ref{"14.r0.5.t0.up", 0}},
			{"14.r1.5.t0.up", "samples", ref{"14.r0.5.t1.up", 0}}, // the carry
			{"18", "samples", ref{"14.r1.5.t1.up", 0}},
		})
		checkValues(t, r, []wantValue{{"14.r1.5.t0.up", "scale_by", 3.0}})

		// An error keeps the item of the blueprint that raised it.
		bad := simpleProgram(t, "test/tagger", latentPort, latentPort, []Setting{names, scale}, []string{"LatentUpscaleBy"}, prog(
			`ctx.child("t", item = ("names", 1)).node("up", "Foo")`,
			`return {"latent": inputs.latent}`))
		each = eachTagger(t, bad)
		e := compileError(t, nestedGraph(t, each, map[string]any{"scales": []any{2.0}}), each, bad)
		wantMsg(t, e, "class Foo is not on this server")
		if !reflect.DeepEqual(e.Item, &Item{Setting: "names", Index: 1}) || e.Blueprint != "test/tagger@1.0.0" ||
			!reflect.DeepEqual(e.Instances, []string{"14", "5"}) {
			t.Fatalf("got %+v (item %+v)", e, e.Item)
		}
	})
}

// eachTagger is a node group test/each-tagger that repeats an instance of
// tagger, node 5, once per element of scales, each copy taking the latent of
// the copy before, and holding names a and b.
func eachTagger(t *testing.T, tagger *Package) *Package {
	t.Helper()
	manifest := fmt.Sprintf(`{
  "format": 1, "id": "test/each-tagger", "version": "1.0.0", "title": "Tag each",
  "inputs": [{ "name": "latent", "type": "LATENT", "to": [{ "nodeId": "5", "input": "latent" }] }],
  "outputs": [{ "name": "latent", "type": "LATENT", "from": { "nodeId": "5", "output": 0 } }],
  "settings": [{ "name": "scales", "kind": "float", "min": 1, "max": 4, "list": { "min": 0, "max": 4 },
                 "to": [{ "nodeId": "5", "input": "scale" }] }],
  "repeats": [{ "nodes": ["5"], "over": ["scales"],
                "carry": [{ "to": { "nodeId": "5", "input": "latent" }, "from": { "nodeId": "5", "output": 0 } }] }],
  "requires": { "classes": {} },
  "uses": { "test/tagger": { "version": "1.0.0", "digest": %q } }
}`, tagger.Digest)
	group := fmt.Sprintf(`{
  "5": { "class_type": "bp:test/tagger@1.0.0",
         "inputs": { "latent": ["in", 0], "names": ["a", "b"], "scale": 1.5 },
         "_meta": { "blueprint": %q } }
}`, tagger.Digest)
	return mustLoad(t, fstest.MapFS{
		"blueprint.json": {Data: []byte(manifest)},
		"graph.json":     {Data: []byte(group)},
	})
}

func TestNodeLimit(t *testing.T) {
	rg := loadRefGroup(t)
	opt := options(t, rg)
	opt.MaxNodes = 10
	_, err := Compile(context.Background(), refGraph(t, rg, "a.png", "b.png", "c.png"), opt)
	wantMsg(t, asError(t, err), "compile makes more than 10 nodes")

	// Nested nodes count against the limit too, and so does the nested
	// instance itself: nested-group makes one instance and two nodes.
	ng, hires := loadNestedGroup(t), loadHires(t)
	opt = options(t, ng, hires)
	opt.MaxNodes = 3
	if _, err := Compile(context.Background(), nestedGraph(t, ng, nil), opt); err != nil {
		t.Fatalf("1 instance and 2 nodes, MaxNodes 3: %v", err)
	}
	opt.MaxNodes = 2
	_, err = Compile(context.Background(), nestedGraph(t, ng, nil), opt)
	e := asError(t, err)
	wantMsg(t, e, "compile makes more than 2 nodes")
	if !reflect.DeepEqual(e.Instances, []string{"14", "5"}) {
		t.Fatalf("Instances = %v", e.Instances)
	}
}

// fanOut is levels node groups test/fan1 to test/fan<levels>, top first, and
// below them a program test/pass that gives its latent straight back. Each
// group holds k instances of the level below, nodes "1" to "k", all fed the
// group's latent, and its output is node 1's. Nothing emits a node.
func fanOut(t *testing.T, levels, k int) []*Package {
	t.Helper()
	below := simpleProgram(t, "test/pass", latentPort, latentPort, nil, nil, prog(`return {"latent": inputs.latent}`))
	pkgs := []*Package{below}
	for l := levels; l >= 1; l-- {
		var targets, nodes []string
		for i := 1; i <= k; i++ {
			targets = append(targets, fmt.Sprintf(`{ "nodeId": "%d", "input": "latent" }`, i))
			nodes = append(nodes, fmt.Sprintf(`"%d": { "class_type": %q, "inputs": { "latent": ["in", 0] }, "_meta": { "blueprint": %q } }`,
				i, below.Manifest.Class(), below.Digest))
		}
		manifest := fmt.Sprintf(`{
  "format": 1, "id": "test/fan%d", "version": "1.0.0", "title": "Fan out",
  "inputs": [{ "name": "latent", "type": "LATENT", "to": [%s] }],
  "outputs": [{ "name": "latent", "type": "LATENT", "from": { "nodeId": "1", "output": 0 } }],
  "settings": [],
  "requires": { "classes": {} },
  "uses": { %q: { "version": "1.0.0", "digest": %q } }
}`, l, strings.Join(targets, ", "), below.Manifest.ID, below.Digest)
		below = mustLoad(t, fstest.MapFS{
			"blueprint.json": {Data: []byte(manifest)},
			"graph.json":     {Data: []byte("{" + strings.Join(nodes, ",\n") + "}")},
		})
		pkgs = append([]*Package{below}, pkgs...)
	}
	return pkgs
}

// TestNodeLimitCountsNestedInstances: an instance that makes no node still
// counts, so instances that fan out to nothing cannot run without bound.
func TestNodeLimitCountsNestedInstances(t *testing.T) {
	// fan1 holds 3 fan2, each holding 3 pass: 12 nested instances.
	pkgs := fanOut(t, 2, 3)
	opt := options(t, pkgs...)
	opt.MaxNodes = 12
	r, err := Compile(context.Background(), nestedGraph(t, pkgs[0], nil), opt)
	if err != nil {
		t.Fatalf("12 instances, MaxNodes 12: %v", err)
	}
	checkIDs(t, r, hiresOuterIDs)
	checkLinks(t, r, []wantLink{{"18", "samples", ref{"17", 0}}})
	if len(r.Sources) != 0 {
		t.Fatalf("Sources = %v", r.Sources)
	}
	opt.MaxNodes = 11
	_, err = Compile(context.Background(), nestedGraph(t, pkgs[0], nil), opt)
	wantMsg(t, asError(t, err), "compile makes more than 11 nodes")

	// The reviewer's shape, 10 to a level, stops at the default limit.
	pkgs = fanOut(t, 4, 10)
	_, err = Compile(context.Background(), nestedGraph(t, pkgs[0], nil), options(t, pkgs...))
	wantMsg(t, asError(t, err), "compile makes more than 2000 nodes")
}

// countTo is a program test/slow that counts to n before it gives its latent
// straight back.
func countTo(t *testing.T, n int) *Package {
	t.Helper()
	return simpleProgram(t, "test/slow", latentPort, latentPort, nil, nil, prog(
		`x = 0`, fmt.Sprintf(`for i in range(%d):`, n), `    x += 1`, `return {"latent": inputs.latent}`))
}

// slowEach is a node group test/slow-each that repeats an instance of slow,
// node 5, copies times, each copy taking the latent of the copy before.
func slowEach(t *testing.T, slow *Package) *Package {
	t.Helper()
	manifest := fmt.Sprintf(`{
  "format": 1, "id": "test/slow-each", "version": "1.0.0", "title": "Slow copies",
  "inputs": [{ "name": "latent", "type": "LATENT", "to": [{ "nodeId": "5", "input": "latent" }] }],
  "outputs": [{ "name": "latent", "type": "LATENT", "from": { "nodeId": "5", "output": 0 } }],
  "settings": [{ "name": "copies", "kind": "int", "default": 1, "min": 0, "max": 64 }],
  "repeats": [{ "nodes": ["5"], "count": "copies",
                "carry": [{ "to": { "nodeId": "5", "input": "latent" }, "from": { "nodeId": "5", "output": 0 } }] }],
  "requires": { "classes": {} },
  "uses": { "test/slow": { "version": "1.0.0", "digest": %q } }
}`, slow.Digest)
	group := fmt.Sprintf(`{
  "5": { "class_type": "bp:test/slow@1.0.0", "inputs": { "latent": ["in", 0] }, "_meta": { "blueprint": %q } }
}`, slow.Digest)
	return mustLoad(t, fstest.MapFS{
		"blueprint.json": {Data: []byte(manifest)},
		"graph.json":     {Data: []byte(group)},
	})
}

// forever is more counting than any test waits for.
const forever = 1_000_000_000_000

// TestBudget: Steps and Timeout bound one instance, and Budget the whole
// compile, so a repeat of program copies, each inside its own limits, stops
// when the compile's time is spent.
func TestBudget(t *testing.T) {
	if got := (Options{}).withDefaults().Budget; got != 10*time.Second {
		t.Fatalf("default Budget = %v", got)
	}

	// Each copy takes about 100ms, ten times that under -race; 40 of them
	// take seconds.
	slow := countTo(t, 4_500_000)
	each := slowEach(t, slow)
	opt := options(t, each, slow)
	opt.Steps, opt.Timeout, opt.Budget = 1<<62, time.Minute, 300*time.Millisecond
	start := time.Now()
	_, err := Compile(context.Background(), nestedGraph(t, each, map[string]any{"copies": 40.0}), opt)
	elapsed := time.Since(start)
	e := asError(t, err)
	wantMsg(t, e, "compile took longer than 300ms")
	if errors.Is(err, context.DeadlineExceeded) || e.Err != nil {
		t.Fatalf("a spent budget reads as ctx's error: %+v", e)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("a 300ms budget stopped the compile after %v", elapsed)
	}

	// A program that is running when the budget is spent is cancelled then,
	// not at its own timeout.
	p := countTo(t, forever)
	opt = options(t, p)
	opt.Steps, opt.Timeout, opt.Budget = 1<<62, time.Minute, 100*time.Millisecond
	start = time.Now()
	_, err = Compile(context.Background(), nestedGraph(t, p, nil), opt)
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Fatalf("a 100ms budget stopped the compile after %v", elapsed)
	}
	e = asError(t, err)
	if e.Msg != "compile took longer than 100ms" || !reflect.DeepEqual(e.Instances, []string{"14"}) || e.Blueprint != "test/slow@1.0.0" {
		t.Fatalf("got %+v", e)
	}
}

// TestCompileCancelled: a compile stops when its ctx is done, with an
// *Error that wraps ctx's error.
func TestCompileCancelled(t *testing.T) {
	// Done before compile starts: the first node is refused.
	hires := loadHires(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Compile(ctx, hiresGraph(t, hires), options(t, hires))
	e := asError(t, err)
	if !errors.Is(err, context.Canceled) || e.Msg != "compile stopped: context canceled" ||
		!reflect.DeepEqual(e.Instances, []string{"21"}) {
		t.Fatalf("got %+v", e)
	}

	// Cancelled while a program runs: its thread stops.
	p := countTo(t, forever)
	opt := options(t, p)
	opt.Steps, opt.Timeout, opt.Budget = 1<<62, time.Minute, time.Minute
	ctx, cancel = context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	start := time.Now()
	_, err = Compile(ctx, nestedGraph(t, p, nil), opt)
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Fatalf("cancelled after 50ms, the compile stopped after %v", elapsed)
	}
	if e := asError(t, err); !errors.Is(err, context.Canceled) || e.Msg != "compile stopped: context canceled" {
		t.Fatalf("got %+v", e)
	}

	// A ctx deadline before the budget's end is ctx's error, not the
	// budget's.
	ctx, cancel = context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err = Compile(ctx, nestedGraph(t, p, nil), opt)
	if e := asError(t, err); !errors.Is(err, context.DeadlineExceeded) || e.Msg != "compile stopped: context deadline exceeded" {
		t.Fatalf("got %+v", e)
	}
}

// concurrentGraph is refGraph with a ref-group instance "14" feeding a
// ref-program instance "20" before the guidance, and a nested-group "21"
// then a nested-program "22" between the sampler and the decode.
func concurrentGraph(t *testing.T, rg, rp, ng, np *Package) graph.Prompt {
	t.Helper()
	g := refGraph(t, rg, "a.png", "b.png")
	g["20"] = refInstance(rp, lk("14", 0), []any{"c.png"})
	g["20"].Class = rp.Manifest.Class()
	g["15"].Inputs["conditioning"] = lk("20", 0)
	latent := func(p *Package, samples []any) *graph.Node {
		return &graph.Node{
			Class: p.Manifest.Class(),
			Inputs: map[string]any{
				"model": lk("4", 0), "positive": lk("15", 0), "negative": lk("15", 0), "samples": samples,
				"denoise": 0.4, "seed": 3.0,
			},
			Meta: &graph.Meta{Blueprint: p.Digest},
		}
	}
	g["21"] = latent(ng, lk("17", 0))
	g["22"] = latent(np, lk("21", 0))
	g["18"].Inputs["samples"] = lk("22", 0)
	return g
}

func TestCompileConcurrent(t *testing.T) {
	rg, rp, ng, np, hires := loadRefGroup(t), loadRefProgram(t), loadNestedGroup(t), loadNestedProgram(t), loadHires(t)
	g := concurrentGraph(t, rg, rp, ng, np)
	opt := options(t, rg, rp, ng, np, hires)
	first := mustCompile(t, g, rg, rp, ng, np, hires)
	checkLinks(t, first, []wantLink{
		{"20.ref0.ref", "conditioning", ref{"14.r1.4", 0}},
		{"21.5.1", "samples", ref{"17", 0}},
		{"22.base.1", "samples", ref{"21.5.2", 0}},
		{"18", "samples", ref{"22.base.2", 0}},
	})
	want, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	got := make([][]byte, 8)
	errs := make([]error, len(got))
	for i := range got {
		wg.Go(func() {
			r, err := Compile(context.Background(), g, opt)
			if err == nil {
				got[i], err = json.Marshal(r)
			}
			errs[i] = err
		})
	}
	wg.Wait()
	for i := range got {
		if errs[i] != nil {
			t.Fatalf("compile %d: %v", i, errs[i])
		}
		if !bytes.Equal(got[i], want) {
			t.Fatalf("compile %d differs:\n%s\n%s", i, got[i], want)
		}
	}
}

// TestFixtureUsesDigests keeps the nested fixtures pinned to hires as it is.
// When hires changes, paste the digest this prints into each fixture's uses,
// and into nested-group's node 5 _meta.blueprint.
func TestFixtureUsesDigests(t *testing.T) {
	want := loadHires(t).Digest
	for _, dir := range []string{nestedProgramDir, nestedGroupDir} {
		p := mustLoad(t, os.DirFS(dir))
		if got := p.Manifest.Uses["test/hires"].Digest; got != want {
			t.Errorf("%s pins test/hires at %s; hires's digest is now %s", dir, got, want)
		}
		if n := p.Group["5"]; n != nil && (n.Meta == nil || n.Meta.Blueprint != want) {
			t.Errorf("%s node 5 has _meta %+v; hires's digest is now %s", dir, n.Meta, want)
		}
	}
}
