package blueprint

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"go.starlark.net/starlark"

	"github.com/erstaples/comfyform/graph"
)

const refProgramDir = "testdata/packages/ref-program"

func loadRefProgram(t *testing.T) *Package { return mustLoad(t, os.DirFS(refProgramDir)) }

// refProgramGraph is refGraph with its instance "14" an instance of
// ref-program.
func refProgramGraph(t *testing.T, rp *Package, images ...string) graph.Prompt {
	t.Helper()
	g := refGraph(t, rp, images...)
	g["14"].Class = rp.Manifest.Class()
	return g
}

// programPkg is a program package test/prog with main.star star, input
// ports conditioning, vae and an optional image, an output conditioning,
// ref-group's four classes in requires, and the given settings.
func programPkg(t *testing.T, star string, settings ...Setting) *Package {
	t.Helper()
	s := fixtureSchema(t)
	classes := map[string]string{}
	for _, c := range []string{"LoadImage", "ImageScaleToTotalPixels", "VAEEncode", "ReferenceLatent"} {
		classes[c] = s.Classes[c].Hash()
	}
	m := Manifest{
		Format: Format, ID: "test/prog", Version: "1.0.0", Title: "Program",
		Inputs: []Port{
			{Name: "conditioning", Type: "CONDITIONING"},
			{Name: "vae", Type: "VAE"},
			{Name: "image", Type: "IMAGE", Optional: true},
		},
		Outputs:  []Port{{Name: "conditioning", Type: "CONDITIONING"}},
		Settings: append([]Setting{}, settings...),
		Requires: Requires{Classes: classes},
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

// prog is a main.star whose build runs the given lines.
func prog(lines ...string) string {
	var b strings.Builder
	b.WriteString("def build(ctx, inputs, settings):\n")
	for _, l := range lines {
		b.WriteString("    " + l + "\n")
	}
	return b.String()
}

// passThrough ends a build that gives its conditioning straight back.
const passThrough = `return {"conditioning": inputs.conditioning}`

// programGraph has an instance "14" of p with every port linked, holding
// the given settings, and a FluxGuidance "15" after it.
func programGraph(p *Package, settings map[string]any) graph.Prompt {
	inputs := map[string]any{"conditioning": lk("6", 0), "vae": lk("3", 0), "image": lk("2", 0)}
	for k, v := range settings {
		inputs[k] = v
	}
	return graph.Prompt{
		"2":  {Class: "LoadImage", Inputs: map[string]any{"image": "a.png"}},
		"3":  {Class: "VAELoader", Inputs: map[string]any{"vae_name": "ae.safetensors"}},
		"4":  {Class: "CheckpointLoaderSimple", Inputs: map[string]any{"ckpt_name": "model.safetensors"}},
		"6":  {Class: "CLIPTextEncode", Inputs: map[string]any{"clip": lk("4", 1), "text": "a harbor at dusk"}},
		"14": {Class: p.Manifest.Class(), Inputs: inputs, Meta: &graph.Meta{Blueprint: p.Digest}},
		"15": {Class: "FluxGuidance", Inputs: map[string]any{"conditioning": lk("14", 0), "guidance": 2.5}},
	}
}

// programError compiles programGraph(p, nil) and returns its *Error.
func programError(t *testing.T, p *Package) *Error {
	t.Helper()
	return compileError(t, programGraph(p, nil), p)
}

func asError(t *testing.T, err error) *Error {
	t.Helper()
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("error %T is not an *Error: %v", err, err)
	}
	return e
}

func TestProgramMatchesGroup(t *testing.T) {
	rg, rp := loadRefGroup(t), loadRefProgram(t)
	images := []string{"a.png", "b.png", "c.png"}
	gr := mustCompile(t, refGraph(t, rg, images...), rg)
	pr := mustCompile(t, refProgramGraph(t, rp, images...), rp)

	gh, err := gr.Prompt.Hashes()
	if err != nil {
		t.Fatal(err)
	}
	ph, err := pr.Prompt.Hashes()
	if err != nil {
		t.Fatal(err)
	}
	if gh["19"] != ph["19"] {
		t.Fatalf("SaveImage hashes differ: group %s, program %s", gh["19"], ph["19"])
	}

	var ids []string
	for _, c := range []string{"ref0", "ref1", "ref2"} {
		for _, n := range []string{"load", "scale", "encode", "ref"} {
			ids = append(ids, "14."+c+"."+n)
		}
	}
	checkIDs(t, pr, append(ids, refOuterIDs...))
	checkLinks(t, pr, []wantLink{
		{"14.ref0.ref", "conditioning", ref{"6", 0}},
		{"14.ref1.ref", "conditioning", ref{"14.ref0.ref", 0}},
		{"14.ref1.encode", "vae", ref{"3", 0}},
		{"15", "conditioning", ref{"14.ref2.ref", 0}},
	})
	want := Source{Instances: []string{"14"}, Blueprint: "test/ref-program@1.0.0", Item: &Item{Setting: "images", Index: 1}}
	if got := pr.Sources["14.ref1.encode"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("source of 14.ref1.encode = %+v, want %+v", got, want)
	}

	// With no images the program's output is its input's link.
	for _, c := range []struct {
		name string
		p    *Package
		g    graph.Prompt
	}{
		{"group", rg, refGraph(t, rg)},
		{"program", rp, refProgramGraph(t, rp)},
	} {
		r := mustCompile(t, c.g, c.p)
		if got := link(t, r, "15", "conditioning"); got != (ref{"6", 0}) {
			t.Errorf("%s: 15 conditioning links %v, want 6:0", c.name, got)
		}
		checkIDs(t, r, refOuterIDs)
	}
}

func TestProgramDefaults(t *testing.T) {
	p := programPkg(t, prog(`ctx.node("s", "ImageScaleToTotalPixels", image = inputs.image)`, passThrough))
	r := mustCompile(t, programGraph(p, nil), p)
	for input, want := range map[string]any{"megapixels": 1.0, "resolution_steps": 1.0, "upscale_method": "nearest-exact"} {
		if got := value(t, r, "14.s", input); got != want {
			t.Errorf("14.s %s = %#v, want %#v", input, got, want)
		}
	}
	if got := link(t, r, "14.s", "image"); got != (ref{"2", 0}) {
		t.Fatalf("14.s image links %v", got)
	}
}

func TestProgramNoneIsDefault(t *testing.T) {
	p := programPkg(t, prog(`ctx.node("s", "ImageScaleToTotalPixels", image = inputs.image, megapixels = None)`, passThrough))
	if got := value(t, mustCompile(t, programGraph(p, nil), p), "14.s", "megapixels"); got != 1.0 {
		t.Fatalf("megapixels = %#v, want the default 1", got)
	}
}

func TestProgramOptionalInputIsNone(t *testing.T) {
	p := programPkg(t, prog(
		`if inputs.image != None:`,
		`    fail("image is linked")`,
		`ctx.node("s", "ImageScaleToTotalPixels", image = inputs.image)`,
		passThrough))
	g := programGraph(p, nil)
	delete(g["14"].Inputs, "image")
	r := mustCompile(t, g, p)
	if v, ok := r.Prompt["14.s"].Inputs["image"]; ok {
		t.Fatalf("14.s image = %v, want it left out", v)
	}
}

func TestProgramUnknownClass(t *testing.T) {
	e := programError(t, programPkg(t, prog(`ctx.node("a", "Foo")`, passThrough)))
	if want := "main.star:2: class Foo is not on this server"; e.Msg != want {
		t.Fatalf("Msg = %q, want %q", e.Msg, want)
	}
	if !reflect.DeepEqual(e.Instances, []string{"14"}) || e.Blueprint != "test/prog@1.0.0" {
		t.Fatalf("got %+v", e)
	}
}

func TestProgramClassNotRequired(t *testing.T) {
	p := programPkg(t, prog(`ctx.node("d", "VAEDecode", samples = inputs.image, vae = inputs.vae)`, passThrough))
	wantMsg(t, programError(t, p), "VAEDecode is not in requires.classes")
}

func TestProgramDuplicateName(t *testing.T) {
	p := programPkg(t, prog(
		`ctx.node("a", "VAEEncode", pixels = inputs.image, vae = inputs.vae)`,
		`ctx.node("a", "VAEEncode", pixels = inputs.image, vae = inputs.vae)`,
		passThrough))
	wantMsg(t, programError(t, p), "duplicate node id 14.a")
}

func TestProgramBadName(t *testing.T) {
	p := programPkg(t, prog(`ctx.node("a.b", "VAEEncode", pixels = inputs.image, vae = inputs.vae)`, passThrough))
	wantMsg(t, programError(t, p), "invalid node name")
}

// loop runs 100 million steps of Starlark.
var loop = prog(`x = 0`, `for i in range(100000000):`, `    x += 1`, passThrough)

func TestProgramStepLimit(t *testing.T) {
	p := programPkg(t, loop)
	opt := options(t, p)
	// so the default step limit comes first, even under -race
	opt.Timeout, opt.Budget = time.Minute, time.Minute
	_, err := Compile(context.Background(), programGraph(p, nil), opt)
	wantMsg(t, asError(t, err), "too many steps")
}

// TestProgramTimeout: an instance's own timer stops it, with a compile
// budget well beyond it.
func TestProgramTimeout(t *testing.T) {
	p := programPkg(t, loop)
	opt := options(t, p)
	opt.Steps, opt.Timeout, opt.Budget = 1<<62, 50*time.Millisecond, time.Minute
	start := time.Now()
	_, err := Compile(context.Background(), programGraph(p, nil), opt)
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Fatalf("a 50ms timeout took %v", elapsed)
	}
	e := asError(t, err)
	if !strings.HasPrefix(e.Msg, "main.star:") || !strings.HasSuffix(e.Msg, ": Starlark computation cancelled: timeout") || e.Err != nil {
		t.Fatalf("got %+v", e)
	}
}

