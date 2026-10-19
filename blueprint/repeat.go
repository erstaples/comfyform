package blueprint

import (
	"fmt"
	"maps"
	"math"
	"slices"

	"github.com/erstaples/comfyform/graph"
)

// checkRepeats returns the problems with a node group's repeats, each on
// repeats[i], and one on each list setting that drives no repeat. With
// classes it also compares the types a carry joins. A group with no repeats
// has no problems here: checkGroup reports a list setting it holds.
func checkRepeats(m *Manifest, g graph.Prompt, classes Classes) Problems {
	if len(m.Repeats) == 0 {
		return nil
	}
	bindings := groupBindings(m)
	links := groupLinks(g, bindings)
	owner := repeatOwners(m, g)
	var ps Problems
	drives := map[string]int{} // list setting -> the repeat it drives
	for i, r := range m.Repeats {
		where := fmt.Sprintf("repeats[%d]", i)
		member := ps.checkRepeatNodes(where, i, r, g, owner)
		ps.checkCopies(where, i, r, m, member, drives)
		ps.checkCarries(where, r, g, bindings, member, classes)
		ps.checkLeaving(where, i, r, m, links, owner)
	}
	for i, s := range m.Settings {
		if _, ok := drives[s.Name]; s.List != nil && !ok {
			ps.add(label("settings", i, s.Name), "a list setting in a node group must drive a repeat")
		}
	}
	return ps
}

// repeatOwners maps each node of g that a repeat names to the first repeat
// that names it.
func repeatOwners(m *Manifest, g graph.Prompt) map[string]int {
	owner := map[string]int{}
	for i, r := range m.Repeats {
		for _, id := range r.Nodes {
			if _, ok := owner[id]; !ok && g[id] != nil {
				owner[id] = i
			}
		}
	}
	return owner
}

// groupLink is a link between two nodes of a node group that compile keeps:
// input of consumer links output out of node.
type groupLink struct {
	consumer, input string
	node            string
	out             int
}

// groupLinks lists the links of g in order of consumer, then input. An
// input that a port or setting fills is left out, since compile replaces
// it, and so is a link to "in" or to a node g does not have.
func groupLinks(g graph.Prompt, bindings map[string][]binding) []groupLink {
	var links []groupLink
	for _, id := range g.IDs() {
		n := g[id]
		if n == nil {
			continue
		}
		for _, k := range slices.Sorted(maps.Keys(n.Inputs)) {
			if isBound(bindings[id], k) {
				continue
			}
			if node, out, ok := graph.AsLink(n.Inputs[k]); ok && node != inLinkID && g[node] != nil {
				links = append(links, groupLink{consumer: id, input: k, node: node, out: out})
			}
		}
	}
	return links
}

func isBound(bs []binding, input string) bool {
	return slices.ContainsFunc(bs, func(b binding) bool { return b.input == input })
}

// carriedLink is what feeds the input a carry goes to: the port that fills
// it, else the link graph.json holds there. ok is false when it is neither,
// which includes an input a setting fills.
func carriedLink(g graph.Prompt, bs []binding, to Target) (port, node string, out int, ok bool) {
	for _, b := range bs {
		if b.input != to.Input {
			continue
		}
		if b.port != "" {
			return b.port, "", 0, true
		}
		return "", "", 0, false
	}
	if n := g[to.NodeID]; n != nil {
		if node, out, isLink := graph.AsLink(n.Inputs[to.Input]); isLink && node != inLinkID && g[node] != nil {
			return "", node, out, true
		}
	}
	return "", "", 0, false
}

func (m *Manifest) settingNamed(name string) *Setting {
	for i := range m.Settings {
		if m.Settings[i].Name == name {
			return &m.Settings[i]
		}
	}
	return nil
}

// checkRepeatNodes checks the nodes repeat i names and returns them as a set.
func (ps *Problems) checkRepeatNodes(where string, i int, r Repeat, g graph.Prompt, owner map[string]int) map[string]bool {
	if len(r.Nodes) == 0 {
		ps.add(where, "nodes is empty")
	}
	member := map[string]bool{}
	for _, id := range r.Nodes {
		switch {
		case member[id]:
			ps.add(where, "node %s appears twice", id)
		case g[id] == nil:
			ps.add(where, "node %q is not in %s", id, groupFile)
		case owner[id] != i:
			ps.add(where, "repeats share node %s (repeats[%d] has it too)", id, owner[id])
		}
		member[id] = true
	}
	return member
}

