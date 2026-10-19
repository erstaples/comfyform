package blueprint

import (
	"slices"
	"strings"
	"testing"
)

// hiresPair parses the hires fixture twice, so a test can edit either copy.
func hiresPair(t *testing.T) (old, next *Manifest) {
	t.Helper()
	b := readFile(t, hiresDir+"/blueprint.json")
	return mustParse(t, b), mustParse(t, b)
}

func setting(m *Manifest, name string) *Setting {
	for i := range m.Settings {
		if m.Settings[i].Name == name {
			return &m.Settings[i]
		}
	}
	panic("no setting " + name)
}

func port(ports []Port, name string) *Port {
	for i := range ports {
		if ports[i].Name == name {
			return &ports[i]
		}
	}
	panic("no port " + name)
}

func removeSetting(m *Manifest, name string) {
	m.Settings = slices.DeleteFunc(m.Settings, func(s Setting) bool { return s.Name == name })
}

func removePort(ports []Port, name string) []Port {
	return slices.DeleteFunc(ports, func(p Port) bool { return p.Name == name })
}

// both applies edit to the old and the new manifest.
func both(edit func(*Manifest)) func(old, next *Manifest) {
	return func(old, next *Manifest) { edit(old); edit(next) }
}

func onNew(edit func(*Manifest)) func(old, next *Manifest) {
	return func(_, next *Manifest) { edit(next) }
}

func comboSetting() Setting {
	return Setting{Name: "style", Kind: KindCombo, Default: "a", Options: []string{"a", "b", "c"}}
}

func listSetting() Setting {
	return Setting{Name: "images", Kind: KindImage, List: &ListBounds{Min: 1, Max: 4}}
}