func TestProgramNoWhile(t *testing.T) {
	e := programError(t, programPkg(t, prog(`while True:`, `    pass`, passThrough)))
	wantMsg(t, e, "main.star:2: ")
}

func TestProgramNoRecursion(t *testing.T) {
	p := programPkg(t, "def f(n):\n    return f(n - 1) if n > 0 else 0\n\n"+prog(`f(3)`, passThrough))
	wantMsg(t, programError(t, p), "recursive")
}

func TestProgramNoLoad(t *testing.T) {
	p := programPkg(t, `load("x.star", "y")`+"\n"+prog(passThrough))
	wantMsg(t, programError(t, p), "main.star:1: ")
}

func TestProgramSandbox(t *testing.T) {
	// struct and json are there; nothing that reaches the outside is.
	p := programPkg(t, prog(
		`s = struct(a = json.encode({"b": [1, 2]}))`,
		`if s.a != '{"b":[1,2]}':`,
		`    fail("json gave " + s.a)`,
		`print("discarded")`,
		passThrough))
	mustCompile(t, programGraph(p, nil), p)

	for _, name := range []string{"time", "random", "open", "load_file"} {
		e := programError(t, programPkg(t, prog(name+"()", passThrough)))
		wantMsg(t, e, "undefined: "+name)
	}
	wantMsg(t, programError(t, programPkg(t, prog("set()", passThrough))), "does not support sets")
}

