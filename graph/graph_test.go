package graph

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestMetaKeepsBlueprint(t *testing.T) {
	in := []byte(`{"14":{"class_type":"bp:a/b@1.0.0","inputs":{},"_meta":{"title":"R","blueprint":"sha256:ab"}}}`)
	var p Prompt
	if err := json.Unmarshal(in, &p); err != nil {
		t.Fatal(err)
	}
	if got := p["14"].Meta.Blueprint; got != "sha256:ab" {
		t.Fatalf("blueprint = %q", got)
	}
	out, _ := json.Marshal(p)
	if !bytes.Contains(out, []byte(`"blueprint":"sha256:ab"`)) {
		t.Fatalf("lost on marshal: %s", out)
	}
}

func TestHashesIgnoreBlueprintMeta(t *testing.T) {
	a := Prompt{"1": {Class: "X", Inputs: map[string]any{}, Meta: &Meta{Blueprint: "sha256:aa"}}}
	b := Prompt{"1": {Class: "X", Inputs: map[string]any{}, Meta: &Meta{Blueprint: "sha256:bb"}}}
	ha, _ := a.Hashes()
	hb, _ := b.Hashes()
	if ha["1"] != hb["1"] {
		t.Fatal("hash depends on _meta")
	}
}