// checkCopies checks what makes the copies of repeat i, and records in drives
// the list settings its over names.
func (ps *Problems) checkCopies(where string, i int, r Repeat, m *Manifest, member map[string]bool, drives map[string]int) {
	switch {
	case len(r.Over) > 0 && r.Count != "":
		ps.add(where, "needs over or count, not both")
	case len(r.Over) == 0 && r.Count == "":
		ps.add(where, "needs over or count")
	case r.Count != "":
		switch s := m.settingNamed(r.Count); {
		case s == nil:
			ps.add(where, "count names %s, which is not a setting", r.Count)
		case s.List != nil:
			ps.add(where, "count setting %s is a list; a count is one int", r.Count)
		case s.Kind != KindInt:
			ps.add(where, "count setting %s is %s; a count is an int", r.Count, s.Kind)
		}
	}
	seen := map[string]bool{}
	for _, name := range r.Over {
		if seen[name] {
			ps.add(where, "over names %s twice", name)
			continue
		}
		seen[name] = true
		s := m.settingNamed(name)
		switch {
		case s == nil:
			ps.add(where, "over names %s, which is not a setting", name)
			continue
		case s.List == nil:
			ps.add(where, "over setting %s is not a list", name)
			continue
		}
		if j, ok := drives[name]; ok {
			ps.add(where, "over setting %s already drives repeats[%d]", name, j)
		} else {
			drives[name] = i
		}
		for _, t := range s.To {
			if !member[t.NodeID] {
				ps.add(where, "over setting %s has a target outside the repeat (node %s)", name, t.NodeID)
				break
			}
		}
	}
}

// checkCarries checks each carry of a repeat: both ends in it, an input that
// something outside the repeat feeds, and, with classes, matching types.
func (ps *Problems) checkCarries(where string, r Repeat, g graph.Prompt, bindings map[string][]binding,
	member map[string]bool, classes Classes) {
	carried := map[Target]bool{}
	for _, c := range r.Carry {
		to, from := c.To, c.From
		toOK := member[to.NodeID] && g[to.NodeID] != nil
		fromOK := member[from.NodeID] && g[from.NodeID] != nil
		if !member[to.NodeID] {
			ps.add(where, "carry input %s.%s is on node %s, which is not in the repeat", to.NodeID, to.Input, to.NodeID)
		}
		if !member[from.NodeID] {
			ps.add(where, "carry from output %d of node %s, which is not in the repeat", from.Output, from.NodeID)
		}
		if carried[to] {
			ps.add(where, "carry input %s.%s is carried twice", to.NodeID, to.Input)
		}
		carried[to] = true
		if toOK {
			switch port, node, _, ok := carriedLink(g, bindings[to.NodeID], to); {
			case !ok:
				ps.add(where, "carry input %s.%s is not linked", to.NodeID, to.Input)
			case port == "" && member[node]:
				ps.add(where, "carry input %s.%s links node %s, inside the repeat; the first copy takes it from outside",
					to.NodeID, to.Input, node)
			}
		}
		if classes != nil && toOK && fromOK {
			ps.checkCarryTypes(where, c, g, classes)
		}
	}
}

// checkCarryTypes compares the type of a carry's output with its input's,
// when the server has both classes.
func (ps *Problems) checkCarryTypes(where string, c Carry, g graph.Prompt, classes Classes) {
	fromClass, toClass := g[c.From.NodeID].Class, g[c.To.NodeID].Class
	fc, ok := classes.Class(fromClass)
	if !ok {
		return
	}
	tc, ok := classes.Class(toClass)
	if !ok {
		return
	}
	if c.From.Output < 0 || c.From.Output >= len(fc.Outputs) {
		ps.add(where, "carry from output %d of node %s, and %s has %s",
			c.From.Output, c.From.NodeID, fromClass, count(len(fc.Outputs), "output"))
		return
	}
	in := slices.IndexFunc(tc.Inputs, func(in InputInfo) bool { return in.Name == c.To.Input })
	if in < 0 {
		ps.add(where, "carry input %s.%s: %s has no input %s", c.To.NodeID, c.To.Input, toClass, c.To.Input)
		return
	}
	ft, tt := fc.Outputs[c.From.Output].Type, tc.Inputs[in].Type
	if ft != tt && ft != "*" && tt != "*" {
		ps.add(where, "carry types differ: %s and %s, from output %d of node %s to %s.%s",
			ft, tt, c.From.Output, c.From.NodeID, c.To.NodeID, c.To.Input)
	}
}

