package blueprint

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/erstaples/comfyform/graph"
)

// Resolver finds the package a digest names. Compile asks it once for each
// instance in the graph, and once per compile for each digest that a nested
// instance's uses pins.
type Resolver interface {
	Package(digest string) (*Package, error)
}

// Options configure Compile. A limit left at zero takes its default. Compile
// only reads Options, so one value can serve compiles that run at once.
type Options struct {
	Classes  Classes
	Packages Resolver
	MaxNodes int           // nodes and nested instances one compile may make; 0 means 2000
	MaxDepth int           // uses nesting below an instance; 0 means 8
	Steps    uint64        // Starlark steps per instance; 0 means 10,000,000
	Timeout  time.Duration // Starlark time per instance; 0 means 2 seconds
}

const (
	defaultMaxNodes = 2000
	defaultMaxDepth = 8
	defaultSteps    = 10_000_000
	defaultTimeout  = 2 * time.Second
)

func (o Options) withDefaults() Options {
	if o.MaxNodes <= 0 {
		o.MaxNodes = defaultMaxNodes
	}
	if o.MaxDepth <= 0 {
		o.MaxDepth = defaultMaxDepth
	}
	if o.Steps == 0 {
		o.Steps = defaultSteps
	}
	if o.Timeout <= 0 {
		o.Timeout = defaultTimeout
	}
	return o
}

// Result is a compiled graph and where its nodes came from.
type Result struct {
	// Prompt is a graph a stock ComfyUI server runs, normalized: its links
	// are []any{id, float64}, the form JSON decodes them in.
	Prompt graph.Prompt
	// Sources has one entry for each node compile made, by compiled id.
	// Nodes copied from the input graph have none.
	Sources map[string]Source
	// Results maps each instance of the input graph whose blueprint has a
	// result to the compiled id of that result node.
	Results map[string]string
}

// Source is where a compiled node came from.
type Source struct {
	// Instances is the chain of instances from the input graph inward:
	// ["14"], or ["14", "base"] for a node of an instance nested in 14.
	Instances []string `json:"instances"`
	// Blueprint is the innermost instance's id@version.
	Blueprint string `json:"blueprint"`
	// Item is the list element or repeat copy the node was made for, as an
	// element of a list setting of the instance in the graph, Instances[0]:
	// a copy of its repeat, or its program's tag. A nested blueprint's own
	// copies and tags do not show here; its nodes carry the item of the
	// outer instance they were made in.
	Item *Item `json:"item,omitempty"`
}

// Item is one element of a list setting, or one copy of a repeat, named by
// the setting that makes the copies.
type Item struct {
	Setting string `json:"setting"`
	Index   int    `json:"index"`
}

// Error is a compile failure, placed as precisely as compile knows: the
// instance chain, the innermost blueprint, and the setting and item it is
// about.
type Error struct {
	Instances []string
	Blueprint string
	Setting   string
	Item      *Item
	Msg       string
}

// Error reads "14 (test/hires@1.0.0): setting denoise: must be at most 1".
// A Msg that already names its setting ("setting seed is linked", or
// "main.star:3: setting seed is linked") is not prefixed again.
func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString(strings.Join(e.Instances, "."))
	if e.Blueprint != "" {
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString("(" + e.Blueprint + ")")
	}
	if e.Item != nil {
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		fmt.Fprintf(&b, "%s[%d]", e.Item.Setting, e.Item.Index)
	}
	if b.Len() > 0 {
		b.WriteString(": ")
	}
	if e.Setting != "" && !strings.Contains(e.Msg, "setting "+e.Setting+" ") {
		b.WriteString("setting " + e.Setting + ": ")
	}
	b.WriteString(e.Msg)
	return b.String()
}

