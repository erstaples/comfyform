// Package schema normalizes ComfyUI's /object_info into something codegen,
// validation and the importer can share.
package schema

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

type Kind int

const (
	KindLink Kind = iota
	KindInt
	KindFloat
	KindString
	KindBool
	KindCombo    // string options
	KindAnyCombo // options of mixed/non-string types
)

func (k Kind) String() string {
	return [...]string{"link", "int", "float", "string", "bool", "combo", "combo"}[k]
}

type Input struct {
	Name       string
	Type       string // socket type for links (MODEL, "*", "IMAGE,MASK"); INT/FLOAT/... otherwise
	Kind       Kind
	Required   bool
	Default    any
	HasDefault bool
	Min, Max   *float64
	Options    []any
}

type Output struct {
	Name   string
	Type   string
	IsList bool
}

type Class struct {
	Name        string
	DisplayName string
	Category    string
	Module      string // python_module, e.g. "custom_nodes.ComfyUI-Impact-Pack"
	OutputNode  bool
	Inputs      []Input
	Outputs     []Output
}

type Schema struct {
	Classes map[string]*Class
}

func (s *Schema) Names() []string {
	ns := make([]string, 0, len(s.Classes))
	for n := range s.Classes {
		ns = append(ns, n)
	}
	sort.Strings(ns)
	return ns
}

func (c *Class) Input(name string) *Input {
	for i := range c.Inputs {
		if c.Inputs[i].Name == name {
			return &c.Inputs[i]
		}
	}
	return nil
}

// IsFileList guesses whether a combo lists files on disk (models, images).
// Those lists change constantly, so they're excluded from drift hashing and
// recorded in the lockfile instead.
func (in *Input) IsFileList() bool {
	if in.Kind != KindCombo && in.Kind != KindAnyCombo {
		return false
	}
	switch in.Name {
	case "image", "video", "audio":
		return true
	}
	if len(in.Options) == 0 {
		return strings.HasSuffix(in.Name, "_name") // e.g. empty models dir
	}
	for _, o := range in.Options {
		if s, ok := o.(string); ok && (strings.Contains(s, "/") || fileExt.MatchString(s)) {
			return true
		}
	}
	return false
}

var fileExt = regexp.MustCompile(`\.(safetensors|ckpt|pt|pth|bin|gguf|sft|onnx|png|jpe?g|webp|gif|mp4|webm|wav|mp3|flac|json|yaml)$`)

// Hash fingerprints a class's interface (names, types, defaults, bounds, and
// non-file combo options) to detect drift in node code between lock and server.
func (c *Class) Hash() string {
	type sig struct {
		I []Input
		O []Output
	}
	s := sig{O: c.Outputs}
	for _, in := range c.Inputs {
		if in.IsFileList() {
			in.Options = nil
		}
		s.I = append(s.I, in)
	}
	b, _ := json.Marshal(s)
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:12])
}

// Parse reads raw /object_info JSON.
func Parse(data []byte) (*Schema, error) {
	keys, vals, err := orderedObject(data)
	if err != nil {
		return nil, fmt.Errorf("object_info: %w", err)
	}
	s := &Schema{Classes: map[string]*Class{}}
	for _, name := range keys {
		c, err := parseClass(name, vals[name])
		if err != nil {
			return nil, fmt.Errorf("class %q: %w", name, err)
		}
		s.Classes[name] = c
	}
	return s, nil
}

type rawClass struct {
	Input        json.RawMessage     `json:"input"`
	InputOrder   map[string][]string `json:"input_order"`
	Output       []json.RawMessage   `json:"output"`
	OutputIsList []bool              `json:"output_is_list"`
	OutputName   []string            `json:"output_name"`
	Name         string              `json:"name"`
	DisplayName  string              `json:"display_name"`
	Category     string              `json:"category"`
	OutputNode   bool                `json:"output_node"`
	PythonModule string              `json:"python_module"`
}