// checkLeaving checks what leaves repeat i: only carried outputs, to nodes or
// to output ports, never the result, and never on a path back into it.
func (ps *Problems) checkLeaving(where string, i int, r Repeat, m *Manifest, links []groupLink, owner map[string]int) {
	in := func(id string) bool {
		j, ok := owner[id]
		return ok && j == i
	}
	carried := map[Socket]bool{}
	for _, c := range r.Carry {
		carried[c.From] = true
	}
	for _, l := range links {
		if in(l.node) && !in(l.consumer) && !carried[Socket{NodeID: l.node, Output: l.out}] {
			ps.add(where, "only carried outputs leave a repeat: %s.%s links output %d of node %s",
				l.consumer, l.input, l.out, l.node)
		}
	}
	for _, o := range m.Outputs {
		if o.From != nil && in(o.From.NodeID) && !carried[*o.From] {
			ps.add(where, "only carried outputs leave a repeat: output port %s comes from output %d of node %s",
				o.Name, o.From.Output, o.From.NodeID)
		}
	}
	if m.Result != "" && in(m.Result) {
		ps.add(where, "result is inside a repeat, which makes a copy of it per item")
	}

	// Walk forward from the nodes outside the repeat that a repeated node
	// feeds; reaching a repeated node again closes a path back in.
	consumers := map[string][]string{}
	var queue []string
	seen := map[string]bool{}
	for _, l := range links {
		consumers[l.node] = append(consumers[l.node], l.consumer)
		if in(l.node) && !in(l.consumer) && !seen[l.consumer] {
			seen[l.consumer] = true
			queue = append(queue, l.consumer)
		}
	}
	for len(queue) > 0 {
		at := queue[0]
		queue = queue[1:]
		for _, next := range consumers[at] {
			if in(next) {
				ps.add(where, "a path leaves the repeat and comes back into node %s from node %s", next, at)
				return
			}
			if !seen[next] {
				seen[next] = true
				queue = append(queue, next)
			}
		}
	}
}

// unrolled is how one instance unrolls a node group's repeats: how many
// copies each makes, and where a link to a repeated node goes.
type unrolled struct {
	f        *frame
	pkg      *Package
	inputs   map[string]*ref
	bindings map[string][]binding
	owner    map[string]int // repeated node -> its repeat
	nodes    [][]string     // each repeat's nodes, in natural order
	copies   []int          // each repeat's number of copies
}

// unroll works out the copies of each repeat of pkg for the instance's
// settings: the length of the over lists, which must agree, or the count.
func (f *frame) unroll(pkg *Package, inputs map[string]*ref, settings map[string]any) (*unrolled, error) {
	m := pkg.Manifest
	u := &unrolled{
		f: f, pkg: pkg, inputs: inputs, bindings: groupBindings(m),
		owner:  repeatOwners(m, pkg.Group),
		nodes:  make([][]string, len(m.Repeats)),
		copies: make([]int, len(m.Repeats)),
	}
	for _, id := range pkg.Group.IDs() {
		if i, ok := u.owner[id]; ok {
			u.nodes[i] = append(u.nodes[i], id)
		}
	}
	for i, r := range m.Repeats {
		n, err := f.copies(r, settings)
		if err != nil {
			return nil, err
		}
		u.copies[i] = n
	}
	return u, nil
}

func (f *frame) copies(r Repeat, settings map[string]any) (int, error) {
	if r.Count == "" {
		n := 0
		for j, name := range r.Over {
			items, ok := settings[name].([]any)
			switch {
			case !ok:
				return 0, f.errorf(name, "setting %s must be a list to drive a repeat", name)
			case j == 0:
				n = len(items)
			case len(items) != n:
				return 0, f.errorf(name, "%s has %s and %s has %d", r.Over[0], count(n, "item"), name, len(items))
			}
		}
		return n, nil
	}
	v := settings[r.Count]
	if _, linked := v.(*ref); linked {
		return 0, f.errorf(r.Count, "setting %s is linked; this blueprint needs a value", r.Count)
	}
	c, ok := asNumber(v)
	switch {
	case !ok || c != math.Trunc(c):
		return 0, f.errorf(r.Count, "a count must be a whole number, got %s", show(v))
	case c < 0:
		return 0, f.errorf(r.Count, "a count must be 0 or more, got %v", c)
	case c > float64(f.b.opt.MaxNodes):
		// Every copy is at least one node, and an int conversion of a huge
		// float is not to be trusted.
		return 0, f.errorf(r.Count, "compile makes more than %d nodes", f.b.opt.MaxNodes)
	}
	return int(c), nil
}