func TestDiffLevels(t *testing.T) {
	addCombo := func(m *Manifest) { m.Settings = append(m.Settings, comboSetting()) }
	addList := func(m *Manifest) { m.Settings = append(m.Settings, listSetting()) }
	addSecondOutput := func(m *Manifest) {
		m.Outputs = append(m.Outputs, Port{Name: "preview", Type: "IMAGE", From: &Socket{NodeID: "2", Output: 1}})
	}
	addThirdOutput := func(m *Manifest) {
		addSecondOutput(m)
		m.Outputs = append(m.Outputs, Port{Name: "extra", Type: "MASK", From: &Socket{NodeID: "2", Output: 2}})
	}

	cases := []struct {
		name string
		edit func(old, next *Manifest)
		want Level
		what string // each of these appears in some change's What; "" for no change
	}{
		// Inputs.
		{"input removed", onNew(func(m *Manifest) { m.Inputs = removePort(m.Inputs, "model") }), Major, `input "model" removed`},
		{"input retyped", onNew(func(m *Manifest) { port(m.Inputs, "positive").Type = "LATENT" }), Major,
			`input "positive" changed from CONDITIONING to LATENT`},
		{"input added, required", onNew(func(m *Manifest) {
			m.Inputs = append(m.Inputs, Port{Name: "clip_vision", Type: "CLIP_VISION"})
		}), Major, `input "clip_vision" added and required`},
		{"input added, optional", onNew(func(m *Manifest) {
			m.Inputs = append(m.Inputs, Port{Name: "clip_vision", Type: "CLIP_VISION", Optional: true})
		}), Minor, `input "clip_vision" added`},
		{"input required to optional", onNew(func(m *Manifest) { port(m.Inputs, "negative").Optional = true }), Minor,
			`input "negative" is now optional`},
		{"input optional to required", func(old, next *Manifest) { port(old.Inputs, "negative").Optional = true }, Major,
			`input "negative" is now required`},
		{"input optional stays optional", both(func(m *Manifest) { port(m.Inputs, "negative").Optional = true }), Patch, ""},
		{"input renamed", onNew(func(m *Manifest) { port(m.Inputs, "samples").Name = "latent_in" }), Major, `input "samples" removed`},
		{"input retyped and made optional", onNew(func(m *Manifest) {
			p := port(m.Inputs, "samples")
			p.Type = "IMAGE"
			p.Optional = true
		}), Major, `input "samples" changed from LATENT to IMAGE`},
		{"input targets moved", onNew(func(m *Manifest) { port(m.Inputs, "model").To[0].NodeID = "9" }), Patch, ""},

		// Outputs.
		{"output removed", onNew(func(m *Manifest) { m.Outputs = nil }), Major, `output "latent" removed`},
		{"output retyped", onNew(func(m *Manifest) { port(m.Outputs, "latent").Type = "IMAGE" }), Major,
			`output "latent" changed from LATENT to IMAGE`},
		{"output added at the end", onNew(addSecondOutput), Minor, `output "preview" added`},
		{"output added at the start", onNew(func(m *Manifest) {
			m.Outputs = slices.Insert(m.Outputs, 0, Port{Name: "preview", Type: "IMAGE", From: &Socket{NodeID: "2", Output: 1}})
		}), Major, "outputs reordered"},
		{"output added in the middle", func(old, next *Manifest) {
			addSecondOutput(old)
			addSecondOutput(next)
			next.Outputs = slices.Insert(next.Outputs, 1, Port{Name: "mid", Type: "MASK", From: &Socket{NodeID: "2", Output: 2}})
		}, Major, "outputs reordered"},
		{"outputs swapped", func(old, next *Manifest) {
			addSecondOutput(old)
			addSecondOutput(next)
			slices.Reverse(next.Outputs)
		}, Major, "outputs reordered"},
		{"output in the middle removed", func(old, next *Manifest) {
			addThirdOutput(old)
			addThirdOutput(next)
			next.Outputs = removePort(next.Outputs, "preview")
		}, Major, `output "preview" removed`},
		{"output source moved", onNew(func(m *Manifest) { port(m.Outputs, "latent").From.NodeID = "1" }), Patch, ""},

		// Settings.
		{"setting removed", onNew(func(m *Manifest) { removeSetting(m, "denoise") }), Major, `setting "denoise" removed`},
		{"setting kind changed", onNew(func(m *Manifest) { setting(m, "steps").Kind = KindFloat }), Major,
			`setting "steps" changed from int to float`},
		{"setting became a list", onNew(func(m *Manifest) { setting(m, "steps").List = &ListBounds{Min: 1, Max: 3} }), Major,
			`setting "steps" is now a list`},
		{"setting stopped being a list", func(old, next *Manifest) {
			old.Settings = append(old.Settings, listSetting())
			next.Settings = append(next.Settings, Setting{Name: "images", Kind: KindImage})
		}, Major, `setting "images" is no longer a list`},
		{"setting added with a default", onNew(func(m *Manifest) {
			m.Settings = append(m.Settings, Setting{Name: "crop", Kind: KindBool, Default: false})
		}), Minor, `setting "crop" added`},
		{"image setting added without a default", onNew(func(m *Manifest) {
			m.Settings = append(m.Settings, Setting{Name: "mask", Kind: KindImage})
		}), Minor, `setting "mask" added`},
		{"list setting added without a default", onNew(addList), Minor, `setting "images" added`},
		{"setting added without a default", onNew(func(m *Manifest) {
			m.Settings = append(m.Settings, Setting{Name: "x", Kind: KindInt})
		}), Major, `setting "x" added without a default`},
		{"setting renamed", onNew(func(m *Manifest) { setting(m, "steps").Name = "iterations" }), Major, `setting "steps" removed`},
		{"min raised", onNew(func(m *Manifest) { setting(m, "scale_by").Min = ptr(2.0) }), Major, `setting "scale_by" range narrowed`},
		{"max lowered", onNew(func(m *Manifest) { setting(m, "scale_by").Max = ptr(3.0) }), Major, `setting "scale_by" range narrowed`},
		{"min added", onNew(func(m *Manifest) { setting(m, "seed").Min = ptr(0.0) }), Major, `setting "seed" range narrowed`},
		{"max added", onNew(func(m *Manifest) { setting(m, "seed").Max = ptr(99.0) }), Major, `setting "seed" range narrowed`},
		{"min lowered", onNew(func(m *Manifest) { setting(m, "scale_by").Min = ptr(0.5) }), Minor, `setting "scale_by" range widened`},
		{"max raised", onNew(func(m *Manifest) { setting(m, "scale_by").Max = ptr(8.0) }), Minor, `setting "scale_by" range widened`},
		{"min removed", onNew(func(m *Manifest) { setting(m, "scale_by").Min = nil }), Minor, `setting "scale_by" range widened`},
		{"max removed", onNew(func(m *Manifest) { setting(m, "scale_by").Max = nil }), Minor, `setting "scale_by" range widened`},
		{"min up and max up", onNew(func(m *Manifest) {
			s := setting(m, "scale_by")
			s.Min, s.Max = ptr(2.0), ptr(8.0)
		}), Major, `setting "scale_by" range narrowed`},
		{"step and default changed", onNew(func(m *Manifest) {
			s := setting(m, "scale_by")
			s.Step, s.Default = ptr(0.25), 2.0
		}), Patch, ""},
		{"setting targets moved", onNew(func(m *Manifest) { setting(m, "steps").To[0].Input = "cfg" }), Patch, ""},
		{"option removed", func(old, next *Manifest) {
			addCombo(old)
			addCombo(next)
			s := setting(next, "style")
			s.Options = []string{"a", "b"}
		}, Major, `setting "style" range narrowed`},
		{"option added", func(old, next *Manifest) {
			addCombo(old)
			addCombo(next)
			setting(next, "style").Options = []string{"a", "b", "c", "d"}
		}, Minor, `setting "style" range widened`},
		{"options reordered", func(old, next *Manifest) {
			addCombo(old)
			addCombo(next)
			setting(next, "style").Options = []string{"c", "b", "a"}
		}, Patch, ""},
		{"option swapped", func(old, next *Manifest) {
			addCombo(old)
			addCombo(next)
			setting(next, "style").Options = []string{"a", "b", "z"}
		}, Major, `setting "style" range narrowed`},
		{"list max lowered", func(old, next *Manifest) {
			addList(old)
			addList(next)
			setting(next, "images").List.Max = 3
		}, Major, `setting "images" range narrowed`},
		{"list min raised", func(old, next *Manifest) {
			addList(old)
			addList(next)
			setting(next, "images").List.Min = 2
		}, Major, `setting "images" range narrowed`},
		{"list max raised", func(old, next *Manifest) {
			addList(old)
			addList(next)
			setting(next, "images").List.Max = 8
		}, Minor, `setting "images" range widened`},
		{"list min lowered", func(old, next *Manifest) {
			addList(old)
			addList(next)
			setting(next, "images").List.Min = 0
		}, Minor, `setting "images" range widened`},
		{"linkable true to false", onNew(func(m *Manifest) { setting(m, "seed").Linkable = ptr(false) }), Major,
			`setting "seed" no longer linkable`},
		{"linkable false to true", onNew(func(m *Manifest) { setting(m, "steps").Linkable = ptr(true) }), Minor,
			`setting "steps" now linkable`},
		{"seed linkable by default and explicitly", onNew(func(m *Manifest) { setting(m, "seed").Linkable = ptr(true) }), Patch, ""},
		{"linkable false stays false", onNew(func(m *Manifest) { setting(m, "steps").Linkable = ptr(false) }), Patch, ""},
		{"combo source folder changed", func(old, next *Manifest) {
			src := func(folder string) Setting {
				return Setting{Name: "ckpt", Kind: KindCombo, Source: &ModelSource{Kind: "models", Folder: folder}}
			}
			old.Settings = append(old.Settings, src("checkpoints"))
			next.Settings = append(next.Settings, src("unet"))
		}, Major, `setting "ckpt" source changed`},
		{"combo source unchanged", both(func(m *Manifest) {
			m.Settings = append(m.Settings, Setting{Name: "ckpt", Kind: KindCombo, Source: &ModelSource{Kind: "models", Folder: "checkpoints"}})
		}), Patch, ""},
		{"combo options to source", func(old, next *Manifest) {
			addCombo(old)
			next.Settings = append(next.Settings, Setting{Name: "style", Kind: KindCombo,
				Source: &ModelSource{Kind: "models", Folder: "checkpoints"}})
		}, Major, `setting "style" source changed`},
		{"combo source to options", func(old, next *Manifest) {
			old.Settings = append(old.Settings, Setting{Name: "style", Kind: KindCombo,
				Source: &ModelSource{Kind: "models", Folder: "checkpoints"}})
			addCombo(next)
		}, Major, `setting "style" source changed`},

		// Result.
		{"result added", onNew(func(m *Manifest) { m.Result = "2" }), Minor, "result changed"},
		{"result changed", func(old, next *Manifest) { old.Result, next.Result = "1", "2" }, Major, "result changed"},
		{"result removed", func(old, next *Manifest) { old.Result = "2" }, Major, "result changed"},
		{"result same", both(func(m *Manifest) { m.Result = "2" }), Patch, ""},

		// Nothing in the interface.
		{"identical", func(old, next *Manifest) {}, Patch, ""},
		{"title, description, requires and uses", onNew(func(m *Manifest) {
			m.Title = "Another title"
			m.Description = "Another description"
			m.Requires.Classes["KSampler"] = "sha256:111111111111111111111111"
			m.Uses["a/b"] = Use{Version: "1.0.0"}
			m.Repeats = []Repeat{{Nodes: []string{"1"}}}
		}), Patch, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			old, next := hiresPair(t)
			c.edit(old, next)
			changes := Diff(old, next)
			if got := LevelOf(changes); got != c.want {
				t.Fatalf("LevelOf = %v, want %v; changes = %+v", got, c.want, changes)
			}
			if c.what == "" {
				if len(changes) != 0 {
					t.Fatalf("changes = %+v, want none", changes)
				}
				return
			}
			for _, ch := range changes {
				if ch.Level == c.want && strings.Contains(ch.What, c.what) {
					return
				}
			}
			t.Fatalf("no %v change contains %q; changes = %+v", c.want, c.what, changes)
		})
	}
}

