package engine

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strings"

	"github.com/erstaples/comfyform/graph"
	"github.com/erstaples/comfyform/schema"
)

type ChangeKind int

const (
	Unchanged ChangeKind = iota
	Add
	Update
	Rerun // own inputs identical, something upstream changed
	Remove
)

type FieldDiff struct {
	Name     string
	Old, New string // "" means absent
}

type Change struct {
	ID     string
	Class  string
	Kind   ChangeKind
	Fields []FieldDiff
}

type Severity int

const (
	Warn Severity = iota
	Error
)

type Problem struct {
	Sev  Severity
	Node string
	Msg  string
}

type Plan struct {
	Changes  []Change
	Problems []Problem
	Hashes   map[string]string
}

func (p *Plan) HasErrors() bool {
	for _, pr := range p.Problems {
		if pr.Sev == Error {
			return true
		}
	}
	return false
}

func (p *Plan) Count(k ChangeKind) int {
	n := 0
	for _, c := range p.Changes {
		if c.Kind == k {
			n++
		}
	}
	return n
}

// MakePlan diffs next against the last applied prompt, validates it against
// the server schema, and checks for drift against the lockfile.
// packs is what the server reports about custom node packs, or nil.
func MakePlan(prev *State, next graph.Prompt, s *schema.Schema, lock *Lock, packs *ServerPacks) (*Plan, error) {
	hashes, err := next.Hashes()
	if err != nil {
		return nil, err
	}
	pl := &Plan{Hashes: hashes}
	var old graph.Prompt
	var oldHashes map[string]string
	if prev != nil {
		old, oldHashes = prev.Prompt, prev.Hashes
	}
	for _, id := range TopoOrder(next) {
		n := next[id]
		ch := Change{ID: id, Class: n.Class}
		o, existed := old[id]
		switch {
		case !existed:
			ch.Kind = Add
			ch.Fields = fieldDiffs(nil, n, s, next)
		case oldHashes[id] == hashes[id]:
			ch.Kind = Unchanged
		default:
			ch.Fields = fieldDiffs(o, n, s, next)
			if len(ch.Fields) == 0 && o.Class == n.Class {
				ch.Kind = Rerun
			} else {
				ch.Kind = Update
			}
		}
		pl.Changes = append(pl.Changes, ch)
	}
	for _, id := range old.IDs() {
		if _, ok := next[id]; !ok {
			pl.Changes = append(pl.Changes, Change{ID: id, Class: old[id].Class, Kind: Remove})
		}
	}
	if s != nil {
		pl.Problems = append(pl.Problems, Validate(next, s)...)
		pl.Problems = append(pl.Problems, Drift(next, s, lock, packs)...)
	}
	return pl, nil
}

func fieldDiffs(o, n *graph.Node, s *schema.Schema, p graph.Prompt) []FieldDiff {
	keys := map[string]bool{}
	if o != nil {
		for k := range o.Inputs {
			keys[k] = true
		}
	}
	for k := range n.Inputs {
		keys[k] = true
	}
	order := inputOrder(n.Class, keys, s)
	var out []FieldDiff
	if o != nil && o.Class != n.Class {
		out = append(out, FieldDiff{Name: "class_type", Old: o.Class, New: n.Class})
	}
	for _, k := range order {
		var ov, nv string
		if o != nil {
			if v, ok := o.Inputs[k]; ok {
				ov = FormatValue(v, s, p)
			}
		}
		if v, ok := n.Inputs[k]; ok {
			nv = FormatValue(v, s, p)
		}
		if ov != nv {
			out = append(out, FieldDiff{Name: k, Old: ov, New: nv})
		}
	}
	return out
}

func inputOrder(class string, keys map[string]bool, s *schema.Schema) []string {
	var order []string
	if s != nil {
		if c := s.Classes[class]; c != nil {
			for _, in := range c.Inputs {
				if keys[in.Name] {
					order = append(order, in.Name)
					delete(keys, in.Name)
				}
			}
		}
	}
	var rest []string
	for k := range keys {
		rest = append(rest, k)
	}
	sort.Strings(rest)
	return append(order, rest...)
}