func parseClass(name string, data json.RawMessage) (*Class, error) {
	var rc rawClass
	if err := json.Unmarshal(data, &rc); err != nil {
		return nil, err
	}
	c := &Class{Name: name, DisplayName: rc.DisplayName, Category: rc.Category, Module: rc.PythonModule, OutputNode: rc.OutputNode}

	if len(rc.Input) > 0 {
		groupKeys, groups, err := orderedObject(rc.Input)
		if err != nil {
			return nil, err
		}
		_ = groupKeys
		for _, group := range []string{"required", "optional"} {
			raw, ok := groups[group]
			if !ok || string(raw) == "null" {
				continue
			}
			order, specs, err := orderedObject(raw)
			if err != nil {
				return nil, fmt.Errorf("%s inputs: %w", group, err)
			}
			if io, ok := rc.InputOrder[group]; ok && len(io) == len(order) {
				order = io
			}
			for _, iname := range order {
				in, err := parseInput(iname, specs[iname])
				if err != nil {
					return nil, fmt.Errorf("input %q: %w", iname, err)
				}
				in.Required = group == "required"
				c.Inputs = append(c.Inputs, in)
			}
		}
	}

	for i, raw := range rc.Output {
		o := Output{Type: "*"}
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			o.Type = s
		} else {
			o.Type = "COMBO" // outputs a list of options
		}
		o.Name = o.Type
		if i < len(rc.OutputName) && rc.OutputName[i] != "" {
			o.Name = rc.OutputName[i]
		}
		if i < len(rc.OutputIsList) {
			o.IsList = rc.OutputIsList[i]
		}
		c.Outputs = append(c.Outputs, o)
	}
	return c, nil
}

func parseInput(name string, raw json.RawMessage) (Input, error) {
	in := Input{Name: name}
	var spec []json.RawMessage
	if err := json.Unmarshal(raw, &spec); err != nil || len(spec) == 0 {
		return in, fmt.Errorf("unexpected spec %s", string(raw))
	}
	var opts map[string]any
	if len(spec) > 1 {
		_ = json.Unmarshal(spec[1], &opts)
	}

	var typ string
	var options []any
	if err := json.Unmarshal(spec[0], &typ); err != nil {
		// legacy combo: first element is the option list
		if err := json.Unmarshal(spec[0], &options); err != nil {
			return in, fmt.Errorf("unexpected type %s", string(spec[0]))
		}
		typ = "COMBO"
	} else if typ == "COMBO" {
		if o, ok := opts["options"].([]any); ok {
			options = o
		}
	}
	in.Type = typ

	switch typ {
	case "INT":
		in.Kind = KindInt
	case "FLOAT":
		in.Kind = KindFloat
	case "STRING":
		in.Kind = KindString
	case "BOOLEAN":
		in.Kind = KindBool
	case "COMBO":
		in.Kind = KindCombo
		in.Options = options
		for _, o := range options {
			if _, ok := o.(string); !ok {
				in.Kind = KindAnyCombo
				break
			}
		}
	default:
		in.Kind = KindLink
	}

	if in.Kind != KindLink && opts != nil {
		if d, ok := opts["default"]; ok && d != nil {
			in.Default, in.HasDefault = d, true
		}
		if v, ok := opts["min"].(float64); ok {
			in.Min = &v
		}
		if v, ok := opts["max"].(float64); ok {
			in.Max = &v
		}
	}
	// A combo with no explicit default defaults to its first option in the UI.
	// Not for file lists: silently picking whichever model sorts first is a trap.
	if in.Kind == KindCombo && !in.HasDefault && len(in.Options) > 0 && !in.IsFileList() {
		in.Default, in.HasDefault = in.Options[0], true
	}
	return in, nil
}

// orderedObject decodes a JSON object preserving key order.
func orderedObject(data []byte) ([]string, map[string]json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil {
		return nil, nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, nil, fmt.Errorf("expected object")
	}
	var keys []string
	vals := map[string]json.RawMessage{}
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return nil, nil, err
		}
		k := kt.(string)
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, nil, err
		}
		if _, seen := vals[k]; !seen {
			keys = append(keys, k)
		}
		vals[k] = v
	}
	return keys, vals, nil
}
