package codegen

import (
	"strings"
	"testing"

	"github.com/erstaples/comfyform/schema"
)

// Long combo lists are truncated in field comments; the cut must not split a
// multibyte rune, or gofmt rejects the generated file.
func TestGenerateTruncatesMultibyteCombos(t *testing.T) {
	opts := `["` + strings.Repeat("ab", 37) + `", "→ arrow", "日本語", "x"]`
	raw := `{"Picker": {
		"input": {"required": {"mode": [` + opts + `, {}]}},
		"output": ["STRING"], "output_name": ["STRING"],
		"name": "Picker", "display_name": "Picker", "category": "test", "output_node": false
	}}`
	s, err := schema.Parse([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Generate(s, "nodes"); err != nil {
		t.Fatal(err)
	}
}

// A class whose name is already its Go name keeps it, even when another
// class (here WAS's "Checkpoint Loader (Simple)") maps to the same Go name
// and sorts first.
func TestResolvePrefersExactClassName(t *testing.T) {
	cls := func(name, module string) string {
		return `"` + name + `": {"input": {"required": {}}, "output": [], "output_name": [],
			"name": "` + name + `", "display_name": "` + name + `", "category": "test",
			"output_node": true, "python_module": "` + module + `"}`
	}
	raw := `{` + cls("Checkpoint Loader (Simple)", "custom_nodes.was-node-suite-comfyui") + `,` +
		cls("CheckpointLoaderSimple", "nodes") + `}`
	s, err := schema.Parse([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	names := Resolve(s)
	if got := names["CheckpointLoaderSimple"].Func; got != "CheckpointLoaderSimple" {
		t.Errorf("core class got Go name %q, want CheckpointLoaderSimple", got)
	}
	if got := names["Checkpoint Loader (Simple)"].Func; got != "CheckpointLoaderSimple2" {
		t.Errorf("WAS class got Go name %q, want CheckpointLoaderSimple2", got)
	}
}