// FormatValue renders an input for diffs: links as node:OUTPUT, strings quoted.
func FormatValue(v any, s *schema.Schema, p graph.Prompt) string {
	if id, idx, ok := graph.AsLink(v); ok {
		label := fmt.Sprint(idx)
		if s != nil && p[id] != nil {
			if c := s.Classes[p[id].Class]; c != nil && idx < len(c.Outputs) {
				label = c.Outputs[idx].Name
			}
		}
		return id + ":" + label
	}
	switch t := v.(type) {
	case string:
		q := fmt.Sprintf("%q", t)
		if len(q) > 60 {
			q = q[:57] + `..."`
		}
		return q
	case float64:
		if t == math.Trunc(t) && math.Abs(t) < 1e15 {
			return fmt.Sprintf("%d", int64(t))
		}
		return fmt.Sprintf("%g", t)
	default:
		b, _ := json.Marshal(t)
		return string(b)
	}
}

// TopoOrder lists node ids upstream-first; ties keep natural id order.
func TopoOrder(p graph.Prompt) []string {
	ids := p.IDs()
	indeg := map[string]int{}
	down := map[string][]string{}
	for _, id := range ids {
		indeg[id] += 0
		deps := map[string]bool{}
		for _, v := range p[id].Inputs {
			if up, _, ok := graph.AsLink(v); ok && p[up] != nil && !deps[up] {
				deps[up] = true
				indeg[id]++
				down[up] = append(down[up], id)
			}
		}
	}
	var queue, out []string
	for _, id := range ids {
		if indeg[id] == 0 {
			queue = append(queue, id)
		}
	}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		out = append(out, id)
		next := down[id]
		sort.Slice(next, func(i, j int) bool { return graph.NaturalLess(next[i], next[j]) })
		for _, d := range next {
			indeg[d]--
			if indeg[d] == 0 {
				queue = append(queue, d)
			}
		}
	}
	if len(out) < len(ids) { // cycle: append the rest so nothing is hidden
		seen := map[string]bool{}
		for _, id := range out {
			seen[id] = true
		}
		for _, id := range ids {
			if !seen[id] {
				out = append(out, id)
			}
		}
	}
	return out
}

// OutputNodes returns ids of nodes the server will execute as outputs.
func OutputNodes(p graph.Prompt, s *schema.Schema) []string {
	var out []string
	for _, id := range p.IDs() {
		if s == nil {
			continue
		}
		if c := s.Classes[p[id].Class]; c != nil && c.OutputNode {
			out = append(out, id)
		}
	}
	return out
}

// --- validation ---

func Validate(p graph.Prompt, s *schema.Schema) []Problem {
	var probs []Problem
	add := func(sev Severity, node, f string, a ...any) {
		probs = append(probs, Problem{Sev: sev, Node: node, Msg: fmt.Sprintf(f, a...)})
	}
	if len(OutputNodes(p, s)) == 0 {
		add(Error, "", "workflow has no output nodes (SaveImage, PreviewImage, ...); the server would reject it")
	}
	for _, id := range TopoOrder(p) {
		n := p[id]
		c := s.Classes[n.Class]
		if c == nil {
			add(Error, id, "class %q is not installed on the server", n.Class)
			continue
		}
		for k := range n.Inputs {
			if c.Input(k) == nil {
				add(Warn, id, "input %q is not in the %s schema (ignored by the server)", k, n.Class)
			}
		}
		for _, in := range c.Inputs {
			v, present := n.Inputs[in.Name]
			if !present || v == nil {
				if in.Required {
					add(Error, id, "required input %q is missing", in.Name)
				}
				continue
			}
			if up, idx, ok := graph.AsLink(v); ok {
				validateLink(add, p, s, id, in, up, idx)
				continue
			}
			if in.Kind == schema.KindLink {
				add(Error, id, "input %q needs a %s link, got literal %s", in.Name, in.Type, FormatValue(v, nil, nil))
				continue
			}
			validateLiteral(add, id, in, v)
		}
	}
	return probs
}

