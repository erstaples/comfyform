// Package graph is the intermediate representation shared by generated node
// bindings, the user program and the plan/apply engine. A compiled workflow is
// a Prompt: ComfyUI's API format, keyed by stable, human-readable node IDs.
package graph

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// AnyRef is a reference to one output slot of a node.
type AnyRef interface {
	RefNode() string
	RefIndex() int
}

// Ref is a typed reference to an output slot. T is a marker type generated per
// ComfyUI socket type (MODEL, CLIP, LATENT, ...), so wiring a CLIP into a MODEL
// input fails at compile time.
type Ref[T any] struct {
	node string
	idx  int
}

// MakeRef is used by generated code.
func MakeRef[T any](node string, idx int) Ref[T] { return Ref[T]{node: node, idx: idx} }

func (r Ref[T]) RefNode() string { return r.node }
func (r Ref[T]) RefIndex() int   { return r.idx }
func (r Ref[T]) IsSet() bool     { return r.node != "" }

// MarshalJSON emits ComfyUI's link form: ["node-id", slot].
func (r Ref[T]) MarshalJSON() ([]byte, error) { return json.Marshal([]any{r.node, r.idx}) }

// Link builds an untyped reference, for raw nodes and Raw overrides.
func Link(node string, idx int) AnyRef { return Ref[struct{}]{node: node, idx: idx} }

// Inputs is a node's input map. Values are literals or AnyRef links.
type Inputs map[string]any

// Bool returns a pointer, for optional BOOLEAN fields in generated Args.
func Bool(v bool) *bool { return &v }

// Node is one entry of the API-format prompt.
type Node struct {
	Class  string         `json:"class_type"`
	Inputs map[string]any `json:"inputs"`
	Meta   *Meta          `json:"_meta,omitempty"`
}

type Meta struct {
	Title string `json:"title,omitempty"`
}

// Prompt is ComfyUI's API format: node id -> node.
type Prompt map[string]*Node

// IDs returns node ids in natural order (numeric ids sort numerically).
func (p Prompt) IDs() []string {
	ids := make([]string, 0, len(p))
	for id := range p {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return NaturalLess(ids[i], ids[j]) })
	return ids
}

// NaturalLess orders "2" before "10" and falls back to string order.
func NaturalLess(a, b string) bool {
	ai, aerr := strconv.Atoi(a)
	bi, berr := strconv.Atoi(b)
	if aerr == nil && berr == nil {
		return ai < bi
	}
	if (aerr == nil) != (berr == nil) {
		return aerr == nil
	}
	return a < b
}

// AsLink reports whether a decoded JSON value is a link ["id", n].
func AsLink(v any) (node string, idx int, ok bool) {
	switch t := v.(type) {
	case AnyRef:
		return t.RefNode(), t.RefIndex(), true
	case []any:
		if len(t) != 2 {
			return "", 0, false
		}
		s, sok := t[0].(string)
		if !sok {
			return "", 0, false
		}
		switch n := t[1].(type) {
		case float64:
			return s, int(n), true
		case int:
			return s, n, true
		case json.Number:
			i, err := n.Int64()
			return s, int(i), err == nil
		}
	}
	return "", 0, false
}

// Normalize round-trips the prompt through JSON so in-memory refs and decoded
// links share one representation.
func (p Prompt) Normalize() (Prompt, error) {
	b, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	var out Prompt
	return out, json.Unmarshal(b, &out)
}

// Hashes computes a Merkle hash per node: its class, literal inputs, and the
// hashes of everything upstream. Equal hash means equal (deterministic) work.
func (p Prompt) Hashes() (map[string]string, error) {
	memo := map[string]string{}
	visiting := map[string]bool{}
	var visit func(id string) (string, error)
	visit = func(id string) (string, error) {
		if h, ok := memo[id]; ok {
			return h, nil
		}
		n, ok := p[id]
		if !ok {
			return "", fmt.Errorf("link to missing node %q", id)
		}
		if visiting[id] {
			return "", fmt.Errorf("cycle through node %q", id)
		}
		visiting[id] = true
		defer delete(visiting, id)

		keys := make([]string, 0, len(n.Inputs))
		for k := range n.Inputs {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var sb strings.Builder
		sb.WriteString(n.Class)
		for _, k := range keys {
			v := n.Inputs[k]
			sb.WriteString("|" + k + "=")
			if up, idx, ok := AsLink(v); ok {
				h, err := visit(up)
				if err != nil {
					return "", err
				}
				fmt.Fprintf(&sb, "@%s#%d", h, idx)
				continue
			}
			b, _ := json.Marshal(v)
			sb.Write(b)
		}
		sum := sha256.Sum256([]byte(sb.String()))
		h := hex.EncodeToString(sum[:8])
		memo[id] = h
		return h, nil
	}
	for _, id := range p.IDs() {
		if _, err := visit(id); err != nil {
			return nil, err
		}
	}
	return memo, nil
}