func TestProgramOutputs(t *testing.T) {
	cases := []struct {
		ret, want string
	}{
		{`return {}`, "build returned no output conditioning"},
		{`return {"conditioning": inputs.conditioning, "x": inputs.conditioning}`, "build returned unknown output x"},
		{`return {"conditioning": 1}`, "build returned int for output conditioning"},
		{`return [inputs.conditioning]`, "build returned list, not a dict"},
	}
	for _, c := range cases {
		t.Run(c.ret, func(t *testing.T) {
			wantMsg(t, programError(t, programPkg(t, prog(c.ret))), c.want)
		})
	}

	p := programPkg(t, "def f(ctx, inputs, settings):\n    pass\n")
	wantMsg(t, programError(t, p), "main.star defines no function build")
}

func TestProgramOutputType(t *testing.T) {
	p := programPkg(t, prog(
		`lat = ctx.node("e", "VAEEncode", pixels = inputs.image, vae = inputs.vae).LATENT`,
		`return {"conditioning": lat}`))
	e := programError(t, p)
	if want := "build returned LATENT for output conditioning, which is CONDITIONING"; e.Msg != want {
		t.Fatalf("Msg = %q, want %q", e.Msg, want)
	}
}

func TestLinkedSettingMisuse(t *testing.T) {
	seed := Setting{Name: "seed", Kind: KindSeed, Default: 0.0}
	linked := func(p *Package) graph.Prompt {
		g := programGraph(p, map[string]any{"seed": lk("30", 0)})
		g["30"] = &graph.Node{Class: "SeedNode", Inputs: map[string]any{}}
		return g
	}

	// Passed into ctx.node, a linked setting becomes a link.
	p := programPkg(t, prog(
		`ctx.node("s", "ImageScaleToTotalPixels", image = inputs.image, resolution_steps = settings.seed)`,
		passThrough), seed)
	if got := link(t, mustCompile(t, linked(p), p), "14.s", "resolution_steps"); got != (ref{"30", 0}) {
		t.Fatalf("resolution_steps links %v, want 30:0", got)
	}

	p = programPkg(t, prog(`s = settings.seed + 1`, passThrough), seed)
	e := compileError(t, linked(p), p)
	wantMsg(t, e, "setting seed is linked; this blueprint needs a value")
	if e.Setting != "seed" {
		t.Fatalf("Setting = %q", e.Setting)
	}
	if got, want := e.Error(), "14 (test/prog@1.0.0): main.star:2: setting seed is linked; this blueprint needs a value"; got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}

	// A seed with a value reads as an int.
	p = programPkg(t, prog(`if settings.seed + 1 != 8:`, `    fail("seed is", settings.seed)`, passThrough), seed)
	mustCompile(t, programGraph(p, map[string]any{"seed": 7.0}), p)
}