func validateLink(add func(Severity, string, string, ...any), p graph.Prompt, s *schema.Schema, id string, in schema.Input, up string, idx int) {
	un, ok := p[up]
	if !ok {
		add(Error, id, "input %q links to missing node %q", in.Name, up)
		return
	}
	uc := s.Classes[un.Class]
	if uc == nil {
		return // reported on that node
	}
	if idx < 0 || idx >= len(uc.Outputs) {
		add(Error, id, "input %q links to %s output #%d, but %s has %d outputs", in.Name, up, idx, un.Class, len(uc.Outputs))
		return
	}
	if !typesCompatible(uc.Outputs[idx].Type, in.Type) {
		add(Error, id, "input %q expects %s but %s:%s is %s", in.Name, in.Type, up, uc.Outputs[idx].Name, uc.Outputs[idx].Type)
	}
}

func typesCompatible(out, in string) bool {
	if out == "*" || in == "*" || out == in {
		return true
	}
	if in == "COMBO" && out == "COMBO" {
		return true
	}
	for _, t := range strings.Split(in, ",") {
		if strings.TrimSpace(t) == out {
			return true
		}
	}
	return false
}

func validateLiteral(add func(Severity, string, string, ...any), id string, in schema.Input, v any) {
	switch in.Kind {
	case schema.KindInt, schema.KindFloat:
		f, ok := v.(float64)
		if !ok {
			add(Error, id, "input %q expects a number, got %s", in.Name, FormatValue(v, nil, nil))
			return
		}
		if in.Kind == schema.KindInt && f != math.Trunc(f) {
			add(Error, id, "input %q expects an integer, got %g", in.Name, f)
		}
		if in.Min != nil && f < *in.Min {
			add(Error, id, "input %q = %g is below the minimum %g", in.Name, f, *in.Min)
		}
		if in.Max != nil && f > *in.Max && *in.Max < 1.8e19 {
			add(Error, id, "input %q = %g is above the maximum %g", in.Name, f, *in.Max)
		}
	case schema.KindString:
		if _, ok := v.(string); !ok {
			add(Error, id, "input %q expects a string", in.Name)
		}
	case schema.KindBool:
		if _, ok := v.(bool); !ok {
			add(Error, id, "input %q expects a boolean", in.Name)
		}
	case schema.KindCombo, schema.KindAnyCombo:
		for _, o := range in.Options {
			if fmt.Sprint(o) == fmt.Sprint(v) {
				return
			}
		}
		if in.IsFileList() {
			add(Error, id, "%s = %s not found on the server", in.Name, FormatValue(v, nil, nil))
			return
		}
		opts := make([]string, 0, len(in.Options))
		for _, o := range in.Options {
			opts = append(opts, fmt.Sprint(o))
		}
		add(Error, id, "%s = %s is not one of: %s", in.Name, FormatValue(v, nil, nil), truncate(strings.Join(opts, ", "), 200))
	}
}

// Drift compares the classes this prompt uses, and the packs they come from,
// with the lockfile. Packs are skipped when the server reports none.
func Drift(p graph.Prompt, s *schema.Schema, l *Lock, packs *ServerPacks) []Problem {
	if l == nil {
		return nil
	}
	var probs []Problem
	seen := map[string]bool{}
	for _, id := range p.IDs() {
		cls := p[id].Class
		if seen[cls] {
			continue
		}
		seen[cls] = true
		c := s.Classes[cls]
		if c == nil {
			continue
		}
		lc, ok := l.Classes[cls]
		switch {
		case !ok:
			probs = append(probs, Problem{Sev: Warn, Msg: fmt.Sprintf("drift: %s is not in %s (run comfy lock)", cls, LockFile)})
		case lc.Schema != c.Hash():
			probs = append(probs, Problem{Sev: Warn, Msg: fmt.Sprintf("drift: %s (%s) changed its interface since lock; outputs may differ", cls, c.Module)})
		case lc.Module != c.Module:
			probs = append(probs, Problem{Sev: Warn, Msg: fmt.Sprintf("drift: %s now comes from %s (locked: %s)", cls, c.Module, lc.Module)})
		}
		if dir := PackDir(c.Module); dir != "" && !seen["pack:"+dir] {
			seen["pack:"+dir] = true
			probs = append(probs, packDrift(dir, l, packs)...)
		}
	}
	return probs
}