// Compile expands every blueprint instance in p into plain nodes, giving a
// graph a stock ComfyUI server runs. Each instance's nodes take ids under the
// instance's own id ("21.1"), and every link to an instance output is
// replaced by the socket that output resolved to.
//
// A graph with no instances comes back as p itself. Otherwise the result is a
// new, normalized graph; Compile never changes p or the packages, and makes
// no calls but to opt.Packages. Failures about an instance are *Error.
func Compile(p graph.Prompt, opt Options) (*Result, error) {
	ids := p.IDs()
	var instIDs []string
	for _, id := range ids {
		if isInstance(p[id]) {
			instIDs = append(instIDs, id)
		}
	}
	if len(instIDs) == 0 {
		return &Result{Prompt: p}, nil
	}
	if opt.Packages == nil {
		return nil, errors.New("blueprint: the graph has blueprint instances and Options.Packages is nil")
	}

	insts := instances{}
	for _, id := range instIDs {
		in, err := resolveInstance(id, p[id], opt.Packages)
		if err != nil {
			return nil, err
		}
		insts[id] = in
	}
	order, err := insts.order(instIDs)
	if err != nil {
		return nil, err
	}

	b := &build{opt: opt.withDefaults(), outer: p, out: graph.Prompt{}, sources: map[string]Source{}, packages: map[string]*Package{}}
	for _, in := range order {
		if err := b.instance(in, insts); err != nil {
			return nil, err
		}
	}
	for _, id := range ids {
		if insts[id] == nil {
			if err := b.copyNode(id, p[id], insts); err != nil {
				return nil, err
			}
		}
	}

	results := map[string]string{}
	for _, id := range instIDs {
		in := insts[id]
		name := in.pkg.Manifest.Result
		if name == "" {
			continue
		}
		cid := id + "." + name
		if _, ok := b.sources[cid]; !ok {
			return nil, in.errorf("", "result %s was not built", name)
		}
		results[id] = cid
	}

	out, err := b.out.Normalize()
	if err != nil {
		return nil, fmt.Errorf("blueprint: encode the compiled graph: %w", err)
	}
	return &Result{Prompt: out, Sources: b.sources, Results: results}, nil
}

// ref is a compiled socket: output out of compiled node node. A nil *ref
// means nothing is behind a socket, as for an optional port with no link.
type ref struct {
	node string
	out  int
}

func (r *ref) link() graph.AnyRef { return graph.Link(r.node, r.out) }

// build is the output of one Compile call as it grows.
type build struct {
	opt      Options      // with the defaults filled in
	outer    graph.Prompt // the graph being compiled; read only
	out      graph.Prompt // the compiled graph
	sources  map[string]Source
	nodes    int                 // nodes emitted and instances nested, counted against opt.MaxNodes
	packages map[string]*Package // the packages nested instances use, by digest
}

// frame is where nodes are being emitted: under which id prefix, and with
// what Source.
type frame struct {
	b         *build
	prefix    string            // a node named x is emitted as prefix.x
	chain     []string          // Source.Instances
	blueprint string            // Source.Blueprint
	item      *Item             // the item errors name: this blueprint's tag, else the one it was made for
	source    *Item             // Source.Item: the item of the instance in the graph
	depth     int               // uses levels below the outer instance
	requires  map[string]string // the classes this blueprint may emit
}

func (f *frame) errorf(setting, format string, args ...any) *Error {
	return &Error{
		Instances: slices.Clone(f.chain),
		Blueprint: f.blueprint,
		Setting:   setting,
		Item:      f.item.clone(),
		Msg:       fmt.Sprintf(format, args...),
	}
}

// tag makes f's nodes for item it. Errors name it whatever the depth; the
// source map names it only in the instance in the graph, whose settings
// Source.Item refers to.
func (f *frame) tag(it *Item) {
	f.item = it
	if f.depth == 0 {
		f.source = it
	}
}

// take counts one more node or nested instance against opt.MaxNodes. An
// instance counts even when it makes no node, so instances that fan out to
// nothing cannot run without bound.
func (f *frame) take() error {
	if f.b.nodes >= f.b.opt.MaxNodes {
		return f.errorf("", "compile makes more than %d nodes", f.b.opt.MaxNodes)
	}
	f.b.nodes++
	return nil
}

func (it *Item) clone() *Item {
	if it == nil {
		return nil
	}
	c := *it
	return &c
}