func TestDiffRangeMovedBothWays(t *testing.T) {
	old, next := hiresPair(t)
	s := setting(next, "scale_by")
	s.Min, s.Max = ptr(2.0), ptr(8.0)
	want := []Change{
		{Major, `setting "scale_by" range narrowed`},
		{Minor, `setting "scale_by" range widened`},
	}
	if got := Diff(old, next); !slices.Equal(got, want) {
		t.Fatalf("Diff = %+v, want %+v", got, want)
	}
}

func TestDiffSourceChangeReportsNoOptionChanges(t *testing.T) {
	old, next := hiresPair(t)
	old.Settings = append(old.Settings, comboSetting())
	next.Settings = append(next.Settings, Setting{Name: "style", Kind: KindCombo,
		Source: &ModelSource{Kind: "models", Folder: "checkpoints"}})
	want := []Change{{Major, `setting "style" source changed`}}
	if got := Diff(old, next); !slices.Equal(got, want) {
		t.Fatalf("Diff = %+v, want %+v", got, want)
	}
}

func TestDiffRemovedOutputInTheMiddleIsNotReordered(t *testing.T) {
	old, next := hiresPair(t)
	for _, m := range []*Manifest{old, next} {
		m.Outputs = append(m.Outputs,
			Port{Name: "preview", Type: "IMAGE", From: &Socket{NodeID: "2", Output: 1}},
			Port{Name: "extra", Type: "MASK", From: &Socket{NodeID: "2", Output: 2}})
	}
	next.Outputs = removePort(next.Outputs, "preview")
	want := []Change{{Major, `output "preview" removed`}}
	if got := Diff(old, next); !slices.Equal(got, want) {
		t.Fatalf("Diff = %+v, want %+v", got, want)
	}
}