// item names copy k of a repeat in the source map: by its first over setting
// or by its count setting.
func (r Repeat) item(k int) *Item {
	name := r.Count
	if len(r.Over) > 0 {
		name = r.Over[0]
	}
	return &Item{Setting: name, Index: k}
}

// id is the compiled id of a node of the group outside any repeat, and
// copyID that of copy k of a repeated node.
func (u *unrolled) id(node string) string { return u.f.prefix + "." + node }
func (u *unrolled) copyID(k int, node string) string {
	return fmt.Sprintf("%s.r%d.%s", u.f.prefix, k, node)
}

// linkTo is groupInputs' hook for a node of repeat i in copy k, or for a node
// outside every repeat with i = -1: a link to a node of the same repeat goes
// to the same copy, any other link resolves from outside.
func (u *unrolled) linkTo(i, k int) func(node string, out int) (*ref, error) {
	return func(node string, out int) (*ref, error) {
		if j, ok := u.owner[node]; ok && j == i {
			return &ref{node: u.copyID(k, node), out: out}, nil
		}
		return u.resolve(node, out, nil)
	}
}

// resolve is the compiled socket behind output out of group node node, seen
// from outside node's repeat: the node's own copy when it repeats nothing,
// the last copy when it is a repeat's carried output, and, when that repeat
// makes no copies, what the carry's input resolves to. A nil ref means
// nothing is behind it. seen holds the carries passed through so far.
func (u *unrolled) resolve(node string, out int, seen map[[2]int]bool) (*ref, error) {
	i, ok := u.owner[node]
	if !ok {
		return &ref{node: u.id(node), out: out}, nil
	}
	r := u.pkg.Manifest.Repeats[i]
	c := slices.IndexFunc(r.Carry, func(c Carry) bool { return c.From == Socket{NodeID: node, Output: out} })
	switch {
	case c < 0:
		return nil, u.f.errorf("", "output %d of node %s leaves repeats[%d], and no carry carries it", out, node, i)
	case u.copies[i] > 0:
		return &ref{node: u.copyID(u.copies[i]-1, node), out: out}, nil
	case seen[[2]int{i, c}]:
		return nil, u.f.errorf("", "with no copies, the carries of repeats[%d] pass through each other in a loop", i)
	}
	if seen == nil {
		seen = map[[2]int]bool{}
	}
	seen[[2]int{i, c}] = true
	to := r.Carry[c].To
	port, from, fromOut, ok := carriedLink(u.pkg.Group, u.bindings[to.NodeID], to)
	switch {
	case !ok:
		return nil, u.f.errorf("", "carry input %s.%s is not linked", to.NodeID, to.Input)
	case port != "":
		return u.inputs[port], nil
	}
	return u.resolve(from, fromOut, seen)
}

// emitCopies emits the copies of repeat i in order, each node of a copy with
// its over settings at the copy's element and its carried inputs, after the
// first copy, linked to the copy before.
func (u *unrolled) emitCopies(i int, settings map[string]any) error {
	r := u.pkg.Manifest.Repeats[i]
	for k := range u.copies[i] {
		cf := *u.f
		cf.prefix = fmt.Sprintf("%s.r%d", u.f.prefix, k)
		cf.item = r.item(k)
		values := maps.Clone(settings)
		for _, name := range r.Over {
			values[name] = settings[name].([]any)[k]
		}
		for _, id := range u.nodes[i] {
			n := u.pkg.Group[id]
			in, err := cf.groupInputs(u.pkg.Group, id, u.bindings[id], u.linkTo(i, k), u.inputs, values)
			if err != nil {
				return err
			}
			if k > 0 {
				for _, c := range r.Carry {
					if c.To.NodeID == id {
						in[c.To.Input] = (&ref{node: u.copyID(k-1, c.From.NodeID), out: c.From.Output}).link()
					}
				}
			}
			if _, err := cf.emit(id, &graph.Node{Class: n.Class, Inputs: in, Meta: n.Meta}); err != nil {
				return err
			}
		}
	}
	return nil
}
