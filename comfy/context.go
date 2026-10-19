// Package comfy is what workflow programs import. A program calls Run with a
// function that declares nodes through generated bindings; the comfy CLI runs
// the program and takes the compiled prompt from there.
package comfy

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/erstaples/comfyform/graph"
)

// Env vars the CLI uses to talk to the program.
const (
	EnvEmit   = "COMFY_EMIT"   // path to write the compiled prompt
	EnvStack  = "COMFY_STACK"  // stack name
	EnvConfig = "COMFY_CONFIG" // stack config as JSON
)

// Emitted is the program's output, consumed by the CLI.
type Emitted struct {
	Stack  string       `json:"stack"`
	Prompt graph.Prompt `json:"prompt"`
}

type build struct {
	prompt graph.Prompt
	errs   []string
}

// Context declares nodes. Child contexts add a name prefix, which keeps node
// ids stable and meaningful across edits ("hires.sampler" instead of "17").
type Context struct {
	b      *build
	prefix string
	stack  string
	cfg    Config
}

// NewContext is for tests and embedding; programs normally use Run.
func NewContext(stack string, cfg Config) *Context {
	return &Context{b: &build{prompt: graph.Prompt{}}, stack: stack, cfg: cfg}
}

func (c *Context) Stack() string  { return c.stack }
func (c *Context) Config() Config { return c.cfg }

// Child returns a context whose nodes are named "<prefix>.<name>".
func (c *Context) Child(name string) *Context {
	cc := *c
	cc.prefix = c.qualify(name)
	return &cc
}

func (c *Context) qualify(name string) string {
	if c.prefix == "" {
		return name
	}
	return c.prefix + "." + name
}

// Errorf records a build error; Run reports all of them at the end.
func (c *Context) Errorf(format string, args ...any) {
	c.b.errs = append(c.b.errs, fmt.Sprintf(format, args...))
}

var validName = regexp.MustCompile(`^[A-Za-z0-9_\-]+$`)

// Node adds a node and returns its id. Generated bindings call this; use it
// directly for classes you haven't generated bindings for.
func (c *Context) Node(name, class string, in graph.Inputs, raw graph.Inputs) string {
	if !validName.MatchString(name) {
		c.Errorf("invalid node name %q (use letters, digits, _ and -)", name)
	}
	id := c.qualify(name)
	if _, dup := c.b.prompt[id]; dup {
		c.Errorf("duplicate node id %q", id)
	}
	inputs := map[string]any{}
	for k, v := range in {
		inputs[k] = v
	}
	for k, v := range raw {
		inputs[k] = v
	}
	c.b.prompt[id] = &graph.Node{Class: class, Inputs: inputs, Meta: &graph.Meta{Title: id}}
	return id
}

// Prompt returns the compiled prompt and any build errors.
func (c *Context) Prompt() (graph.Prompt, error) {
	if len(c.b.errs) > 0 {
		return nil, fmt.Errorf("%d build error(s):\n  %s", len(c.b.errs), strings.Join(c.b.errs, "\n  "))
	}
	return c.b.prompt.Normalize()
}

// Run executes a workflow program. Under the CLI it writes the compiled prompt
// to $COMFY_EMIT; run directly, it prints the API JSON to stdout.
func Run(fn func(ctx *Context) error) {
	stack := os.Getenv(EnvStack)
	if stack == "" {
		stack = "default"
	}
	cfg := Config{}
	if raw := os.Getenv(EnvConfig); raw != "" {
		if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
			fail("bad %s: %v", EnvConfig, err)
		}
	}
	ctx := NewContext(stack, cfg)
	if err := fn(ctx); err != nil {
		fail("%v", err)
	}
	p, err := ctx.Prompt()
	if err != nil {
		fail("%v", err)
	}
	out, _ := json.MarshalIndent(Emitted{Stack: stack, Prompt: p}, "", "  ")
	if path := os.Getenv(EnvEmit); path != "" {
		if err := os.WriteFile(path, out, 0o644); err != nil {
			fail("write %s: %v", path, err)
		}
		return
	}
	api, _ := json.MarshalIndent(p, "", "  ")
	fmt.Println(string(api))
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "comfy: "+format+"\n", args...)
	os.Exit(1)
}

// --- helpers used by generated bindings ---

// SetLink sets a link input. A missing required link is a build error.
func SetLink(c *Context, in graph.Inputs, node, key string, r graph.AnyRef, required bool) {
	if r == nil || r.RefNode() == "" {
		if required {
			c.Errorf("%s: required input %q is not connected", c.qualify(node), key)
		}
		return
	}
	in[key] = r
}

// SetValue sets a literal. Zero means "use the schema default"; an optional
// input with no default is omitted. Use Raw to force a literal zero.
func SetValue[T comparable](in graph.Inputs, key string, v T, def T, hasDef, required bool) {
	var zero T
	switch {
	case v != zero:
		in[key] = v
	case hasDef:
		in[key] = def
	case required:
		in[key] = v
	}
}

// SetBool handles *bool fields: nil means default.
func SetBool(in graph.Inputs, key string, v *bool, def, hasDef, required bool) {
	switch {
	case v != nil:
		in[key] = *v
	case hasDef:
		in[key] = def
	case required:
		in[key] = false
	}
}

// SetAny is for inputs whose Go type is any (mixed-type combos).
func SetAny(in graph.Inputs, key string, v any, def any, hasDef, required bool) {
	switch {
	case v != nil:
		in[key] = v
	case hasDef:
		in[key] = def
	case required:
		in[key] = nil
	}
}

// --- stack config ---

// Config is the "config" object from comfy.<stack>.json.
type Config map[string]any

func (c Config) Has(key string) bool { _, ok := c[key]; return ok }

func (c Config) IntOr(key string, def int64) int64 {
	if v, ok := c[key].(float64); ok {
		return int64(v)
	}
	return def
}

func (c Config) FloatOr(key string, def float64) float64 {
	if v, ok := c[key].(float64); ok {
		return v
	}
	return def
}

func (c Config) StringOr(key, def string) string {
	if v, ok := c[key].(string); ok {
		return v
	}
	return def
}

func (c Config) Bool(key string) bool {
	v, _ := c[key].(bool)
	return v
}

// Floats reads a list of numbers, e.g. sweep values.
func (c Config) Floats(key string) []float64 {
	arr, _ := c[key].([]any)
	var out []float64
	for _, v := range arr {
		if f, ok := v.(float64); ok {
			out = append(out, f)
		}
	}
	return out
}

// Keys is mostly for debugging.
func (c Config) Keys() []string {
	ks := make([]string, 0, len(c))
	for k := range c {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
