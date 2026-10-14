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
