package blueprint

import (
	"maps"
	"os"
	"slices"
	"testing"

	"github.com/erstaples/comfyform/schema"
)

// fixtureSchema is the classes captured from comfy-2's /object_info.
func fixtureSchema(t *testing.T) *schema.Schema {
	t.Helper()
	b, err := os.ReadFile("testdata/object_info.json")
	if err != nil {
		t.Fatal(err)
	}
	s, err := schema.Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func fixtureClasses(t *testing.T) Classes { return SchemaClasses(fixtureSchema(t)) }

func TestSchemaClasses(t *testing.T) {
	c := fixtureClasses(t)
	ks, ok := c.Class("KSampler")
	if !ok {
		t.Fatal("no KSampler")
	}
	byName := map[string]InputInfo{}
	for _, in := range ks.Inputs {
		byName[in.Name] = in
	}
	if in := byName["model"]; in.Type != "MODEL" || in.Widget || !in.Required {
		t.Errorf("model = %+v, want a required MODEL socket", in)
	}
	if in := byName["seed"]; in.Type != "INT" || !in.Widget {
		t.Errorf("seed = %+v, want an INT widget", in)
	}
	if in := byName["denoise"]; !in.HasDefault || in.Default != 1.0 {
		t.Errorf("denoise = %+v, want default 1", in)
	}
	if want := []OutputInfo{{Name: "LATENT", Type: "LATENT"}}; !slices.Equal(ks.Outputs, want) {
		t.Errorf("outputs = %+v, want %+v", ks.Outputs, want)
	}

	ckpt, _ := c.Class("CheckpointLoaderSimple")
	if want := []OutputInfo{{"MODEL", "MODEL"}, {"CLIP", "CLIP"}, {"VAE", "VAE"}}; !slices.Equal(ckpt.Outputs, want) {
		t.Errorf("CheckpointLoaderSimple outputs = %+v, want %+v", ckpt.Outputs, want)
	}

	if _, ok := c.Class("NoSuchNode"); ok {
		t.Error("NoSuchNode found")
	}
	if _, ok := SchemaClasses(nil).Class("KSampler"); ok {
		t.Error("a nil schema has KSampler")
	}
}

// TestFixtureClassHashes keeps the fixtures' requires.classes in step with the
// captured classes: each hash is schema.Class.Hash on testdata/object_info.json.
func TestFixtureClassHashes(t *testing.T) {
	s := fixtureSchema(t)
	for _, dir := range []string{hiresDir, refGroupDir} {
		m := mustLoad(t, os.DirFS(dir)).Manifest
		for _, class := range slices.Sorted(maps.Keys(m.Requires.Classes)) {
			c := s.Classes[class]
			if c == nil {
				t.Errorf("%s: %s is not in testdata/object_info.json", dir, class)
				continue
			}
			if got, want := m.Requires.Classes[class], c.Hash(); got != want {
				t.Errorf("%s: %s hash %s, want %s", dir, class, got, want)
			}
		}
	}
}