// emit adds a node to the compiled graph as f.prefix + "." + name and returns
// that id. It refuses a class the blueprint does not require, an id the input
// graph or an earlier emit already has, and a node past opt.MaxNodes.
func (f *frame) emit(name string, n *graph.Node) (id string, err error) {
	id = f.prefix + "." + name
	if _, ok := f.requires[n.Class]; !ok {
		return "", f.errorf("", "%s is not in requires.classes", n.Class)
	}
	if _, ok := f.b.outer[id]; ok {
		return "", f.errorf("", "%s is already a node in the graph", id)
	}
	if _, ok := f.b.out[id]; ok {
		return "", f.errorf("", "duplicate node id %s", id)
	}
	if err := f.take(); err != nil {
		return "", err
	}
	f.b.out[id] = n
	f.b.sources[id] = Source{Instances: slices.Clone(f.chain), Blueprint: f.blueprint, Item: f.source.clone()}
	return id, nil
}

// expand emits the nodes of an instance of pkg, given the compiled sockets
// behind its input ports and its settings' values (a *ref for a linked
// setting). It returns the compiled socket behind each output port, by name.
func (f *frame) expand(pkg *Package, inputs map[string]*ref, settings map[string]any) (map[string]*ref, error) {
	if pkg.Form == FormProgram {
		return f.expandProgram(pkg, inputs, settings)
	}
	return f.expandGroup(pkg, inputs, settings)
}

// isInstance reports whether n is a blueprint instance.
func isInstance(n *graph.Node) bool { return n != nil && strings.HasPrefix(n.Class, classPrefix) }

// nest expands an instance of the blueprint id that uses pins, named name in
// f: a node group's node id, or a program's ctx.use name. Its nodes go out
// under f.prefix + "." + name, its sources and errors carry f's chain with
// name added, and it is one level deeper than f. It counts against
// opt.MaxNodes like a node. Its nodes' Source.Item is f's; its errors name
// f's item unless it tags its own. version, when not "", is the version the
// instance's class names. fill gives what feeds the instance's inputs once
// its manifest is known; nest checks that against the manifest as it checks
// an instance in the graph. nest returns the instance's manifest and the
// compiled socket behind each output port.
func (f *frame) nest(uses map[string]Use, id, version, name string,
	fill func(nf *frame, m *Manifest) (feed, error)) (*Manifest, map[string]*ref, error) {
	use, ok := uses[id]
	if !ok {
		return nil, nil, f.errorf("", "%s is not in uses", id)
	}
	if version != "" && version != use.Version {
		return nil, nil, f.errorf("", "node %s: uses pins %s at %s, not %s", name, id, use.Version, version)
	}
	nf := &frame{
		b:         f.b,
		prefix:    f.prefix + "." + name,
		chain:     append(slices.Clone(f.chain), name),
		blueprint: id + "@" + use.Version,
		item:      f.item,
		source:    f.source,
		depth:     f.depth + 1,
	}
	if limit := f.b.opt.MaxDepth; nf.depth > limit {
		return nil, nil, nf.errorf("", "nested deeper than %d", limit)
	}
	if err := nf.take(); err != nil {
		return nil, nil, err
	}
	pkg, ok := f.b.packages[use.Digest]
	if !ok {
		var problem string
		if pkg, problem = fetch(f.b.opt.Packages, use.Digest); problem != "" {
			return nil, nil, nf.errorf("", "%s", problem)
		}
		f.b.packages[use.Digest] = pkg
	}
	m := pkg.Manifest
	if m.ID != id || m.Version != use.Version {
		got := m.ID + "@" + m.Version
		if m.ID == id {
			got = "@" + m.Version
		}
		return nil, nil, nf.errorf("", "uses says %s@%s, digest is %s", id, use.Version, got)
	}
	nf.requires = m.Requires.Classes
	fd, err := fill(nf, m)
	if err != nil {
		return nil, nil, err
	}
	inputs, settings, err := nf.bind(m, fd)
	if err != nil {
		return nil, nil, err
	}
	outs, err := nf.expand(pkg, inputs, settings)
	if err != nil {
		return nil, nil, err
	}
	return m, outs, nil
}

// instance is a blueprint instance of the graph being compiled.
type instance struct {
	id   string
	node *graph.Node
	pkg  *Package
	outs map[string]*ref // by output port name, once the instance is compiled
}

func (in *instance) blueprint() string { return in.pkg.Manifest.ID + "@" + in.pkg.Manifest.Version }