func packDrift(dir string, l *Lock, packs *ServerPacks) []Problem {
	if packs == nil {
		return nil
	}
	warn := func(format string, args ...any) []Problem {
		return []Problem{{Sev: Warn, Msg: "drift: " + fmt.Sprintf(format, args...)}}
	}
	lp, ok := l.Packs[dir]
	if !ok {
		return warn("pack %s is not in %s (run comfy lock)", dir, LockFile)
	}
	pi, ok := packs.Packs[dir]
	if !ok || lp.Source == Unpinned {
		return nil
	}
	if src := pi.Source(); src != lp.Source {
		return warn("pack %s is now %s (locked: %s)", dir, src, lp.Source)
	}
	if lp.Content != "" && pi.Content != "" && pi.Content != lp.Content {
		return warn("pack %s has local changes since lock (same %s, different files)", dir, lp.Source)
	}
	return nil
}

// --- rendering ---

type Printer struct {
	W     io.Writer
	Color bool
}

func NewPrinter(w io.Writer) *Printer {
	color := false
	if f, ok := w.(*os.File); ok && os.Getenv("NO_COLOR") == "" {
		if fi, err := f.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
			color = true
		}
	}
	return &Printer{W: w, Color: color}
}

func (pr *Printer) c(code, s string) string {
	if !pr.Color {
		return s
	}
	return "\033[" + code + "m" + s + "\033[0m"
}

func (pr *Printer) Plan(pl *Plan, verbose bool) {
	w := pr.W
	for _, ch := range pl.Changes {
		switch ch.Kind {
		case Add:
			fmt.Fprintf(w, "  %s %-28s %s\n", pr.c("32", "+"), ch.ID, pr.c("2", ch.Class))
			if verbose {
				for _, f := range ch.Fields {
					fmt.Fprintf(w, "      %s = %s\n", f.Name, f.New)
				}
			}
		case Update:
			fmt.Fprintf(w, "  %s %-28s %s\n", pr.c("33", "~"), ch.ID, pr.c("2", ch.Class))
			for _, f := range ch.Fields {
				switch {
				case f.Old == "":
					fmt.Fprintf(w, "      %s: %s\n", f.Name, pr.c("32", "+ "+f.New))
				case f.New == "":
					fmt.Fprintf(w, "      %s: %s\n", f.Name, pr.c("31", "- "+f.Old))
				default:
					fmt.Fprintf(w, "      %s: %s → %s\n", f.Name, pr.c("31", f.Old), pr.c("32", f.New))
				}
			}
		case Rerun:
			fmt.Fprintf(w, "  %s %-28s %s\n", pr.c("36", "↻"), ch.ID, pr.c("2", "upstream changed"))
		case Remove:
			fmt.Fprintf(w, "  %s %-28s %s\n", pr.c("31", "-"), ch.ID, pr.c("2", ch.Class))
		case Unchanged:
			if verbose {
				fmt.Fprintf(w, "  %s %-28s %s\n", pr.c("2", "="), ch.ID, pr.c("2", "unchanged"))
			}
		}
	}
	if n := pl.Count(Unchanged); n > 0 && !verbose {
		fmt.Fprintf(w, "  %s\n", pr.c("2", fmt.Sprintf("= %d unchanged", n)))
	}
	if len(pl.Problems) > 0 {
		fmt.Fprintln(w)
	}
	for _, p := range pl.Problems {
		tag := pr.c("33", "warning")
		if p.Sev == Error {
			tag = pr.c("31", "error")
		}
		if p.Node != "" {
			fmt.Fprintf(w, "  %s %s: %s\n", tag, p.Node, p.Msg)
		} else {
			fmt.Fprintf(w, "  %s %s\n", tag, p.Msg)
		}
	}
	fmt.Fprintf(w, "\nPlan: %d to add, %d to change, %d to rerun, %d to remove, %d unchanged.\n",
		pl.Count(Add), pl.Count(Update), pl.Count(Rerun), pl.Count(Remove), pl.Count(Unchanged))
}