func TestSocketMisuse(t *testing.T) {
	for _, expr := range []string{
		`inputs.vae + 1`,
		`-inputs.vae`,
		`inputs.vae < inputs.vae`,
		`{inputs.vae: 1}`,
	} {
		t.Run(expr, func(t *testing.T) {
			e := programError(t, programPkg(t, prog(`x = `+expr, passThrough)))
			wantMsg(t, e, "a socket reference can only be passed to ctx.node")
		})
	}
}

func TestProgramFail(t *testing.T) {
	wantMsg(t, programError(t, programPkg(t, prog(`fail("no images")`))), "no images")
}

func TestProgramDeterministic(t *testing.T) {
	rp := loadRefProgram(t)
	g := refProgramGraph(t, rp, "a.png", "b.png", "c.png")
	a, err := json.Marshal(mustCompile(t, g, rp))
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(mustCompile(t, g, rp))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Fatalf("two compiles differ:\n%s\n%s", a, b)
	}
}

func TestProgramConcurrent(t *testing.T) {
	rp := loadRefProgram(t)
	g := refProgramGraph(t, rp, "a.png", "b.png")
	opt := options(t, rp)
	want, err := json.Marshal(mustCompile(t, g, rp))
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

func TestProgramChild(t *testing.T) {
	images := Setting{Name: "images", Kind: KindImage, List: &ListBounds{Max: 4}}
	p := programPkg(t, prog(
		`c = ctx.child("a", item = ("images", 1)).child("b")`,
		`c.node("load", "LoadImage", image = settings.images[1])`,
		`ctx.child("c").node("load", "LoadImage", image = "x.png")`,
		passThrough), images)
	r := mustCompile(t, programGraph(p, map[string]any{"images": []any{"a.png", "b.png"}}), p)
	if got := value(t, r, "14.a.b.load", "image"); got != "b.png" {
		t.Fatalf("image = %v", got)
	}
	if got := r.Sources["14.a.b.load"].Item; !reflect.DeepEqual(got, &Item{Setting: "images", Index: 1}) {
		t.Fatalf("item of 14.a.b.load = %+v", got)
	}
	if got := r.Sources["14.c.load"].Item; got != nil {
		t.Fatalf("item of 14.c.load = %+v, want none", got)
	}
}

func TestProgramHostErrors(t *testing.T) {
	images := Setting{Name: "images", Kind: KindImage, List: &ListBounds{Max: 4}}
	encode := `n = ctx.node("e", "VAEEncode", pixels = inputs.image, vae = inputs.vae)`
	cases := []struct {
		name, line, want string
	}{
		{"child name", `ctx.child("a.b")`, "invalid child name"},
		{"item not a pair", `ctx.child("a", item = "images")`, "item must be a (setting, index) pair"},
		{"item not a list setting", `ctx.child("a", item = ("megapixels", 0))`, "item names megapixels, which is not a list setting"},
		{"item out of range", `ctx.child("a", item = ("images", 2))`, "item 2 is out of range"},
		{"output by name", encode + `.IMAGE`, "VAEEncode has no output IMAGE"},
		{"output by index", encode + `.out(1)`, "VAEEncode has 1 output"},
		{"dict input", `ctx.node("s", "LoadImage", image = {})`, "input image: "},
		{"node as input", encode + "; ctx.node(\"r\", \"ReferenceLatent\", conditioning = inputs.conditioning, latent = n)", "input latent: "},
		{"link-shaped list", `ctx.node("s", "LoadImage", image = ["6", 0])`, "reads as a link"},
		{"ref in a list", `ctx.node("s", "LoadImage", image = [inputs.vae])`, "input image: "},
		{"not finite", `ctx.node("s", "ImageScaleToTotalPixels", megapixels = float("nan"))`, "input megapixels: "},
		{"too large", `ctx.node("s", "ImageScaleToTotalPixels", resolution_steps = 1 << 70)`, "input resolution_steps: "},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := programPkg(t, prog(c.line, passThrough), images,
				Setting{Name: "megapixels", Kind: KindFloat, Default: 1.0})
			e := compileError(t, programGraph(p, map[string]any{"images": []any{"a.png", "b.png"}}), p)
			wantMsg(t, e, c.want)
			wantMsg(t, e, "main.star:2: ")
		})
	}
}