func TestDiffListsChangesInManifestOrder(t *testing.T) {
	old, next := hiresPair(t)
	next.Inputs = append(next.Inputs, Port{Name: "mask", Type: "MASK", Optional: true})
	next.Outputs = append(next.Outputs, Port{Name: "preview", Type: "IMAGE", From: &Socket{NodeID: "2", Output: 1}})
	removeSetting(next, "denoise")
	next.Result = "2"
	want := []Change{
		{Minor, `input "mask" added`},
		{Minor, `output "preview" added`},
		{Major, `setting "denoise" removed`},
		{Minor, "result changed"},
	}
	if got := Diff(old, next); !slices.Equal(got, want) {
		t.Fatalf("Diff = %+v, want %+v", got, want)
	}
}

func TestDiffOrderIgnoresSettingOrder(t *testing.T) {
	old, next := hiresPair(t)
	slices.Reverse(next.Settings)
	slices.Reverse(next.Inputs)
	if got := Diff(old, next); len(got) != 0 {
		t.Fatalf("Diff = %+v, want none", got)
	}
}

func TestLevelOf(t *testing.T) {
	if got := LevelOf(nil); got != Patch {
		t.Fatalf("LevelOf(nil) = %v", got)
	}
	changes := []Change{{Minor, "a"}, {Major, "b"}, {Minor, "c"}}
	if got := LevelOf(changes); got != Major {
		t.Fatalf("LevelOf = %v", got)
	}
	if got := LevelOf(changes[:1]); got != Minor {
		t.Fatalf("LevelOf = %v", got)
	}
}

func TestLevelString(t *testing.T) {
	for l, want := range map[Level]string{Patch: "patch", Minor: "minor", Major: "major"} {
		if got := l.String(); got != want {
			t.Errorf("Level(%d).String() = %q, want %q", int(l), got, want)
		}
	}
	if Patch != 0 || Minor != 1 || Major != 2 {
		t.Fatalf("levels = %d %d %d", Patch, Minor, Major)
	}
}

