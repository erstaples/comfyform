// Package blueprint holds shareable, versioned pieces of ComfyUI graph that
// compile to plain API-format graphs.
package blueprint

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"regexp"
	"slices"
	"strings"
)

// Format is the manifest format this package reads and writes.
const Format = 1

// Manifest is a blueprint package's blueprint.json.
type Manifest struct {
	Format      int            `json:"format"`
	ID          string         `json:"id"`
	Version     string         `json:"version"`
	Title       string         `json:"title"`
	Description string         `json:"description,omitempty"`
	Inputs      []Port         `json:"inputs"`
	Outputs     []Port         `json:"outputs"`
	Settings    []Setting      `json:"settings"`
	Repeats     []Repeat       `json:"repeats,omitempty"`
	Result      string         `json:"result,omitempty"`
	Requires    Requires       `json:"requires"`
	Uses        map[string]Use `json:"uses,omitempty"`
}

// Port is a link input or output of an instance.
type Port struct {
	Name     string   `json:"name"`
	Type     string   `json:"type"`
	Optional bool     `json:"optional,omitempty"`
	To       []Target `json:"to,omitempty"`   // node group input ports
	From     *Socket  `json:"from,omitempty"` // node group output ports
}

// Target is a node group's input that a port or setting feeds.
type Target struct {
	NodeID string `json:"nodeId"`
	Input  string `json:"input"`
}

// Socket is an output of a node group's node.
type Socket struct {
	NodeID string `json:"nodeId"`
	Output int    `json:"output"`
}

// Kind is the type of value a setting holds.
type Kind string

const (
	KindInt    Kind = "int"
	KindFloat  Kind = "float"
	KindString Kind = "string"
	KindBool   Kind = "bool"
	KindCombo  Kind = "combo"
	KindImage  Kind = "image"
	KindSeed   Kind = "seed"
)

func (k Kind) valid() bool {
	switch k {
	case KindInt, KindFloat, KindString, KindBool, KindCombo, KindImage, KindSeed:
		return true
	}
	return false
}

// Setting is a value a user gives an instance, as opposed to a link.
type Setting struct {
	Name     string       `json:"name"`
	Kind     Kind         `json:"kind"`
	Default  any          `json:"default,omitempty"`
	Min      *float64     `json:"min,omitempty"`
	Max      *float64     `json:"max,omitempty"`
	Step     *float64     `json:"step,omitempty"`
	Options  []string     `json:"options,omitempty"`
	Source   *ModelSource `json:"source,omitempty"`
	List     *ListBounds  `json:"list,omitempty"`
	Linkable *bool        `json:"linkable,omitempty"`
	To       []Target     `json:"to,omitempty"`
}

// IsLinkable reports whether a link in the graph may feed the setting:
// Linkable when the manifest sets it, otherwise true for a seed.
func (s Setting) IsLinkable() bool {
	if s.Linkable != nil {
		return *s.Linkable
	}
	return s.Kind == KindSeed
}

// ListBounds makes a setting a list of between Min and Max values.
type ListBounds struct {
	Min int `json:"min"`
	Max int `json:"max"`
}

// ModelSource fills a combo's options from the models on a server.
type ModelSource struct {
	Kind   string `json:"kind"`
	Folder string `json:"folder"`
}

// Repeat is a part of a node group that is copied once per list element or
// count, each copy building on the one before.
type Repeat struct {
	Nodes []string `json:"nodes"`
	Over  []string `json:"over,omitempty"`
	Count string   `json:"count,omitempty"`
	Carry []Carry  `json:"carry"`
}

// Carry passes an output of one copy of a repeat to an input of the next.
type Carry struct {
	To   Target `json:"to"`
	From Socket `json:"from"`
}

// Requires is what a server must have for the blueprint to run.
type Requires struct {
	Classes map[string]string   `json:"classes"`
	Packs   map[string]Pack     `json:"packs,omitempty"`
	Models  map[string][]string `json:"models,omitempty"`
}

// Pack is a custom node pack a class comes from; same JSON as engine.LockedPack.
type Pack struct {
	Source  string `json:"source"`
	Content string `json:"content,omitempty"`
}

// Use pins a blueprint this one instantiates.
type Use struct {
	Version string `json:"version"`
	Digest  string `json:"digest"`
}

// classPrefix starts the class name of every blueprint instance node.
const classPrefix = "bp:"

var (
	idPattern      = regexp.MustCompile(`^[a-z0-9-]+/[a-z0-9-]+$`)
	versionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)
	namePattern    = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	hexSumPattern  = regexp.MustCompile(`^sha256:[0-9a-f]+$`)
)