func (in *instance) errorf(setting, format string, args ...any) *Error {
	return &Error{Instances: []string{in.id}, Blueprint: in.blueprint(), Setting: setting, Msg: fmt.Sprintf(format, args...)}
}

// instances are the instances of the graph being compiled, by id.
type instances map[string]*instance

// resolveInstance finds the package of instance id and checks that it is the
// blueprint the node's class names.
func resolveInstance(id string, n *graph.Node, packages Resolver) (*instance, error) {
	bid, version, ok := ParseClass(n.Class)
	if !ok {
		return nil, &Error{Instances: []string{id}, Msg: fmt.Sprintf("%q is not a well-formed blueprint class", n.Class)}
	}
	fail := func(format string, args ...any) error {
		return &Error{Instances: []string{id}, Blueprint: bid + "@" + version, Msg: fmt.Sprintf(format, args...)}
	}
	var digest string
	if n.Meta != nil {
		digest = n.Meta.Blueprint
	}
	if digest == "" {
		return nil, fail("has no blueprint digest")
	}
	pkg, problem := fetch(packages, digest)
	if problem != "" {
		return nil, fail("%s", problem)
	}
	switch m := pkg.Manifest; {
	case m.ID != bid:
		return nil, fail("digest is %s@%s, class says %s@%s", m.ID, m.Version, bid, version)
	case m.Version != version:
		return nil, fail("digest is %s@%s, class says @%s", m.ID, m.Version, version)
	}
	return &instance{id: id, node: n, pkg: pkg}, nil
}

// fetch asks packages for the package of digest, and says what is wrong
// when the answer is not that package.
func fetch(packages Resolver, digest string) (*Package, string) {
	pkg, err := packages.Package(digest)
	switch {
	case err != nil:
		return nil, fmt.Sprintf("cannot load %s: %v", digest, err)
	case pkg == nil || pkg.Manifest == nil:
		return nil, "no package for " + digest
	case pkg.Digest != digest:
		return nil, fmt.Sprintf("the resolver served %s when asked for %s", pkg.Digest, digest)
	}
	return pkg, ""
}

// deps are the instances that in's ports and non-list settings link to, in
// manifest order. A list setting's value never reads as a link.
func (insts instances) deps(in *instance) []string {
	m := in.pkg.Manifest
	var names []string
	for _, p := range m.Inputs {
		names = append(names, p.Name)
	}
	for _, s := range m.Settings {
		if s.List == nil {
			names = append(names, s.Name)
		}
	}
	var deps []string
	for _, name := range names {
		if node, _, ok := graph.AsLink(in.node.Inputs[name]); ok && insts[node] != nil {
			deps = append(deps, node)
		}
	}
	return deps
}

// order puts the instances so that each comes after the instances it links
// to, otherwise in the order of ids.
func (insts instances) order(ids []string) ([]*instance, error) {
	const (
		visiting = 1
		done     = 2
	)
	state := map[string]int{}
	var order []*instance
	var path []string
	var visit func(id string) error
	visit = func(id string) error {
		switch state[id] {
		case done:
			return nil
		case visiting:
			cycle := append(slices.Clone(path[slices.Index(path, id):]), id)
			return insts[id].errorf("", "instances link in a cycle: %s", strings.Join(cycle, " -> "))
		}
		state[id] = visiting
		path = append(path, id)
		for _, dep := range insts.deps(insts[id]) {
			if err := visit(dep); err != nil {
				return err
			}
		}
		path = path[:len(path)-1]
		state[id] = done
		order = append(order, insts[id])
		return nil
	}
	for _, id := range ids {
		if err := visit(id); err != nil {
			return nil, err
		}
	}
	return order, nil
}

// resolve is the compiled socket behind a link from input of node consumer
// to output idx of node: that node itself, unless it is an instance, whose
// output port has resolved to a socket already. A nil ref means the instance
// has nothing behind that output.
func (insts instances) resolve(consumer, input, node string, idx int) (*ref, error) {
	src := insts[node]
	if src == nil {
		return &ref{node: node, out: idx}, nil
	}
	outs := src.pkg.Manifest.Outputs
	if idx < 0 || idx >= len(outs) {
		return nil, src.errorf("", "node %s input %s links output %d, and the blueprint has %s",
			consumer, input, idx, count(len(outs), "output"))
	}
	return src.outs[outs[idx].Name], nil
}

