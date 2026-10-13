package engine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/format"
	"go/token"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/erstaples/comfyform/codegen"
	"github.com/erstaples/comfyform/graph"
	"github.com/erstaples/comfyform/schema"
)

// ImportOptions controls code generation for an imported workflow.
type ImportOptions struct {
	NodesImport string // import path of the generated bindings package
	NodesPkg    string // its package name, default "nodes"
}

// ParseWorkflow accepts API-format JSON (Export (API) / Save (API Format)).
func ParseWorkflow(data []byte) (graph.Prompt, error) {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil, err
	}
	if _, ok := probe["nodes"]; ok {
		if _, ok := probe["links"]; ok {
			return nil, fmt.Errorf("this is a UI-format workflow; in ComfyUI use Workflow → Export (API) and import that file")
		}
	}
	if raw, ok := probe["prompt"]; ok { // wrapped {"prompt": {...}}
		data = raw
	}
	var p graph.Prompt
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, err
	}
	for id, n := range p {
		if n == nil || n.Class == "" {
			return nil, fmt.Errorf("node %q has no class_type; is this API-format JSON?", id)
		}
	}
	return p, nil
}

var slugRe = regexp.MustCompile(`[^a-z0-9]+`)

// Import converts an API-format prompt into a comfyform Go program.
func Import(p graph.Prompt, s *schema.Schema, opt ImportOptions) ([]byte, error) {
	if opt.NodesPkg == "" {
		opt.NodesPkg = "nodes"
	}
	names := codegen.Resolve(s)
	order := TopoOrder(p)

	// friendly node names: custom titles win, otherwise class + original id
	nodeName := map[string]string{}
	varName := map[string]string{}
	usedNames := map[string]bool{}
	usedVars := map[string]bool{"ctx": true, "comfy": true, "graph": true, opt.NodesPkg: true, "err": true}
	for _, id := range order {
		n := p[id]
		base := ""
		if n.Meta != nil && n.Meta.Title != "" {
			c := s.Classes[n.Class]
			if n.Meta.Title != n.Class && (c == nil || n.Meta.Title != c.DisplayName) {
				base = strings.Trim(slugRe.ReplaceAllString(strings.ToLower(n.Meta.Title), "_"), "_")
			}
		}
		if base == "" {
			base = strings.ToLower(codegen.GoName(n.Class)) + "_" + strings.Trim(slugRe.ReplaceAllString(strings.ToLower(id), "_"), "_")
		}
		name := base
		for i := 2; usedNames[name]; i++ {
			name = fmt.Sprintf("%s_%d", base, i)
		}
		usedNames[name] = true
		nodeName[id] = name

		v := lowerCamel(name)
		if token.IsKeyword(v) {
			v += "_"
		}
		for i := 2; usedVars[v]; i++ {
			v = fmt.Sprintf("%s%d", lowerCamel(name), i)
		}
		usedVars[v] = true
		varName[id] = v
	}

	// a node needs a variable only if something downstream references it
	referenced := map[string]bool{}
	for _, n := range p {
		for _, v := range n.Inputs {
			if up, _, ok := graph.AsLink(v); ok {
				referenced[up] = true
			}
		}
	}

	var body bytes.Buffer
	w := func(f string, a ...any) { fmt.Fprintf(&body, f, a...) }
	pkg := opt.NodesPkg
	usesGraph := false

	linkExpr := func(up string, idx int, wantType string) (expr string, typed bool) {
		un := p[up]
		if un == nil {
			usesGraph = true
			return fmt.Sprintf("graph.Link(%q, %d)", up, idx), false
		}
		c := s.Classes[un.Class]
		nm := names[un.Class]
		if c == nil || nm == nil || idx >= len(nm.Outputs) {
			usesGraph = true
			return fmt.Sprintf("graph.Link(%s, %d)", idRef(varName[up], c != nil && nm != nil), idx), false
		}
		otype := c.Outputs[idx].Type
		typed = wantType != "" && (codegen.TypeName(wantType) == "" || codegen.TypeName(wantType) == codegen.TypeName(otype))
		return varName[up] + "." + nm.Outputs[idx], typed
	}

	for _, id := range order {
		n := p[id]
		c := s.Classes[n.Class]
		nm := names[n.Class]
		assign := ""
		if referenced[id] {
			assign = varName[id] + " := "
		}

		if c == nil || nm == nil { // not installed here: emit an untyped node
			usesGraph = true
			w("\t// %s is not in the schema; emitted as a raw node.\n", n.Class)
			w("\t%sctx.Node(%q, %q, graph.Inputs{\n", assign, nodeName[id], n.Class)
			for _, k := range sortedKeys(n.Inputs) {
				v := n.Inputs[k]
				if up, idx, ok := graph.AsLink(v); ok {
					e, _ := linkExpr(up, idx, "")
					w("\t\t%q: %s,\n", k, e)
				} else {
					w("\t\t%q: %s,\n", k, goLiteral(v))
				}
			}
			w("\t}, nil)\n")
			continue
		}

		var fields, raw []string
		handled := map[string]bool{}
		for _, in := range c.Inputs {
			v, ok := n.Inputs[in.Name]
			if !ok {
				continue
			}
			handled[in.Name] = true
			f := nm.Inputs[in.Name]
			if up, idx, isLink := graph.AsLink(v); isLink {
				if in.Kind == schema.KindLink {
					if e, typed := linkExpr(up, idx, in.Type); typed {
						fields = append(fields, fmt.Sprintf("%s: %s", f, e))
						continue
					}
				}
				e, _ := linkExpr(up, idx, "")
				usesGraph = true
				raw = append(raw, fmt.Sprintf("%q: %s", in.Name, e))
				continue
			}
			if lit, ok := fieldLiteral(in, v); ok {
				if lit != "" {
					fields = append(fields, fmt.Sprintf("%s: %s", f, lit))
				}
				continue
			}
			usesGraph = true
			raw = append(raw, fmt.Sprintf("%q: %s", in.Name, goLiteral(v)))
		}
		for _, k := range sortedKeys(n.Inputs) {
			if handled[k] {
				continue
			}
			v := n.Inputs[k]
			usesGraph = true
			if up, idx, ok := graph.AsLink(v); ok {
				e, _ := linkExpr(up, idx, "")
				raw = append(raw, fmt.Sprintf("%q: %s", k, e))
			} else {
				raw = append(raw, fmt.Sprintf("%q: %s", k, goLiteral(v)))
			}
		}
		if len(raw) > 0 {
			fields = append(fields, "Raw: graph.Inputs{\n"+strings.Join(raw, ",\n")+",\n}")
		}
		w("\t%s%s.%s(ctx, %q, %s.%s{", assign, pkg, nm.Func, nodeName[id], pkg, nm.Args)
		if len(fields) > 0 {
			w("\n\t\t%s,\n\t", strings.Join(fields, ",\n\t\t"))
		}
		w("})\n")
	}

	var src bytes.Buffer
	fmt.Fprintf(&src, "// Imported by comfy import. Rename nodes freely; ids only need to be unique.\npackage main\n\nimport (\n")
	fmt.Fprintf(&src, "\t%q\n", "github.com/erstaples/comfyform/comfy")
	if usesGraph {
		fmt.Fprintf(&src, "\t%q\n", "github.com/erstaples/comfyform/graph")
	}
	if opt.NodesPkg != pathBase(opt.NodesImport) {
		fmt.Fprintf(&src, "\t%s %q\n", opt.NodesPkg, opt.NodesImport)
	} else {
		fmt.Fprintf(&src, "\t%q\n", opt.NodesImport)
	}
	fmt.Fprintf(&src, ")\n\nfunc main() {\n\tcomfy.Run(func(ctx *comfy.Context) error {\n")
	src.Write(body.Bytes())
	fmt.Fprintf(&src, "\t\treturn nil\n\t})\n}\n")

	out, err := format.Source(src.Bytes())
	if err != nil {
		return src.Bytes(), fmt.Errorf("gofmt imported code: %w", err)
	}
	return out, nil
}