// ParseManifest decodes a blueprint.json, rejecting fields it does not know,
// and runs the checks that need no other file. It returns Problems when it
// finds any.
func ParseManifest(b []byte) (*Manifest, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var m Manifest
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("parse manifest: unexpected data after the manifest")
	}
	if ps := checkManifest(&m); len(ps) > 0 {
		return nil, ps
	}
	return &m, nil
}

// Class is the node class an instance of this blueprint version has in a graph.
func (m *Manifest) Class() string { return classPrefix + m.ID + "@" + m.Version }

// ParseClass splits a blueprint class name into its id and version. ok is
// false for anything that is not a well-formed blueprint class.
func ParseClass(class string) (id, version string, ok bool) {
	rest, found := strings.CutPrefix(class, classPrefix)
	if !found {
		return "", "", false
	}
	id, version, found = strings.Cut(rest, "@")
	if !found || !idPattern.MatchString(id) || !versionPattern.MatchString(version) {
		return "", "", false
	}
	return id, version, true
}

// checkManifest returns every problem the manifest has on its own. It leaves
// the checks that read graph.json or main.star to package loading, and the
// repeat rules to their own check.
func checkManifest(m *Manifest) Problems {
	var ps Problems
	if m.Format != Format {
		ps.add("format", "format %d is not supported (this build reads format %d)", m.Format, Format)
	}
	if !idPattern.MatchString(m.ID) {
		ps.add("id", "id must be namespace/name, both [a-z0-9-]+ (got %q)", m.ID)
	}
	if !versionPattern.MatchString(m.Version) {
		ps.add("version", "version must be MAJOR.MINOR.PATCH, digits only (got %q)", m.Version)
	}
	if strings.TrimSpace(m.Title) == "" {
		ps.add("title", "title is empty")
	}

	// Inputs and settings both become inputs of an instance, so they share
	// one set of names. Outputs are a set of their own.
	inputs := map[string]bool{}
	for i, p := range m.Inputs {
		ps.checkPort("inputs", i, p, inputs)
	}
	outputs := map[string]bool{}
	for i, p := range m.Outputs {
		ps.checkPort("outputs", i, p, outputs)
	}
	settings := map[string]bool{}
	for i, s := range m.Settings {
		where := label("settings", i, s.Name)
		ps.checkName(where, s.Name)
		if s.Name != "" {
			switch {
			case inputs[s.Name]:
				ps.add(where, "this setting names an input port, and both become inputs of an instance")
			case settings[s.Name]:
				ps.add(where, "name appears twice")
			}
			settings[s.Name] = true
		}
		ps.checkSetting(where, s)
	}

	ps.checkRequires(m.Requires)
	for _, id := range slices.Sorted(maps.Keys(m.Uses)) {
		ps.checkUse(fmt.Sprintf("uses %q", id), id, m.Uses[id])
	}
	return ps
}

// label names a port or setting in a Problem: by name when it has one, else
// by position.
func label(group string, i int, name string) string {
	if name == "" {
		return fmt.Sprintf("%s[%d]", group, i)
	}
	return fmt.Sprintf("%s %q", group, name)
}

func (ps *Problems) checkName(where, name string) {
	switch {
	case name == "":
		ps.add(where, "name is empty")
	case !namePattern.MatchString(name):
		ps.add(where, "name must be lowercase letters, digits and underscores, starting with a letter (got %q)", name)
	}
}

func (ps *Problems) checkPort(group string, i int, p Port, taken map[string]bool) {
	where := label(group, i, p.Name)
	ps.checkName(where, p.Name)
	if p.Name != "" {
		if taken[p.Name] {
			ps.add(where, "name appears twice")
		}
		taken[p.Name] = true
	}
	if strings.TrimSpace(p.Type) == "" {
		ps.add(where, "type is empty")
	}
}

func (ps *Problems) checkSetting(where string, s Setting) {
	if !s.Kind.valid() {
		ps.add(where, "unknown kind %q", s.Kind)
		return
	}
	if l := s.List; l != nil {
		if s.Kind == KindSeed {
			ps.add(where, "a seed cannot be a list")
		}
		if l.Min < 0 {
			ps.add(where, "list min is negative")
		}
		if l.Min > l.Max {
			ps.add(where, "list min is above max")
		}
		if s.Linkable != nil && *s.Linkable {
			ps.add(where, "a list setting cannot be linkable")
		}
	}
	if s.boundsInverted() {
		ps.add(where, "min is above max")
	}
	if s.Step != nil && *s.Step <= 0 {
		ps.add(where, "step must be above 0")
	}
	if s.Kind == KindCombo && len(s.Options) == 0 && s.Source == nil {
		ps.add(where, "a combo needs options or a models source")
	}
	if src := s.Source; src != nil {
		if src.Kind != "models" {
			ps.add(where, `source kind must be "models" (got %q)`, src.Kind)
		}
		if src.Folder == "" {
			ps.add(where, "source needs a folder")
		}
	}
	if s.Kind == KindSeed && s.List != nil {
		return // already reported; a default would be judged as a list of seeds
	}
	ps.checkDefault(where, s)
}