// instance compiles one instance: it reads the instance's ports and settings
// and expands its package under the instance's id.
func (b *build) instance(in *instance, insts instances) error {
	m := in.pkg.Manifest
	f := &frame{b: b, prefix: in.id, chain: []string{in.id}, blueprint: in.blueprint(), requires: m.Requires.Classes}
	inputs, settings, err := f.bind(m, feed{
		values: in.node.Inputs,
		resolve: func(input, node string, idx int) (*ref, error) {
			return insts.resolve(in.id, input, node, idx)
		},
	})
	if err != nil {
		return err
	}
	outs, err := f.expand(in.pkg, inputs, settings)
	if err != nil {
		return err
	}
	in.outs = outs
	return nil
}

// feed is what fills the inputs of an instance: a value for each input,
// either as a graph holds it or as a *ref compile has resolved already (nil
// for nothing behind it, which reads as an input left out), and how a link
// in a value resolves.
type feed struct {
	values  map[string]any
	resolve func(input, node string, idx int) (*ref, error)
}

// bind reads the ports and settings of an instance of m from fd, checking
// them against m: the compiled socket behind each input port, and each
// setting's value. Every instance does this, in the graph or nested.
func (f *frame) bind(m *Manifest, fd feed) (map[string]*ref, map[string]any, error) {
	known := map[string]bool{}
	for _, p := range m.Inputs {
		known[p.Name] = true
	}
	for _, s := range m.Settings {
		known[s.Name] = true
	}
	for _, k := range slices.Sorted(maps.Keys(fd.values)) {
		if !known[k] {
			return nil, nil, f.errorf("", "input %s is not a port or setting of the blueprint", k)
		}
	}

	inputs := make(map[string]*ref, len(m.Inputs))
	for _, p := range m.Inputs {
		r, err := f.port(p, fd)
		if err != nil {
			return nil, nil, err
		}
		inputs[p.Name] = r
	}
	settings := make(map[string]any, len(m.Settings))
	for _, s := range m.Settings {
		v, err := f.setting(s, fd)
		if err != nil {
			return nil, nil, err
		}
		settings[s.Name] = v
	}
	return inputs, settings, nil
}

// port is the compiled socket behind an input port, or nil for an optional
// port with nothing behind it.
func (f *frame) port(p Port, fd feed) (*ref, error) {
	v, ok := fd.values[p.Name]
	if r, isRef := v.(*ref); isRef {
		if r != nil {
			return r, nil
		}
		ok = false
	}
	if !ok {
		if p.Optional {
			return nil, nil
		}
		return nil, f.errorf("", "input %s is not connected", p.Name)
	}
	node, idx, isLink := graph.AsLink(v)
	if !isLink {
		return nil, f.errorf("", "input %s must be a link", p.Name)
	}
	r, err := fd.resolve(p.Name, node, idx)
	if err != nil {
		return nil, err
	}
	if r == nil && !p.Optional {
		return nil, f.errorf("", "input %s is not connected: output %d of %s has nothing behind it", p.Name, idx, node)
	}
	return r, nil
}

// setting is the value of a setting: what the instance holds, else the
// manifest's default, checked against the manifest. A linked setting is the
// *ref behind the link.
func (f *frame) setting(s Setting, fd feed) (any, error) {
	v, ok := fd.values[s.Name]
	if r, isRef := v.(*ref); isRef {
		switch {
		case r == nil:
			ok = false
		case s.List != nil || !s.IsLinkable():
			return nil, f.errorf(s.Name, "setting %s is not linkable", s.Name)
		default:
			return r, nil
		}
	}
	if !ok {
		v = s.defaultValue()
	} else if s.List == nil {
		if node, idx, isLink := graph.AsLink(v); isLink {
			if !s.IsLinkable() {
				return nil, f.errorf(s.Name, "setting %s is not linkable", s.Name)
			}
			r, err := fd.resolve(s.Name, node, idx)
			if err != nil {
				return nil, err
			}
			if r == nil {
				return nil, f.errorf(s.Name, "setting %s is linked to output %d of %s, which has nothing behind it", s.Name, idx, node)
			}
			return r, nil
		}
	}
	if msg := settingProblem(s, v); msg != "" {
		return nil, f.errorf(s.Name, "%s", msg)
	}
	return v, nil
}

