package engine

import (
	"context"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/erstaples/comfyform/codegen"
	"github.com/erstaples/comfyform/graph"
	"github.com/erstaples/comfyform/internal/fakecomfy"
	"github.com/erstaples/comfyform/schema"
)

func loadSchema(t *testing.T) *schema.Schema {
	t.Helper()
	b, err := os.ReadFile("../testdata/object_info.json")
	if err != nil {
		t.Fatal(err)
	}
	s, err := schema.Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func loadWorkflow(t *testing.T) graph.Prompt {
	t.Helper()
	b, err := os.ReadFile("../testdata/workflow_api.json")
	if err != nil {
		t.Fatal(err)
	}
	p, err := ParseWorkflow(b)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestSchemaParse(t *testing.T) {
	s := loadSchema(t)
	ks := s.Classes["KSampler"]
	var order []string
	for _, in := range ks.Inputs {
		order = append(order, in.Name)
	}
	if got := strings.Join(order, ","); got != "model,seed,steps,cfg,sampler_name,scheduler,positive,negative,latent_image,denoise" {
		t.Errorf("input order = %s", got)
	}
	if in := ks.Input("sampler_name"); in.IsFileList() || in.Default != "euler" {
		t.Errorf("sampler_name: filelist=%v default=%v", in.IsFileList(), in.Default)
	}
	if in := s.Classes["CheckpointLoaderSimple"].Input("ckpt_name"); !in.IsFileList() || in.HasDefault {
		t.Errorf("ckpt_name should be a file list without an implied default")
	}
	if in := s.Classes["LatentUpscaleBy"].Input("upscale_method"); in.Kind != schema.KindCombo || len(in.Options) != 5 {
		t.Errorf("new-style COMBO not parsed: %+v", in)
	}
	// no input_order: falls back to document order
	sw := s.Classes["Any Switch (rgthree)"]
	if len(sw.Inputs) != 2 || sw.Inputs[0].Name != "any_01" || sw.Inputs[0].Kind != schema.KindLink {
		t.Errorf("any switch inputs: %+v", sw.Inputs)
	}
}

func TestCodegenNames(t *testing.T) {
	s := loadSchema(t)
	nm := codegen.Resolve(s)
	if got := nm["Image Film Grain (WAS)"].Func; got != "ImageFilmGrainWAS" {
		t.Errorf("func name = %s", got)
	}
	if got := nm["Image Film Grain (WAS)"].Outputs; strings.Join(got, ",") != "Image,Image2" {
		t.Errorf("duplicate outputs = %v", got)
	}
	if _, err := codegen.Generate(s, "nodes"); err != nil {
		t.Fatal(err)
	}
}

func TestHashesIgnoreIDs(t *testing.T) {
	a := graph.Prompt{
		"1": {Class: "A", Inputs: map[string]any{"x": 1.0}},
		"2": {Class: "B", Inputs: map[string]any{"in": []any{"1", 0.0}}},
	}
	b := graph.Prompt{
		"src": {Class: "A", Inputs: map[string]any{"x": 1.0}},
		"dst": {Class: "B", Inputs: map[string]any{"in": []any{"src", 0.0}}},
	}
	ha, _ := a.Hashes()
	hb, _ := b.Hashes()
	if ha["2"] != hb["dst"] {
		t.Error("structurally identical graphs should hash the same")
	}
	b["src"].Inputs["x"] = 2.0
	hb, _ = b.Hashes()
	if ha["2"] == hb["dst"] {
		t.Error("upstream change must change downstream hash")
	}
}

func TestPlanKinds(t *testing.T) {
	s := loadSchema(t)
	old := loadWorkflow(t)
	h, _ := old.Hashes()
	prev := &State{Prompt: old, Hashes: h}

	next := loadWorkflow(t)
	next["3"].Inputs["steps"] = 30.0 // KSampler change
	delete(next, "12")               // drop the seed node...
	next["3"].Inputs["seed"] = 7.0   // ...and inline the seed

	pl, err := MakePlan(prev, next, s, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]ChangeKind{}
	for _, c := range pl.Changes {
		kinds[c.ID] = c.Kind
	}
	want := map[string]ChangeKind{"3": Update, "8": Rerun, "9": Rerun, "4": Unchanged, "10": Unchanged, "12": Remove}
	for id, k := range want {
		if kinds[id] != k {
			t.Errorf("node %s: kind %d, want %d", id, kinds[id], k)
		}
	}
	for _, c := range pl.Changes {
		if c.ID == "3" && len(c.Fields) != 2 {
			t.Errorf("expected seed+steps diffs, got %+v", c.Fields)
		}
	}
}

func TestValidate(t *testing.T) {
	s := loadSchema(t)
	p := loadWorkflow(t)
	delete(p, "12")
	p["3"].Inputs["seed"] = 1.0
	if probs := Validate(p, s); len(probs) != 0 {
		t.Fatalf("valid workflow reported problems: %+v", probs)
	}
	p["3"].Inputs["sampler_name"] = "nope"
	p["3"].Inputs["steps"] = 0.0
	p["3"].Inputs["positive"] = []any{"4", 1.0} // CLIP into CONDITIONING
	p["4"].Inputs["ckpt_name"] = "missing.safetensors"
	delete(p["8"].Inputs, "vae")
	msgs := ""
	for _, pr := range Validate(p, s) {
		msgs += pr.Msg + "\n"
	}
	for _, want := range []string{"not one of", "below the minimum", "expects CONDITIONING", "not found on the server", `required input "vae"`} {
		if !strings.Contains(msgs, want) {
			t.Errorf("missing %q in:\n%s", want, msgs)
		}
	}
}

func TestImportUIFormatRejected(t *testing.T) {
	_, err := ParseWorkflow([]byte(`{"nodes": [], "links": []}`))
	if err == nil || !strings.Contains(err.Error(), "Export (API)") {
		t.Errorf("want helpful error, got %v", err)
	}
}

func TestImportGeneratesValidGo(t *testing.T) {
	src, err := Import(loadWorkflow(t), loadSchema(t), ImportOptions{NodesImport: "example.com/x/nodes"})
	if err != nil {
		t.Fatalf("%v\n%s", err, src)
	}
	for _, want := range []string{`"strength_clip": 0`, `graph.Link(seedrgthree12, 0)`, `nodes.CLIPTextEncode(ctx, "positive"`} {
		if !strings.Contains(string(src), want) {
			t.Errorf("imported code missing %q", want)
		}
	}
}

func TestClientAgainstFake(t *testing.T) {
	b, _ := os.ReadFile("../testdata/object_info.json")
	fs, err := fakecomfy.New(b)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(fs)
	defer srv.Close()
	c := NewClient(srv.URL)
	ctx := context.Background()

	p := loadWorkflow(t)
	if _, err := c.Queue(ctx, p); err == nil || !strings.Contains(err.Error(), "Seed (rgthree)") {
		t.Errorf("expected node error for uninstalled class, got %v", err)
	}
	delete(p, "12")
	p["3"].Inputs["seed"] = 5.0
	id, err := c.Queue(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	res, err := c.Wait(ctx, id, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Outputs["9"]) != 1 {
		t.Fatalf("outputs: %+v", res.Outputs)
	}
	img, err := c.Download(ctx, res.Outputs["9"][0])
	if err != nil || len(img) < 8 || string(img[1:4]) != "PNG" {
		t.Errorf("download: %v (%d bytes)", err, len(img))
	}
}
