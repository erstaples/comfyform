package blueprint

import (
	"errors"
	"fmt"
	"maps"
	"math"
	"math/big"
	"regexp"
	"slices"
	"time"

	starlarkjson "go.starlark.net/lib/json"
	"go.starlark.net/resolve"
	"go.starlark.net/starlark"
	"go.starlark.net/starlarkstruct"
	"go.starlark.net/syntax"

	"github.com/erstaples/comfyform/graph"
)

// partName is a name ctx.node and ctx.child accept: one part of a compiled id.
var partName = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// predeclared is all a program can name besides starlark-go's universe
// (len, range, fail and the rest) and the arguments build gets. There is no
// load, time, randomness or I/O.
var predeclared = starlark.StringDict{
	"struct": starlark.NewBuiltin("struct", starlarkstruct.Make),
	"json":   jsonModule(),
}

// maxJSONDepth is how deeply json.decode lets arrays and objects nest.
// starlark-go's decoder recurses once per level and has no limit of its own,
// so input nested deeply enough overflows the Go stack, a fatal error that
// kills the whole process before the step limit or the timer can act.
const maxJSONDepth = 1000

// jsonModule is starlark-go's json module with a decode that first refuses
// input nested deeper than maxJSONDepth. Its other members are its own.
func jsonModule() *starlarkstruct.Module {
	members := maps.Clone(starlarkjson.Module.Members)
	decode := members["decode"].(*starlark.Builtin)
	members["decode"] = starlark.NewBuiltin(decode.Name(),
		func(thread *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
			if len(args) > 0 {
				if s, ok := starlark.AsString(args[0]); ok && jsonDepth(s) > maxJSONDepth {
					return nil, fmt.Errorf("%s: input is nested deeper than %d", decode.Name(), maxJSONDepth)
				}
			}
			return decode.CallInternal(thread, args, kwargs)
		})
	return &starlarkstruct.Module{Name: starlarkjson.Module.Name, Members: members}
}

// jsonDepth is how deeply the arrays and objects of JSON text s nest, not
// counting brackets inside string literals. It stops counting once past
// maxJSONDepth, and leaves malformed text for the decoder to report.
func jsonDepth(s string) int {
	depth, deepest := 0, 0
	inString, escaped := false, false
	for i := 0; i < len(s) && deepest <= maxJSONDepth; i++ {
		c := s[i]
		switch {
		case escaped:
			escaped = false
		case inString:
			switch c {
			case '\\':
				escaped = true
			case '"':
				inString = false
			}
		case c == '"':
			inString = true
		case c == '[' || c == '{':
			depth++
			deepest = max(deepest, depth)
		case c == ']' || c == '}':
			depth--
		}
	}
	return deepest
}

// expandProgram runs main.star's build(ctx, inputs, settings) in a sandbox on
// a thread of its own, with the step limit and cancel timer of opt, and
// returns the compiled socket behind each output port. ctx emits under the
// frame's prefix; inputs holds a socket reference for each port, or None for
// an optional port with nothing behind it; settings holds each setting's
// value, or a socket reference for a linked one.
func (f *frame) expandProgram(pkg *Package, inputs map[string]*ref, settings map[string]any) (map[string]*ref, error) {
	if f.b.opt.Classes == nil {
		return nil, f.errorf("", "compiling a program needs Options.Classes")
	}
	m := pkg.Manifest
	ins := make(starlark.StringDict, len(m.Inputs))
	for _, p := range m.Inputs {
		if r := inputs[p.Name]; r != nil {
			ins[p.Name] = &socket{f: f, r: r, typ: p.Type}
		} else {
			ins[p.Name] = starlark.None
		}
	}
	sets := make(starlark.StringDict, len(m.Settings))
	for _, s := range m.Settings {
		v, err := f.settingValue(s, settings[s.Name])
		if err != nil {
			return nil, err
		}
		sets[s.Name] = v
	}

	thread := &starlark.Thread{Name: f.prefix, Print: func(*starlark.Thread, string) {}}
	thread.SetMaxExecutionSteps(f.b.opt.Steps)
	timer := time.AfterFunc(f.b.opt.Timeout, func() { thread.Cancel("timeout") })
	defer timer.Stop()

	globals, err := starlark.ExecFileOptions(&syntax.FileOptions{}, thread, programFile, pkg.Program, predeclared)
	if err != nil {
		return nil, f.programError(err)
	}
	build, ok := globals["build"].(*starlark.Function)
	if !ok {
		return nil, f.errorf("", "%s defines no function build", programFile)
	}
	ctx := &hostCtx{h: &host{m: m, classes: f.b.opt.Classes, settings: settings}, f: f}
	args := starlark.Tuple{
		ctx,
		starlarkstruct.FromStringDict(starlarkstruct.Default, ins),
		starlarkstruct.FromStringDict(starlarkstruct.Default, sets),
	}
	v, err := starlark.Call(thread, build, args, nil)
	if err != nil {
		return nil, f.programError(err)
	}
	return f.programOutputs(m, v)
}

