package blueprint

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"testing"
)

const refGroupManifest = "testdata/packages/ref-group/blueprint.json"

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func mustParse(t *testing.T, b []byte) *Manifest {
	t.Helper()
	m, err := ParseManifest(b)
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	return m
}

func ptr[T any](v T) *T { return &v }

func TestParseManifestFixture(t *testing.T) {
	m := mustParse(t, readFile(t, refGroupManifest))
	if m.Class() != "bp:test/ref-group@1.0.0" {
		t.Fatal(m.Class())
	}
	if l := m.Settings[0].List; l == nil || l.Max != 8 {
		t.Fatalf("images list = %+v", l)
	}
	if len(m.Repeats) != 1 || m.Repeats[0].Over[0] != "images" {
		t.Fatalf("repeats = %+v", m.Repeats)
	}
	if got := m.Inputs[1].To[0]; got != (Target{NodeID: "3", Input: "vae"}) {
		t.Fatalf("vae target = %+v", got)
	}
	if got := m.Outputs[0].From; got == nil || *got != (Socket{NodeID: "4", Output: 0}) {
		t.Fatalf("conditioning source = %+v", got)
	}
}

func TestParseClass(t *testing.T) {
	good := []struct{ class, id, version string }{
		{"bp:a-b/c@1.2.3", "a-b/c", "1.2.3"},
		{"bp:test/ref-group@1.0.0", "test/ref-group", "1.0.0"},
		{"bp:0/9@10.20.30", "0/9", "10.20.30"},
	}
	for _, c := range good {
		id, version, ok := ParseClass(c.class)
		if !ok || id != c.id || version != c.version {
			t.Errorf("ParseClass(%q) = %q, %q, %v", c.class, id, version, ok)
		}
	}
	for _, class := range []string{
		"KSampler", "bp:a/b", "bp:A/b@1.0.0", "bp:a/b@1.0", "bp:a/b@1.0.0.0", "bp:a/b@1.0.0@2.0.0",
		"bp:a@1.0.0", "bp:a/b/c@1.0.0", "bp:/b@1.0.0", "bp:a/@1.0.0", "bp:a/b@", "bp:a/b@1.x.0",
		"a/b@1.0.0", "", "bp:", "bp:a/b@1.0.0\n",
	} {
		if id, version, ok := ParseClass(class); ok {
			t.Errorf("ParseClass(%q) = %q, %q, true; want false", class, id, version)
		}
	}
}

func TestManifestClassRoundTrips(t *testing.T) {
	m := &Manifest{ID: "x-y/z", Version: "12.0.7"}
	id, version, ok := ParseClass(m.Class())
	if !ok || id != m.ID || version != m.Version {
		t.Fatalf("ParseClass(%q) = %q, %q, %v", m.Class(), id, version, ok)
	}
}