func TestCheckVersion(t *testing.T) {
	removeDenoise := func(m *Manifest) { removeSetting(m, "denoise") }
	addOptional := func(m *Manifest) {
		m.Inputs = append(m.Inputs, Port{Name: "mask", Type: "MASK", Optional: true})
	}
	cases := []struct {
		name    string
		oldV    string
		newV    string
		edit    func(next *Manifest)
		wantErr string // "" for accepted
	}{
		{"removal needs major: minor", "1.0.0", "1.1.0", removeDenoise,
			`1.1.0 understates the change: setting "denoise" removed needs a major version`},
		{"removal needs major: patch", "1.0.0", "1.0.1", removeDenoise,
			`1.0.1 understates the change: setting "denoise" removed needs a major version`},
		{"removal needs major: major", "1.0.0", "2.0.0", removeDenoise, ""},
		{"removal accepts a bigger major", "1.4.2", "3.0.0", removeDenoise, ""},
		{"optional input: patch", "1.0.0", "1.0.1", addOptional,
			`1.0.1 understates the change: input "mask" added needs a minor version`},
		{"optional input: minor", "1.0.0", "1.1.0", addOptional, ""},
		{"optional input: major", "1.0.0", "2.0.0", addOptional, ""},
		{"optional input: minor with a patch", "1.2.3", "1.3.0", addOptional, ""},
		{"optional input: patch bump of a later minor", "1.2.3", "1.2.4", addOptional,
			`1.2.4 understates the change: input "mask" added needs a minor version`},
		{"no change: patch", "1.0.0", "1.0.1", nil, ""},
		{"no change: minor", "1.0.0", "1.1.0", nil, ""},
		{"no change: major", "1.0.0", "2.0.0", nil, ""},
		{"no change: same", "1.0.0", "1.0.0", nil, "1.0.0 is not above 1.0.0"},
		{"no change: lower", "1.2.0", "1.1.9", nil, "1.1.9 is not above 1.2.0"},
		{"lower major with a bigger minor", "2.0.0", "1.9.9", nil, "1.9.9 is not above 2.0.0"},
		{"same version beats the understatement", "1.0.0", "1.0.0", removeDenoise, "1.0.0 is not above 1.0.0"},
		{"versions compare by number: minor", "1.9.0", "1.10.0", nil, ""},
		{"versions compare by number: not above", "1.10.0", "1.9.0", nil, "1.9.0 is not above 1.10.0"},
		{"versions compare by number: patch", "1.0.9", "1.0.10", nil, ""},
		{"versions compare by number: major", "9.0.0", "10.0.0", removeDenoise, ""},
		{"first change at the required level", "1.0.0", "1.1.0", func(m *Manifest) {
			addOptional(m)
			m.Inputs = removePort(m.Inputs, "model")
			removeDenoise(m)
		}, `1.1.0 understates the change: input "model" removed needs a major version`},
		{"minor change among patches", "1.0.0", "1.0.1", func(m *Manifest) {
			m.Title = "x"
			addOptional(m)
		}, `1.0.1 understates the change: input "mask" added needs a minor version`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			old, next := hiresPair(t)
			old.Version, next.Version = c.oldV, c.newV
			if c.edit != nil {
				c.edit(next)
			}
			err := CheckVersion(old, next)
			switch {
			case c.wantErr == "" && err != nil:
				t.Fatalf("CheckVersion: %v", err)
			case c.wantErr != "" && err == nil:
				t.Fatalf("CheckVersion = nil, want %q", c.wantErr)
			case c.wantErr != "" && err.Error() != c.wantErr:
				t.Fatalf("CheckVersion = %q, want %q", err, c.wantErr)
			}
		})
	}
}

func TestCheckVersionRefusesDifferentIDs(t *testing.T) {
	old, next := hiresPair(t)
	old.ID, next.ID = "a/b", "a/c"
	next.Version = "2.0.0"
	err := CheckVersion(old, next)
	if err == nil || err.Error() != "ids differ: a/b and a/c" {
		t.Fatalf("CheckVersion = %v", err)
	}
}

func TestCheckVersionRefusesMalformedVersions(t *testing.T) {
	for _, v := range []string{"", "1.0", "1.0.0.0", "v1.0.0", "1.0.x", "1.0.0-beta", "99999999999999999999.0.0"} {
		old, next := hiresPair(t)
		next.Version = v
		err := CheckVersion(old, next)
		if err == nil || !strings.Contains(err.Error(), "version") {
			t.Errorf("new version %q: CheckVersion = %v", v, err)
		}
		old.Version, next.Version = v, "9.0.0"
		if err := CheckVersion(old, next); err == nil || !strings.Contains(err.Error(), "version") {
			t.Errorf("old version %q: CheckVersion = %v", v, err)
		}
	}
}