// programOutputs reads what build returned: a dict from each output port's
// name to a socket reference of a type the port takes, or None for nothing
// behind it.
func (f *frame) programOutputs(m *Manifest, v starlark.Value) (map[string]*ref, error) {
	d, ok := v.(*starlark.Dict)
	if !ok {
		return nil, f.errorf("", "build returned %s, not a dict", v.Type())
	}
	outs := make(map[string]*ref, len(m.Outputs))
	for _, kv := range d.Items() {
		name, ok := starlark.AsString(kv[0])
		if !ok {
			return nil, f.errorf("", "build returned a %s key; output names are strings", kv[0].Type())
		}
		i := slices.IndexFunc(m.Outputs, func(p Port) bool { return p.Name == name })
		if i < 0 {
			return nil, f.errorf("", "build returned unknown output %s", name)
		}
		switch s := kv[1].(type) {
		case starlark.NoneType:
			outs[name] = nil
		case *socket:
			if s.setting != "" {
				return nil, s.misuse()
			}
			if want := m.Outputs[i].Type; !typesMatch(s.typ, want) {
				return nil, f.errorf("", "build returned %s for output %s, which is %s", s.typ, name, want)
			}
			outs[name] = s.r
		default:
			return nil, f.errorf("", "build returned %s for output %s, which needs a socket reference", kv[1].Type(), name)
		}
	}
	for _, p := range m.Outputs {
		if _, ok := outs[p.Name]; !ok {
			return nil, f.errorf("", "build returned no output %s", p.Name)
		}
	}
	return outs, nil
}

// typesMatch compares socket types the way the graph builder does: equal, or
// either one is "*".
func typesMatch(a, b string) bool { return a == b || a == "*" || b == "*" }

// programError makes an *Error of what running main.star failed with, its Msg
// starting with the line of main.star it came from. An error the host API
// raised keeps its setting and item. An error from an instance nested under
// this one comes out as it is: its chain says where it is, and a line here
// would read as a line of the nested blueprint.
func (f *frame) programError(err error) *Error {
	where, msg := programFile, err.Error()
	var (
		ee *starlark.EvalError
		se syntax.Error
		re resolve.ErrorList
	)
	switch {
	case errors.As(err, &ee):
		msg = ee.Msg
		for i := len(ee.CallStack) - 1; i >= 0; i-- {
			if pos := ee.CallStack[i].Pos; pos.Filename() == programFile {
				where = fmt.Sprintf("%s:%d", programFile, pos.Line)
				break
			}
		}
	case errors.As(err, &se):
		where, msg = fmt.Sprintf("%s:%d", programFile, se.Pos.Line), se.Msg
	case errors.As(err, &re):
		where, msg = fmt.Sprintf("%s:%d", programFile, re[0].Pos.Line), re[0].Msg
	}
	var hostErr *Error
	if errors.As(err, &hostErr) {
		if len(hostErr.Instances) > len(f.chain) {
			return hostErr
		}
		e := *hostErr
		e.Msg = where + ": " + hostErr.Msg
		return &e
	}
	return f.errorf("", "%s: %s", where, msg)
}