func TestProgramInputValues(t *testing.T) {
	p := programPkg(t, prog(
		`ctx.node("s", "ImageScaleToTotalPixels", image = inputs.image, megapixels = 2,`,
		`         upscale_method = "area", extra = [1, ("a", True), 2.5])`,
		passThrough))
	r := mustCompile(t, programGraph(p, nil), p)
	if got := value(t, r, "14.s", "megapixels"); got != 2.0 {
		t.Errorf("megapixels = %#v", got)
	}
	if got := value(t, r, "14.s", "upscale_method"); got != "area" {
		t.Errorf("upscale_method = %#v", got)
	}
	if got, want := value(t, r, "14.s", "extra"), []any{1.0, []any{"a", true}, 2.5}; !reflect.DeepEqual(got, want) {
		t.Errorf("extra = %#v, want %#v", got, want)
	}
}

func TestProgramNeedsClasses(t *testing.T) {
	p := programPkg(t, prog(passThrough))
	_, err := Compile(context.Background(), programGraph(p, nil), Options{Packages: resolverOf(p)})
	wantMsg(t, asError(t, err), "Options.Classes")
}

func TestNodeOutputs(t *testing.T) {
	// No fixture class has two outputs of one name, so this takes a node
	// value as ctx.node makes it.
	f := &frame{prefix: "14", chain: []string{"14"}, blueprint: "test/prog@1.0.0"}
	n := &nodeValue{f: f, id: "14.t", class: "Twin", outputs: []OutputInfo{{"IMAGE", "IMAGE"}, {"IMAGE", "IMAGE"}, {"MASK", "MASK"}}}
	_, err := n.Attr("IMAGE")
	wantMsg(t, asError(t, err), "two outputs named IMAGE; use out(i)")

	v, err := n.Attr("MASK")
	if s, ok := v.(*socket); err != nil || !ok || *s.r != (ref{"14.t", 2}) || s.typ != "MASK" {
		t.Fatalf("MASK = %v, %v", v, err)
	}
	out, err := n.Attr("out")
	if err != nil {
		t.Fatal(err)
	}
	v, err = starlark.Call(&starlark.Thread{}, out, starlark.Tuple{starlark.MakeInt(1)}, nil)
	if s, ok := v.(*socket); err != nil || !ok || *s.r != (ref{"14.t", 1}) || s.typ != "IMAGE" {
		t.Fatalf("out(1) = %v, %v", v, err)
	}
}

func TestProgramJSONDepth(t *testing.T) {
	// starlark-go's json.decode recurses once per level with no limit, so
	// deep enough input would overflow the Go stack and kill the process.
	for _, expr := range []string{
		`"[" * 100000`,
		`'{"a":' * 100000`,
		`'"\\\\"' + "[" * 5000`, // an escaped backslash ends the string; the brackets count
	} {
		t.Run(expr, func(t *testing.T) {
			e := programError(t, programPkg(t, prog(`x = json.decode(`+expr+`)`, passThrough)))
			wantMsg(t, e, "nested deeper than 1000")
		})
	}

	p := programPkg(t, prog(
		`deep = json.decode("[" * 1000 + "]" * 1000)`,
		`for i in range(999):`,
		`    deep = deep[0]`,
		`if deep != []:`,
		`    fail("depth 1000 decoded as", deep)`,
		`s = json.decode('"' + "[" * 5000 + '"')`,
		`if s != "[" * 5000:`,
		`    fail("a string of brackets decoded as", s)`,
		`q = json.decode('"\\"' + "[" * 5000 + '"')`,
		`if q != '"' + "[" * 5000:`,
		`    fail("an escaped quote decoded as", q)`,
		`if json.encode({"a": [1, 2]}) != '{"a":[1,2]}':`,
		`    fail("encode gave", json.encode({"a": [1, 2]}))`,
		`if json.indent('{"a":1}') != '{\n\t"a": 1\n}':`,
		`    fail("indent gave", json.indent('{"a":1}'))`,
		`if json.encode_indent({"a": 1}) != '{\n\t"a": 1\n}':`,
		`    fail("encode_indent gave", json.encode_indent({"a": 1}))`,
		passThrough))
	mustCompile(t, programGraph(p, nil), p)
}
