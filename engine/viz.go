package engine

import (
	"fmt"
	"strings"

	"github.com/erstaples/comfyform/graph"
	"github.com/erstaples/comfyform/schema"
)

// Mermaid renders the prompt as a left-to-right flowchart. Dotted ids become
// subgraphs, so components show up as boxes.
func Mermaid(p graph.Prompt, s *schema.Schema) string {
	var b strings.Builder
	b.WriteString("flowchart LR\n")
	mid := map[string]string{}
	for i, id := range TopoOrder(p) {
		mid[id] = fmt.Sprintf("n%d", i)
	}
	groups := map[string][]string{}
	var groupOrder []string
	for _, id := range TopoOrder(p) {
		g := ""
		if i := strings.LastIndex(id, "."); i >= 0 {
			g = id[:i]
		}
		if _, ok := groups[g]; !ok {
			groupOrder = append(groupOrder, g)
		}
		groups[g] = append(groups[g], id)
	}
	for _, g := range groupOrder {
		indent := "  "
		if g != "" {
			fmt.Fprintf(&b, "  subgraph %s[\"%s\"]\n", strings.ReplaceAll(g, ".", "_"), g)
			indent = "    "
		}
		for _, id := range groups[g] {
			short := id[strings.LastIndex(id, ".")+1:]
			fmt.Fprintf(&b, "%s%s[\"%s<br/><small>%s</small>\"]\n", indent, mid[id], short, p[id].Class)
		}
		if g != "" {
			b.WriteString("  end\n")
		}
	}
	for _, id := range TopoOrder(p) {
		for _, k := range sortedKeys(p[id].Inputs) {
			up, idx, ok := graph.AsLink(p[id].Inputs[k])
			if !ok || mid[up] == "" {
				continue
			}
			fmt.Fprintf(&b, "  %s -->|%s| %s\n", mid[up], outLabel(p, s, up, idx), mid[id])
		}
	}
	return b.String()
}

// DOT renders the prompt for Graphviz.
func DOT(p graph.Prompt, s *schema.Schema) string {
	var b strings.Builder
	b.WriteString("digraph workflow {\n  rankdir=LR;\n  node [shape=box, style=rounded, fontname=\"Helvetica\"];\n")
	for _, id := range TopoOrder(p) {
		fmt.Fprintf(&b, "  %q [label=\"%s\\n%s\"];\n", id, id, p[id].Class)
	}
	for _, id := range TopoOrder(p) {
		for _, k := range sortedKeys(p[id].Inputs) {
			if up, idx, ok := graph.AsLink(p[id].Inputs[k]); ok {
				fmt.Fprintf(&b, "  %q -> %q [label=%q];\n", up, id, outLabel(p, s, up, idx))
			}
		}
	}
	b.WriteString("}\n")
	return b.String()
}

func outLabel(p graph.Prompt, s *schema.Schema, up string, idx int) string {
	if s != nil && p[up] != nil {
		if c := s.Classes[p[up].Class]; c != nil && idx < len(c.Outputs) {
			return c.Outputs[idx].Type
		}
	}
	return fmt.Sprint(idx)
}