// settingValue is a setting's value as a program reads it: a frozen list for
// a list setting, an int for int and seed, a float, string or bool for the
// other kinds, and a socket reference for a linked setting.
func (f *frame) settingValue(s Setting, v any) (starlark.Value, error) {
	if r, ok := v.(*ref); ok {
		return &socket{f: f, r: r, setting: s.Name}, nil
	}
	if s.List == nil {
		return f.elementValue(s, v)
	}
	items, _ := v.([]any)
	elems := make([]starlark.Value, len(items))
	for i, item := range items {
		e, err := f.elementValue(s, item)
		if err != nil {
			return nil, err
		}
		elems[i] = e
	}
	l := starlark.NewList(elems)
	l.Freeze()
	return l, nil
}

// elementValue is settingValue for one value of the setting's kind, which
// compile has checked already.
func (f *frame) elementValue(s Setting, v any) (starlark.Value, error) {
	switch s.Kind {
	case KindInt, KindSeed:
		switch n := v.(type) {
		case int:
			return starlark.MakeInt(n), nil
		case float64:
			if n >= math.MinInt64 && n < math.MaxInt64 {
				return starlark.MakeInt64(int64(n)), nil
			}
			if b, _ := big.NewFloat(n).Int(nil); b != nil {
				return starlark.MakeBigInt(b), nil
			}
		}
	case KindFloat:
		if n, ok := asNumber(v); ok {
			return starlark.Float(n), nil
		}
	case KindBool:
		if b, ok := v.(bool); ok {
			return starlark.Bool(b), nil
		}
	default: // string, combo and image
		if str, ok := v.(string); ok {
			return starlark.String(str), nil
		}
	}
	return nil, f.errorf(s.Name, "setting %s holds %s, which a program cannot read", s.Name, show(v))
}

// host is what every ctx of one instance's program shares.
type host struct {
	m        *Manifest
	classes  Classes
	settings map[string]any // the instance's settings, for ctx.child's item
}

// hostCtx is a program's ctx: it emits nodes into its frame.
type hostCtx struct {
	h *host
	f *frame
}

var _ starlark.HasAttrs = (*hostCtx)(nil)

func (c *hostCtx) String() string        { return "<ctx " + c.f.prefix + ">" }
func (c *hostCtx) Type() string          { return "ctx" }
func (c *hostCtx) Freeze()               {}
func (c *hostCtx) Truth() starlark.Bool  { return true }
func (c *hostCtx) Hash() (uint32, error) { return 0, errors.New("unhashable type: ctx") }
func (c *hostCtx) AttrNames() []string   { return []string{"child", "node", "use"} }

func (c *hostCtx) Attr(name string) (starlark.Value, error) {
	switch name {
	case "node":
		return starlark.NewBuiltin("node", c.node), nil
	case "child":
		return starlark.NewBuiltin("child", c.child), nil
	case "use":
		return starlark.NewBuiltin("use", c.use), nil
	}
	return nil, nil
}

// node is ctx.node(name, class, **inputs). It emits <prefix>.<name> with the
// inputs given, each widget input left out or given None taking the class's
// default, and returns the node.
func (c *hostCtx) node(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var name, class string
	if err := starlark.UnpackPositionalArgs(b.Name(), args, nil, 2, &name, &class); err != nil {
		return nil, err
	}
	if !partName.MatchString(name) {
		return nil, c.f.errorf("", "invalid node name %q (use letters, digits, _ and -)", name)
	}
	info, ok := c.h.classes.Class(class)
	if !ok {
		return nil, c.f.errorf("", "class %s is not on this server", class)
	}
	in := make(map[string]any, len(kwargs)+len(info.Inputs))
	for _, kv := range kwargs {
		k := string(kv[0].(starlark.String))
		v, given, problem := inputValue(kv[1])
		if problem != "" {
			return nil, c.f.errorf("", "input %s: %s", k, problem)
		}
		if given {
			in[k] = v
		}
	}
	for _, i := range info.Inputs {
		if _, ok := in[i.Name]; !ok && i.Widget && i.HasDefault {
			in[i.Name] = i.Default
		}
	}
	id, err := c.f.emit(name, &graph.Node{Class: class, Inputs: in})
	if err != nil {
		return nil, err
	}
	return &nodeValue{f: c.f, id: id, class: class, outputs: info.Outputs}, nil
}