// idRef refers to a node id in code: typed nodes expose .Node, raw nodes are
// the id string itself.
func idRef(v string, typed bool) string {
	if typed {
		return v + ".Node"
	}
	return v
}

// fieldLiteral renders v for a typed Args field. ok=false means it must go in
// Raw (wrong type, or a zero the binding would replace with a nonzero default).
// lit=="" with ok=true means a zero the binding reproduces on its own.
func fieldLiteral(in schema.Input, v any) (lit string, ok bool) {
	isZero := false
	switch in.Kind {
	case schema.KindInt:
		f, isNum := v.(float64)
		if !isNum || f != math.Trunc(f) || f > math.MaxInt64 || f < math.MinInt64 {
			return "", false
		}
		isZero, lit = f == 0, strconv.FormatInt(int64(f), 10)
	case schema.KindFloat:
		f, isNum := v.(float64)
		if !isNum {
			return "", false
		}
		isZero, lit = f == 0, strconv.FormatFloat(f, 'g', -1, 64)
	case schema.KindString, schema.KindCombo:
		str, isStr := v.(string)
		if !isStr {
			return "", false
		}
		isZero, lit = str == "", strconv.Quote(str)
	case schema.KindBool:
		b, isBool := v.(bool)
		if !isBool {
			return "", false
		}
		return fmt.Sprintf("graph.Bool(%v)", b), true
	case schema.KindAnyCombo:
		return goLiteral(v), true
	default:
		return "", false
	}
	if isZero {
		// zero is "use default" in bindings; only safe if the default is zero too
		if !in.HasDefault || isZeroValue(in.Default) {
			return "", true
		}
		return "", false
	}
	return lit, true
}

func isZeroValue(v any) bool {
	switch t := v.(type) {
	case float64:
		return t == 0
	case string:
		return t == ""
	case bool:
		return !t
	}
	return v == nil
}

func goLiteral(v any) string {
	switch t := v.(type) {
	case nil:
		return "nil"
	case string:
		return strconv.Quote(t)
	case bool:
		return strconv.FormatBool(t)
	case float64:
		if t == math.Trunc(t) && math.Abs(t) < 1e15 {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'g', -1, 64)
	case []any:
		parts := make([]string, len(t))
		for i, e := range t {
			parts[i] = goLiteral(e)
		}
		return "[]any{" + strings.Join(parts, ", ") + "}"
	case map[string]any:
		var parts []string
		for _, k := range sortedKeys(t) {
			parts = append(parts, fmt.Sprintf("%q: %s", k, goLiteral(t[k])))
		}
		return "map[string]any{" + strings.Join(parts, ", ") + "}"
	default:
		return fmt.Sprintf("%#v", t)
	}
}

func lowerCamel(snake string) string {
	g := codegen.GoName(snake)
	if g == "" {
		return "n"
	}
	r := []rune(g)
	i := 0
	for i < len(r) && r[i] >= 'A' && r[i] <= 'Z' {
		i++
	}
	if i > 1 && i < len(r) {
		i--
	}
	if i == 0 {
		i = 1
	}
	return strings.ToLower(string(r[:i])) + string(r[i:])
}

func sortedKeys[V any](m map[string]V) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

func pathBase(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}