func TestSettingIsLinkable(t *testing.T) {
	cases := []struct {
		name string
		s    Setting
		want bool
	}{
		{"seed by default", Setting{Kind: KindSeed}, true},
		{"seed opted out", Setting{Kind: KindSeed, Linkable: ptr(false)}, false},
		{"int opted in", Setting{Kind: KindInt, Linkable: ptr(true)}, true},
		{"float by default", Setting{Kind: KindFloat}, false},
	}
	for _, c := range cases {
		if got := c.s.IsLinkable(); got != c.want {
			t.Errorf("%s: IsLinkable = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestManifestProblems(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(m *Manifest)
		where  string
		want   string
	}{
		{"format", func(m *Manifest) { m.Format = 2 }, "format", "format 2 is not supported"},
		{"id", func(m *Manifest) { m.ID = "Test/x" }, "id", "id must be namespace/name"},
		{"id without namespace", func(m *Manifest) { m.ID = "ref-group" }, "id", "id must be namespace/name"},
		{"version", func(m *Manifest) { m.Version = "1.0" }, "version", "version must be MAJOR.MINOR.PATCH"},
		{"title", func(m *Manifest) { m.Title = "" }, "title", "title is empty"},
		{"blank title", func(m *Manifest) { m.Title = "  \t" }, "title", "title is empty"},

		{"input name", func(m *Manifest) { m.Inputs[0].Name = "Conditioning" }, `inputs "Conditioning"`, "name must be"},
		{"input name empty", func(m *Manifest) { m.Inputs[0].Name = "" }, `inputs[0]`, "name is empty"},
		{"input type empty", func(m *Manifest) { m.Inputs[1].Type = "" }, `inputs "vae"`, "type is empty"},
		{"input twice", func(m *Manifest) { m.Inputs = append(m.Inputs, m.Inputs[0]) }, `inputs "conditioning"`, "appears twice"},
		{"output name", func(m *Manifest) { m.Outputs[0].Name = "1st" }, `outputs "1st"`, "name must be"},
		{"output type empty", func(m *Manifest) { m.Outputs[0].Type = "" }, `outputs "conditioning"`, "type is empty"},
		{"output twice", func(m *Manifest) { m.Outputs = append(m.Outputs, m.Outputs[0]) }, `outputs "conditioning"`, "appears twice"},

		{"setting named like an input", func(m *Manifest) { m.Settings[1].Name = "vae" }, `settings "vae"`, "names an input port"},
		{"setting twice", func(m *Manifest) { m.Settings = append(m.Settings, m.Settings[0]) }, `settings "images"`, "appears twice"},
		{"setting name", func(m *Manifest) { m.Settings[1].Name = "mega-pixels" }, `settings "mega-pixels"`, "name must be"},
		{"setting name empty", func(m *Manifest) { m.Settings[2].Name = "" }, `settings[2]`, "name is empty"},
		{"unknown kind", func(m *Manifest) { m.Settings[1].Kind = "texture" }, `settings "megapixels"`, "unknown kind"},
		{"empty kind", func(m *Manifest) { m.Settings[1].Kind = "" }, `settings "megapixels"`, "unknown kind"},

		{"seed list", func(m *Manifest) { m.Settings[0].Kind = KindSeed }, `settings "images"`, "a seed cannot be a list"},
		{"list min above max", func(m *Manifest) { m.Settings[0].List = &ListBounds{Min: 3, Max: 2} }, `settings "images"`, "list min is above max"},
		{"list min negative", func(m *Manifest) { m.Settings[0].List = &ListBounds{Min: -1, Max: 2} }, `settings "images"`, "list min is negative"},
		{"list linkable", func(m *Manifest) { m.Settings[0].Linkable = ptr(true) }, `settings "images"`, "a list setting cannot be linkable"},
		{"list not linkable is fine", func(m *Manifest) { m.Settings[0].Linkable = ptr(false) }, "", ""},
		{"list default not a list", func(m *Manifest) { m.Settings[0].Default = "a.png" }, `settings "images"`, "default of a list must be a list"},
		{"list default element", func(m *Manifest) { m.Settings[0].Default = []any{"a.png", 3.0} }, `settings "images"`, "default[1] must be a string"},
		{"list default above max", func(m *Manifest) {
			m.Settings[0].Default = []any{"1", "2", "3", "4", "5", "6", "7", "8", "9"}
		}, `settings "images"`, "default has 9 items, above list max 8"},
		{"list default empty is fine", func(m *Manifest) { m.Settings[0].Default = []any{} }, "", ""},
		{"list default within bounds", func(m *Manifest) { m.Settings[0].Default = []any{"a.png", "b.png"} }, "", ""},

		{"combo without options", func(m *Manifest) { m.Settings[2].Options = nil }, `settings "upscale_method"`, "needs options or a models source"},
		{"combo with a models source", func(m *Manifest) {
			m.Settings[2].Options = nil
			m.Settings[2].Source = &ModelSource{Kind: "models", Folder: "checkpoints"}
		}, "", ""},
		{"combo with options and a models source", func(m *Manifest) {
			m.Settings[2].Source = &ModelSource{Kind: "models", Folder: "checkpoints"}
		}, `settings "upscale_method"`, "a combo needs options or a models source, not both"},
		{"combo bad source kind", func(m *Manifest) {
			m.Settings[2].Options = nil
			m.Settings[2].Source = &ModelSource{Kind: "files", Folder: "checkpoints"}
		}, `settings "upscale_method"`, `source kind must be "models"`},
		{"combo source without folder", func(m *Manifest) {
			m.Settings[2].Options = nil
			m.Settings[2].Source = &ModelSource{Kind: "models"}
		}, `settings "upscale_method"`, "source needs a folder"},
		{"combo default not an option", func(m *Manifest) { m.Settings[2].Default = "nearest" }, `settings "upscale_method"`, "default is not one of the options"},
		{"combo default not a string", func(m *Manifest) { m.Settings[2].Default = 3.0 }, `settings "upscale_method"`, "default must be a string"},
		{"combo needs a default", func(m *Manifest) { m.Settings[2].Default = nil }, `settings "upscale_method"`, "needs a default"},

		{"float default above max", func(m *Manifest) { m.Settings[1].Default = 5.0 }, `settings "megapixels"`, "default 5 is above max 4"},
		{"float default below min", func(m *Manifest) { m.Settings[1].Default = 0.1 }, `settings "megapixels"`, "default 0.1 is below min 0.25"},
		{"float default at max", func(m *Manifest) { m.Settings[1].Default = 4.0 }, "", ""},
		{"float default at min", func(m *Manifest) { m.Settings[1].Default = 0.25 }, "", ""},
		{"float default not a number", func(m *Manifest) { m.Settings[1].Default = "big" }, `settings "megapixels"`, "default must be a number"},
		{"float min above max", func(m *Manifest) { m.Settings[1].Min = ptr(5.0) }, `settings "megapixels"`, "min is above max"},
		{"float step zero", func(m *Manifest) { m.Settings[1].Step = ptr(0.0) }, `settings "megapixels"`, "step must be above 0"},

		{"int needs a default", func(m *Manifest) { m.Settings = append(m.Settings, Setting{Name: "steps", Kind: KindInt}) }, `settings "steps"`, "needs a default"},
		{"int default fractional", func(m *Manifest) {
			m.Settings = append(m.Settings, Setting{Name: "steps", Kind: KindInt, Default: 1.5})
		}, `settings "steps"`, "default 1.5 is not a whole number"},
		{"int default above max", func(m *Manifest) {
			m.Settings = append(m.Settings, Setting{Name: "steps", Kind: KindInt, Default: 31.0, Max: ptr(30.0)})
		}, `settings "steps"`, "default 31 is above max 30"},
		{"int default whole", func(m *Manifest) {
			m.Settings = append(m.Settings, Setting{Name: "steps", Kind: KindInt, Default: 20.0, Min: ptr(1.0), Max: ptr(30.0)})
		}, "", ""},

		{"seed needs a default", func(m *Manifest) { m.Settings = append(m.Settings, Setting{Name: "seed", Kind: KindSeed}) }, `settings "seed"`, "needs a default"},
		{"seed default negative", func(m *Manifest) {
			m.Settings = append(m.Settings, Setting{Name: "seed", Kind: KindSeed, Default: -1.0})
		}, `settings "seed"`, "default -1 must be a non-negative whole number"},
		{"seed default fractional", func(m *Manifest) {
			m.Settings = append(m.Settings, Setting{Name: "seed", Kind: KindSeed, Default: 0.5})
		}, `settings "seed"`, "default 0.5 must be a non-negative whole number"},
		{"seed default zero", func(m *Manifest) {
			m.Settings = append(m.Settings, Setting{Name: "seed", Kind: KindSeed, Default: 0.0})
		}, "", ""},

		{"string needs a default", func(m *Manifest) { m.Settings = append(m.Settings, Setting{Name: "note", Kind: KindString}) }, `settings "note"`, "needs a default"},
		{"string default not a string", func(m *Manifest) {
			m.Settings = append(m.Settings, Setting{Name: "note", Kind: KindString, Default: true})
		}, `settings "note"`, "default must be a string"},
		{"string default empty", func(m *Manifest) {
			m.Settings = append(m.Settings, Setting{Name: "note", Kind: KindString, Default: ""})
		}, "", ""},
		{"bool needs a default", func(m *Manifest) { m.Settings = append(m.Settings, Setting{Name: "flip", Kind: KindBool}) }, `settings "flip"`, "needs a default"},
		{"bool default not a bool", func(m *Manifest) {
			m.Settings = append(m.Settings, Setting{Name: "flip", Kind: KindBool, Default: "yes"})
		}, `settings "flip"`, "default must be true or false"},
		{"bool default false", func(m *Manifest) {
			m.Settings = append(m.Settings, Setting{Name: "flip", Kind: KindBool, Default: false})
		}, "", ""},
		{"image may omit its default", func(m *Manifest) {
			m.Settings = append(m.Settings, Setting{Name: "photo", Kind: KindImage})
		}, "", ""},
		{"image default not a string", func(m *Manifest) {
			m.Settings = append(m.Settings, Setting{Name: "photo", Kind: KindImage, Default: 4.0})
		}, `settings "photo"`, "default must be a string"},

		{"requires class empty name", func(m *Manifest) { m.Requires.Classes[""] = hash24 }, `requires.classes ""`, "class name is empty"},
		{"requires class hash", func(m *Manifest) { m.Requires.Classes["VAEEncode"] = "md5:aa" }, `requires.classes "VAEEncode"`, classHashMsg},
		{"requires class hash too short", func(m *Manifest) { m.Requires.Classes["VAEEncode"] = "sha256:aa" }, `requires.classes "VAEEncode"`, classHashMsg},
		{"requires class hash too long", func(m *Manifest) { m.Requires.Classes["VAEEncode"] = digest64 }, `requires.classes "VAEEncode"`, classHashMsg},
		{"requires class hash uppercase", func(m *Manifest) { m.Requires.Classes["VAEEncode"] = "sha256:ABCDEFABCDEFABCDEFABCDEF" }, `requires.classes "VAEEncode"`, classHashMsg},
		{"requires class hash not hex", func(m *Manifest) { m.Requires.Classes["VAEEncode"] = "sha256:zzzzzzzzzzzzzzzzzzzzzzzz" }, `requires.classes "VAEEncode"`, classHashMsg},
		{"requires class hash with a newline", func(m *Manifest) { m.Requires.Classes["VAEEncode"] = hash24 + "\n" }, `requires.classes "VAEEncode"`, classHashMsg},
		{"requires class hash ok", func(m *Manifest) { m.Requires.Classes["VAEEncode"] = "sha256:0123456789abcdef01234567" }, "", ""},
		{"requires models key", func(m *Manifest) {
			m.Requires.Models = map[string][]string{"CheckpointLoaderSimple": {"a.safetensors"}}
		}, `requires.models "CheckpointLoaderSimple"`, "key must be Class.input"},
		{"requires models key ok", func(m *Manifest) {
			m.Requires.Models = map[string][]string{"CheckpointLoaderSimple.ckpt_name": {"a.safetensors"}}
		}, "", ""},

		{"uses id", func(m *Manifest) { m.Uses = map[string]Use{"Bad": {Version: "1.0.0", Digest: digest64}} }, `uses "Bad"`, "id must be namespace/name"},
		{"uses version", func(m *Manifest) { m.Uses = map[string]Use{"a/b": {Version: "1", Digest: digest64}} }, `uses "a/b"`, "version must be MAJOR.MINOR.PATCH"},
		{"uses digest", func(m *Manifest) { m.Uses = map[string]Use{"a/b": {Version: "1.0.0", Digest: "sha256:xyz"}} }, `uses "a/b"`, useDigestMsg},
		{"uses digest too short", func(m *Manifest) { m.Uses = map[string]Use{"a/b": {Version: "1.0.0", Digest: "sha256:aa"}} }, `uses "a/b"`, useDigestMsg},
		{"uses digest is a class hash", func(m *Manifest) { m.Uses = map[string]Use{"a/b": {Version: "1.0.0", Digest: hash24}} }, `uses "a/b"`, useDigestMsg},
		{"uses digest uppercase", func(m *Manifest) {
			m.Uses = map[string]Use{"a/b": {Version: "1.0.0", Digest: "sha256:" + strings.Repeat("A", 64)}}
		}, `uses "a/b"`, useDigestMsg},
		{"uses digest too long", func(m *Manifest) { m.Uses = map[string]Use{"a/b": {Version: "1.0.0", Digest: digest64 + "0"}} }, `uses "a/b"`, useDigestMsg},
		{"uses ok", func(m *Manifest) { m.Uses = map[string]Use{"a/b": {Version: "1.0.0", Digest: digest64}} }, "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := mustParse(t, readFile(t, refGroupManifest))
			c.mutate(m)
			ps := checkManifest(m)
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

const (
	// digest64 is a well-formed package digest: "sha256:" and 64 hex digits.
	digest64 = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
	// hash24 is a well-formed class hash: "sha256:" and 24 hex digits.
	hash24 = "sha256:000000000000000000000000"

	classHashMsg = `hash must be "sha256:" and 24 lowercase hex digits`
	useDigestMsg = `digest must be "sha256:" and 64 lowercase hex digits`
)

func TestManifestValidFixtureHasNoProblems(t *testing.T) {
	if ps := checkManifest(mustParse(t, readFile(t, refGroupManifest))); len(ps) != 0 {
		t.Fatalf("fixture has problems:\n%v", ps)
	}
}

func TestManifestAcceptsEveryKind(t *testing.T) {
	m := mustParse(t, readFile(t, refGroupManifest))
	m.Settings = []Setting{
		{Name: "count", Kind: KindInt, Default: 4.0, Min: ptr(1.0), Max: ptr(9.0), Step: ptr(1.0)},
		{Name: "strength", Kind: KindFloat, Default: 0.5, Min: ptr(0.0), Max: ptr(1.0)},
		{Name: "prompt", Kind: KindString, Default: ""},
		{Name: "flip", Kind: KindBool, Default: true},
		{Name: "sampler", Kind: KindCombo, Options: []string{"euler", "dpmpp_2m"}, Default: "euler"},
		{Name: "checkpoint", Kind: KindCombo, Source: &ModelSource{Kind: "models", Folder: "checkpoints"}, Default: "a.safetensors"},
		{Name: "photo", Kind: KindImage},
		{Name: "photos", Kind: KindImage, List: &ListBounds{Min: 0, Max: 4}},
		{Name: "tags", Kind: KindString, List: &ListBounds{Min: 1, Max: 3}, Default: []any{"a"}},
		{Name: "seed", Kind: KindSeed, Default: 12345.0},
	}
	if ps := checkManifest(m); len(ps) != 0 {
		t.Fatalf("problems:\n%v", ps)
	}
}

func TestManifestReportsEveryProblem(t *testing.T) {
	m := mustParse(t, readFile(t, refGroupManifest))
	m.ID = "Bad"
	m.Title = ""
	m.Settings[1].Name = "vae"
	m.Settings[2].Default = "nearest"
	ps := checkManifest(m)
	if len(ps) != 4 {
		t.Fatalf("want 4 problems, got %d:\n%v", len(ps), ps)
	}
	// Problems come in manifest order: id, title, then settings in order.
	order := []string{"id", "title", `settings "vae"`, `settings "upscale_method"`}
	for i, w := range order {
		if ps[i].Where != w {
			t.Errorf("problem %d Where = %q, want %q", i, ps[i].Where, w)
		}
	}
}

func TestManifestProblemsAreDeterministic(t *testing.T) {
	m := mustParse(t, readFile(t, refGroupManifest))
	m.Requires.Classes = map[string]string{"A": "x", "B": "x", "C": "x", "D": "x", "E": "x"}
	first := checkManifest(m).Error()
	for i := 0; i < 20; i++ {
		if got := checkManifest(m).Error(); got != first {
			t.Fatalf("run %d differs:\n%s\n---\n%s", i, got, first)
		}
	}
}

func TestManifestChecksIgnoreRepeatsAndBindings(t *testing.T) {
	m := mustParse(t, readFile(t, refGroupManifest))
	m.Repeats = []Repeat{{Nodes: []string{"nope"}, Over: []string{"missing"}}}
	m.Inputs[0].To = []Target{{NodeID: "99", Input: "x"}}
	m.Settings[0].To = nil
	if ps := checkManifest(m); len(ps) != 0 {
		t.Fatalf("repeat rules and bindings belong to later checks:\n%v", ps)
	}
}

func TestProblemsError(t *testing.T) {
	ps := Problems{{Where: "id", Msg: "bad"}, {Where: `settings "x"`, Msg: "needs a default"}, {Msg: "no place"}}
	want := "id: bad\nsettings \"x\": needs a default\nno place"
	if got := ps.Error(); got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}
	var err error = ps
	var back Problems
	if !errors.As(err, &back) || len(back) != 3 {
		t.Fatal("Problems does not unwrap as Problems")
	}
}

func TestParseManifestReturnsProblems(t *testing.T) {
	b := bytes.Replace(readFile(t, refGroupManifest), []byte(`"format": 1`), []byte(`"format": 2`), 1)
	m, err := ParseManifest(b)
	if m != nil {
		t.Fatal("manifest returned alongside problems")
	}
	var ps Problems
	if !errors.As(err, &ps) || len(ps) != 1 || !strings.Contains(ps[0].Msg, "format 2 is not supported") {
		t.Fatalf("err = %v", err)
	}
}

func TestParseManifestRejectsUnknownField(t *testing.T) {
	b := bytes.Replace(readFile(t, refGroupManifest), []byte("{"), []byte(`{"colour": "red",`), 1)
	_, err := ParseManifest(b)
	if err == nil || !strings.Contains(err.Error(), "colour") {
		t.Fatalf("err = %v, want it to mention colour", err)
	}
	var ps Problems
	if errors.As(err, &ps) {
		t.Fatalf("an unknown field is a decode error, not a Problem: %v", err)
	}
}

func TestParseManifestRejectsUnknownNestedField(t *testing.T) {
	b := bytes.Replace(readFile(t, refGroupManifest), []byte(`"kind": "image"`), []byte(`"kind": "image", "colour": 1`), 1)
	if _, err := ParseManifest(b); err == nil || !strings.Contains(err.Error(), "colour") {
		t.Fatalf("err = %v, want it to mention colour", err)
	}
}

func TestParseManifestRejectsBadJSON(t *testing.T) {
	good := string(readFile(t, refGroupManifest))
	for name, in := range map[string]string{
		"empty":         "",
		"not json":      "format: 1",
		"truncated":     good[:len(good)/2],
		"trailing data": good + `{"format": 1}`,
		"wrong type":    strings.Replace(good, `"format": 1`, `"format": "1"`, 1),
	} {
		if m, err := ParseManifest([]byte(in)); err == nil || m != nil {
			t.Errorf("%s: ParseManifest = %v, %v; want an error", name, m, err)
		}
	}
}

func TestParseManifestAllowsTrailingWhitespace(t *testing.T) {
	mustParse(t, append(readFile(t, refGroupManifest), []byte("\n\n  \t\n")...))
}