// child is ctx.child(name, item = (setting, index)): a ctx whose nodes are
// emitted under <prefix>.<name>, made for that element of a list setting.
// Without an item it is made for what this ctx is made for.
func (c *hostCtx) child(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var name string
	var item starlark.Value = starlark.None
	if err := starlark.UnpackArgs(b.Name(), args, kwargs, "name", &name, "item?", &item); err != nil {
		return nil, err
	}
	if !partName.MatchString(name) {
		return nil, c.f.errorf("", "invalid child name %q (use letters, digits, _ and -)", name)
	}
	cf := *c.f
	cf.prefix = c.f.prefix + "." + name
	if item != starlark.None {
		it, err := c.item(item)
		if err != nil {
			return nil, err
		}
		cf.tag(it)
	}
	return &hostCtx{h: c.h, f: &cf}, nil
}

// use is ctx.use(id, name, inputs = {…}, settings = {…}): an instance of the
// blueprint id that the manifest's uses pins, its nodes emitted under
// <prefix>.<name>. inputs maps input ports to socket references, and
// settings maps settings to values, or to socket references for linkable
// ones; None, or a name left out, is nothing behind a port and a setting's
// default. It returns a struct with a socket reference for each output
// port, or None for one with nothing behind it.
func (c *hostCtx) use(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var id, name string
	var inputs, settings *starlark.Dict
	if err := starlark.UnpackArgs(b.Name(), args, kwargs, "id", &id, "name", &name, "inputs?", &inputs, "settings?", &settings); err != nil {
		return nil, err
	}
	if !partName.MatchString(name) {
		return nil, c.f.errorf("", "invalid use name %q (use letters, digits, _ and -)", name)
	}
	ins, err := c.names("inputs", inputs)
	if err != nil {
		return nil, err
	}
	sets, err := c.names("settings", settings)
	if err != nil {
		return nil, err
	}
	m, outs, err := c.f.nest(c.h.m.Uses, id, "", name, func(nf *frame, m *Manifest) (feed, error) {
		return useFeed(nf, m, ins, sets)
	})
	if err != nil {
		return nil, err
	}
	fields := make(starlark.StringDict, len(m.Outputs))
	for _, p := range m.Outputs {
		if r := outs[p.Name]; r != nil {
			fields[p.Name] = &socket{f: c.f, r: r, typ: p.Type}
		} else {
			fields[p.Name] = starlark.None
		}
	}
	return starlarkstruct.FromStringDict(starlarkstruct.Default, fields), nil
}

// named is one entry of a dict ctx.use takes.
type named struct {
	name string
	v    starlark.Value
}

// names reads a dict argument of ctx.use, which may be absent, in the order
// the program built it.
func (c *hostCtx) names(arg string, d *starlark.Dict) ([]named, error) {
	if d == nil {
		return nil, nil
	}
	var out []named
	for _, kv := range d.Items() {
		name, ok := starlark.AsString(kv[0])
		if !ok {
			return nil, c.f.errorf("", "%s has key %s, which is not a string", arg, kv[0])
		}
		out = append(out, named{name, kv[1]})
	}
	return out, nil
}

// useFeed is what fills the inputs of the instance a ctx.use makes, read
// against its manifest m: each port's socket reference, and each setting's
// value as a graph holds it. Its errors are about the nested instance nf.
func useFeed(nf *frame, m *Manifest, inputs, settings []named) (feed, error) {
	values := map[string]any{}
	for _, in := range inputs {
		i := slices.IndexFunc(m.Inputs, func(p Port) bool { return p.Name == in.name })
		if i < 0 {
			return feed{}, nf.errorf("", "inputs has %s, which is not an input port of the blueprint", in.name)
		}
		switch v := in.v.(type) {
		case starlark.NoneType:
			// nothing behind it
		case *socket:
			if v.setting != "" {
				// A linked setting passes into ctx.node, or into a linkable
				// setting below, and nothing else.
				return feed{}, v.misuse()
			}
			if want := m.Inputs[i].Type; !typesMatch(v.typ, want) {
				return feed{}, nf.errorf("", "input %s takes %s, got %s", in.name, want, v.typ)
			}
			values[in.name] = v.r
		case *nodeValue:
			return feed{}, nf.errorf("", "input %s: %s is a node, not a socket; use .OUTPUT or .out(i)", in.name, v)
		default:
			return feed{}, nf.errorf("", "input %s must be a socket reference, got %s", in.name, v.Type())
		}
	}
	for _, s := range settings {
		if m.settingNamed(s.name) == nil {
			return feed{}, nf.errorf("", "settings has %s, which is not a setting of the blueprint", s.name)
		}
		switch v := s.v.(type) {
		case starlark.NoneType:
			// the default
		case *socket:
			values[s.name] = v.r
		case *nodeValue:
			return feed{}, nf.errorf(s.name, "setting %s: %s is a node, not a socket; use .OUTPUT or .out(i)", s.name, v)
		default:
			lit, problem := literal(v)
			if problem != "" {
				return feed{}, nf.errorf(s.name, "setting %s: %s", s.name, problem)
			}
			values[s.name] = graphValue(lit)
		}
	}
	// Every socket reference is a *ref already, so a link here is a literal
	// that only looks like one.
	return feed{values: values, resolve: func(input, node string, idx int) (*ref, error) {
		return nil, nf.errorf(input, "setting %s: %s reads as a link; pass a socket reference", input, show([]any{node, idx}))
	}}, nil
}