// copyNode copies a node of the input graph that is not an instance into the
// compiled graph, pointing its links to instance outputs at what they
// resolved to.
func (b *build) copyNode(id string, n *graph.Node, insts instances) error {
	if n == nil {
		return &Error{Msg: fmt.Sprintf("node %s is null", id)}
	}
	in := make(map[string]any, len(n.Inputs))
	for _, k := range slices.Sorted(maps.Keys(n.Inputs)) {
		v := n.Inputs[k]
		if node, idx, ok := graph.AsLink(v); ok && insts[node] != nil {
			r, err := insts.resolve(id, k, node, idx)
			if err != nil {
				return err
			}
			if r == nil {
				return insts[node].errorf("", "node %s input %s links output %d, which has nothing behind it", id, k, idx)
			}
			v = r.link()
		}
		in[k] = v
	}
	b.out[id] = &graph.Node{Class: n.Class, Inputs: in, Meta: n.Meta}
	return nil
}

// defaultValue is what a setting takes when an instance leaves it out: its
// default, else an empty list, else an empty image name.
func (s Setting) defaultValue() any {
	switch {
	case s.Default != nil:
		return s.Default
	case s.List != nil:
		return []any{}
	case s.Kind == KindImage:
		return ""
	}
	return nil
}

// settingProblem says why v is not a value setting s takes, in words that
// follow "setting <name>: ", or returns "" when it is one.
func settingProblem(s Setting, v any) string {
	if s.List == nil {
		return s.elementProblem(v)
	}
	items, ok := v.([]any)
	if !ok {
		return "must be a list, got " + show(v)
	}
	switch {
	case len(items) < s.List.Min:
		return fmt.Sprintf("must have at least %s, has %d", count(s.List.Min, "item"), len(items))
	case len(items) > s.List.Max:
		return fmt.Sprintf("must have at most %s, has %d", count(s.List.Max, "item"), len(items))
	}
	for i, item := range items {
		if msg := s.elementProblem(item); msg != "" {
			return fmt.Sprintf("item %d %s", i, msg)
		}
	}
	return ""
}

// elementProblem is settingProblem for one value of the setting's kind: the
// whole value of a plain setting, one element of a list.
func (s Setting) elementProblem(v any) string {
	switch s.Kind {
	case KindInt, KindFloat, KindSeed:
		f, ok := asNumber(v)
		if !ok {
			return "must be a number, got " + show(v)
		}
		whole := f == math.Trunc(f)
		switch {
		case s.Kind == KindSeed && (f < 0 || !whole):
			return fmt.Sprintf("must be a whole number of 0 or more, got %v", f)
		case s.Kind == KindInt && !whole:
			return fmt.Sprintf("must be a whole number, got %v", f)
		case s.Min != nil && f < *s.Min:
			return fmt.Sprintf("must be at least %v", *s.Min)
		case s.Max != nil && f > *s.Max:
			return fmt.Sprintf("must be at most %v", *s.Max)
		}
	case KindString, KindImage:
		if _, ok := v.(string); !ok {
			return "must be a string, got " + show(v)
		}
	case KindBool:
		if _, ok := v.(bool); !ok {
			return "must be true or false, got " + show(v)
		}
	case KindCombo:
		str, ok := v.(string)
		if !ok {
			return "must be a string, got " + show(v)
		}
		// A models source's files are checked against the server elsewhere.
		if s.Source == nil && !slices.Contains(s.Options, str) {
			return fmt.Sprintf("must be one of %s, got %q", quoteAll(s.Options), str)
		}
	}
	return ""
}

// show writes a value the way the graph holds it.
func show(v any) string {
	if b, err := json.Marshal(v); err == nil {
		return string(b)
	}
	return fmt.Sprint(v)
}

func quoteAll(ss []string) string {
	q := make([]string, len(ss))
	for i, s := range ss {
		q[i] = fmt.Sprintf("%q", s)
	}
	return strings.Join(q, ", ")
}

// count is "1 output", "2 outputs".
func count(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