// boundsInverted reports a min above a max, where checking a value against
// either would only report the same mistake again.
func (s Setting) boundsInverted() bool {
	return s.Min != nil && s.Max != nil && *s.Min > *s.Max
}

func (ps *Problems) checkDefault(where string, s Setting) {
	if s.Default == nil {
		// An image may be left empty and a list starts as [].
		if s.Kind != KindImage && s.List == nil {
			ps.add(where, "needs a default")
		}
		return
	}
	if s.List == nil {
		if msg := s.valueProblem("default", s.Default); msg != "" {
			ps.add(where, "%s", msg)
		}
		return
	}
	items, ok := s.Default.([]any)
	if !ok {
		ps.add(where, "default of a list must be a list")
		return
	}
	if s.List.Min <= s.List.Max && len(items) > s.List.Max {
		ps.add(where, "default has %d items, above list max %d", len(items), s.List.Max)
	}
	for i, item := range items {
		if msg := s.valueProblem(fmt.Sprintf("default[%d]", i), item); msg != "" {
			ps.add(where, "%s", msg)
		}
	}
}

// valueProblem describes why v, a decoded JSON value called subject, is not a
// value of the setting's kind, or returns "" when it is.
func (s Setting) valueProblem(subject string, v any) string {
	switch s.Kind {
	case KindInt, KindFloat, KindSeed:
		f, ok := asNumber(v)
		if !ok {
			return subject + " must be a number"
		}
		whole := f == math.Trunc(f)
		switch {
		case s.Kind == KindSeed && (f < 0 || !whole):
			return fmt.Sprintf("%s %v must be a non-negative whole number", subject, f)
		case s.Kind == KindInt && !whole:
			return fmt.Sprintf("%s %v is not a whole number", subject, f)
		case s.boundsInverted():
			return ""
		case s.Min != nil && f < *s.Min:
			return fmt.Sprintf("%s %v is below min %v", subject, f, *s.Min)
		case s.Max != nil && f > *s.Max:
			return fmt.Sprintf("%s %v is above max %v", subject, f, *s.Max)
		}
	case KindString, KindImage:
		if _, ok := v.(string); !ok {
			return subject + " must be a string"
		}
	case KindBool:
		if _, ok := v.(bool); !ok {
			return subject + " must be true or false"
		}
	case KindCombo:
		str, ok := v.(string)
		if !ok {
			return subject + " must be a string"
		}
		if len(s.Options) > 0 && !slices.Contains(s.Options, str) {
			return fmt.Sprintf("%s is not one of the options (got %q)", subject, str)
		}
	}
	return ""
}

// asNumber reads a decoded JSON number, which is a float64, and an int for a
// manifest built in Go.
func asNumber(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	}
	return 0, false
}

func (ps *Problems) checkRequires(r Requires) {
	for _, class := range slices.Sorted(maps.Keys(r.Classes)) {
		where := fmt.Sprintf("requires.classes %q", class)
		if class == "" {
			ps.add(where, "class name is empty")
		}
		if hash := r.Classes[class]; !hexSumPattern.MatchString(hash) {
			ps.add(where, "hash must be sha256:<hex digits> (got %q)", hash)
		}
	}
	for _, key := range slices.Sorted(maps.Keys(r.Models)) {
		class, input, ok := strings.Cut(key, ".")
		if !ok || class == "" || input == "" {
			ps.add(fmt.Sprintf("requires.models %q", key), "key must be Class.input (got %q)", key)
		}
	}
}

func (ps *Problems) checkUse(where, id string, u Use) {
	if !idPattern.MatchString(id) {
		ps.add(where, "id must be namespace/name, both [a-z0-9-]+")
	}
	if !versionPattern.MatchString(u.Version) {
		ps.add(where, "version must be MAJOR.MINOR.PATCH, digits only (got %q)", u.Version)
	}
	if !hexSumPattern.MatchString(u.Digest) {
		ps.add(where, "digest must be sha256:<hex digits> (got %q)", u.Digest)
	}
}