// graphValue is a literal as a graph's JSON holds it, with every number a
// float64, which is what an instance in the graph holds and what the
// setting checks read.
func graphValue(v any) any {
	switch v := v.(type) {
	case int64:
		return float64(v)
	case uint64:
		return float64(v)
	case []any:
		out := make([]any, len(v))
		for i, x := range v {
			out[i] = graphValue(x)
		}
		return out
	}
	return v
}

// item reads ctx.child's item: a list setting of the blueprint and an index
// within the list the instance holds.
func (c *hostCtx) item(v starlark.Value) (*Item, error) {
	var pair starlark.Indexable
	switch v := v.(type) {
	case starlark.Tuple:
		pair = v
	case *starlark.List:
		pair = v
	}
	if pair == nil || pair.Len() != 2 {
		return nil, c.f.errorf("", "item must be a (setting, index) pair, got %s", v)
	}
	name, nameOK := starlark.AsString(pair.Index(0))
	index, err := starlark.AsInt32(pair.Index(1))
	if !nameOK || err != nil {
		return nil, c.f.errorf("", "item must be a (setting, index) pair, got %s", v)
	}
	if s := c.h.m.settingNamed(name); s == nil || s.List == nil {
		return nil, c.f.errorf("", "item names %s, which is not a list setting", name)
	}
	items, _ := c.h.settings[name].([]any)
	if index < 0 || index >= len(items) {
		return nil, c.f.errorf(name, "item %d is out of range: setting %s has %s", index, name, count(len(items), "item"))
	}
	return &Item{Setting: name, Index: index}, nil
}

// inputValue is what a keyword of ctx.node puts in the node: a link for a
// socket reference, else a literal. given is false for None, which leaves
// the input to its default. problem says why v cannot be an input's value.
func inputValue(v starlark.Value) (val any, given bool, problem string) {
	switch v := v.(type) {
	case starlark.NoneType:
		return nil, false, ""
	case *socket:
		return v.r.link(), true, ""
	case *nodeValue:
		return nil, false, fmt.Sprintf("%s is a node, not a socket; use .OUTPUT or .out(i)", v)
	}
	lit, problem := literal(v)
	if problem != "" {
		return nil, false, problem
	}
	// ComfyUI reads [id, number] as a link; only a socket reference may make one.
	if l, ok := lit.([]any); ok && len(l) == 2 {
		_, isID := l[0].(string)
		switch l[1].(type) {
		case int64, uint64, float64:
			if isID {
				return nil, false, fmt.Sprintf("%s reads as a link; pass a socket reference", v)
			}
		}
	}
	return lit, true, ""
}

// literal converts a string, int, float, bool, or a list or tuple of them.
func literal(v starlark.Value) (any, string) {
	switch v := v.(type) {
	case starlark.String:
		return string(v), ""
	case starlark.Bool:
		return bool(v), ""
	case starlark.Int:
		if i, ok := v.Int64(); ok {
			return i, ""
		}
		if u, ok := v.Uint64(); ok {
			return u, ""
		}
		return nil, fmt.Sprintf("%s does not fit in 64 bits", v)
	case starlark.Float:
		if f := float64(v); !math.IsNaN(f) && !math.IsInf(f, 0) {
			return f, ""
		}
		return nil, fmt.Sprintf("%s is not a finite number", v)
	case starlark.Tuple, *starlark.List:
		seq := v.(starlark.Indexable)
		out := make([]any, seq.Len())
		for i := range out {
			x, problem := literal(seq.Index(i))
			if problem != "" {
				return nil, problem
			}
			out[i] = x
		}
		return out, ""
	}
	return nil, fmt.Sprintf("cannot take a %s (strings, numbers, bools, lists of them and socket references only)", v.Type())
}

// nodeValue is what ctx.node returns: node.OUTPUT and node.out(i) are its
// outputs' socket references.
type nodeValue struct {
	f       *frame
	id      string
	class   string
	outputs []OutputInfo
}

var _ starlark.HasAttrs = (*nodeValue)(nil)

func (n *nodeValue) String() string        { return fmt.Sprintf("<node %s %s>", n.id, n.class) }
func (n *nodeValue) Type() string          { return "node" }
func (n *nodeValue) Freeze()               {}
func (n *nodeValue) Truth() starlark.Bool  { return true }
func (n *nodeValue) Hash() (uint32, error) { return 0, errors.New("unhashable type: node") }

// Attr is node.out, else the socket reference of the output named name.
func (n *nodeValue) Attr(name string) (starlark.Value, error) {
	if name == "out" {
		return starlark.NewBuiltin("out", n.out), nil
	}
	found := -1
	for i, o := range n.outputs {
		if o.Name != name {
			continue
		}
		if found >= 0 {
			return nil, n.f.errorf("", "two outputs named %s; use out(i)", name)
		}
		found = i
	}
	if found < 0 {
		return nil, starlark.NoSuchAttrError(fmt.Sprintf("%s has no output %s", n.class, name))
	}
	return n.socket(found), nil
}

func (n *nodeValue) AttrNames() []string {
	names := []string{"out"}
	for _, o := range n.outputs {
		if !slices.Contains(names, o.Name) {
			names = append(names, o.Name)
		}
	}
	slices.Sort(names)
	return names
}

// out is node.out(i), the socket reference of output i.
func (n *nodeValue) out(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
	var i int
	if err := starlark.UnpackPositionalArgs(b.Name(), args, kwargs, 1, &i); err != nil {
		return nil, err
	}
	if i < 0 || i >= len(n.outputs) {
		return nil, n.f.errorf("", "out(%d): %s has %s", i, n.class, count(len(n.outputs), "output"))
	}
	return n.socket(i), nil
}

func (n *nodeValue) socket(i int) *socket {
	return &socket{f: n.f, r: &ref{node: n.id, out: i}, typ: n.outputs[i].Type}
}

// socket is a socket reference: a compiled socket and its type, or a linked
// setting. A program passes one to ctx.node or returns it from build;
// arithmetic, comparison and hashing fail.
type socket struct {
	f       *frame // where it was made, for errors
	r       *ref
	typ     string
	setting string // the linked setting it is, if any
}

var (
	_ starlark.HasBinary  = (*socket)(nil)
	_ starlark.HasUnary   = (*socket)(nil)
	_ starlark.Comparable = (*socket)(nil)
)

func (s *socket) String() string {
	if s.setting != "" {
		return "<ref setting " + s.setting + ">"
	}
	return fmt.Sprintf("<ref %s:%d %s>", s.r.node, s.r.out, s.typ)
}
func (s *socket) Type() string          { return "ref" }
func (s *socket) Freeze()               {}
func (s *socket) Truth() starlark.Bool  { return true }
func (s *socket) Hash() (uint32, error) { return 0, s.misuse() }

func (s *socket) Binary(syntax.Token, starlark.Value, starlark.Side) (starlark.Value, error) {
	return nil, s.misuse()
}

func (s *socket) Unary(syntax.Token) (starlark.Value, error) { return nil, s.misuse() }

func (s *socket) CompareSameType(syntax.Token, starlark.Value, int) (bool, error) {
	return false, s.misuse()
}

// misuse is the error for using s as anything but a socket.
func (s *socket) misuse() *Error {
	if s.setting != "" {
		return s.f.errorf(s.setting, "setting %s is linked; this blueprint needs a value", s.setting)
	}
	return s.f.errorf("", "a socket reference can only be passed to ctx.node")
}
