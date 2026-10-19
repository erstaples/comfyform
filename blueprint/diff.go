package blueprint

import (
	"cmp"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
)

// Level is how much a change to a blueprint's interface asks of the version
// number: Patch for nothing, Minor for an addition, Major for a break.
type Level int

const (
	Patch Level = iota
	Minor
	Major
)

func (l Level) String() string {
	switch l {
	case Patch:
		return "patch"
	case Minor:
		return "minor"
	case Major:
		return "major"
	}
	return fmt.Sprintf("Level(%d)", int(l))
}

// Change is one difference between the interfaces of two blueprint versions.
type Change struct {
	Level Level
	What  string
}

type changes []Change

func (cs *changes) add(level Level, format string, args ...any) {
	*cs = append(*cs, Change{Level: level, What: fmt.Sprintf(format, args...)})
}

// Diff lists the interface differences that matter to a graph using the
// blueprint, from old to next: inputs, outputs, settings, then the result.
// Ports and settings match by name, so a rename is a removal plus an addition.
// What the blueprint does inside, and a setting's default, step, title and
// description, are not part of the interface.
func Diff(old, next *Manifest) []Change {
	var cs changes
	diffInputs(&cs, old.Inputs, next.Inputs)
	diffOutputs(&cs, old.Outputs, next.Outputs)
	diffSettings(&cs, old.Settings, next.Settings)
	diffResult(&cs, old.Result, next.Result)
	return cs
}

// LevelOf is the highest level among changes, and Patch when there are none.
func LevelOf(cs []Change) Level {
	level := Patch
	for _, c := range cs {
		level = max(level, c.Level)
	}
	return level
}

func portsByName(ports []Port) map[string]Port {
	m := make(map[string]Port, len(ports))
	for _, p := range ports {
		m[p.Name] = p
	}
	return m
}

func diffInputs(cs *changes, old, next []Port) {
	oldByName, nextByName := portsByName(old), portsByName(next)
	for _, o := range old {
		n, ok := nextByName[o.Name]
		if !ok {
			cs.add(Major, "input %q removed", o.Name)
			continue
		}
		if o.Type != n.Type {
			cs.add(Major, "input %q changed from %s to %s", o.Name, o.Type, n.Type)
		}
		switch {
		case o.Optional && !n.Optional:
			cs.add(Major, "input %q is now required", o.Name)
		case !o.Optional && n.Optional:
			cs.add(Minor, "input %q is now optional", o.Name)
		}
	}
	for _, n := range next {
		switch _, ok := oldByName[n.Name]; {
		case ok:
		case n.Optional:
			cs.add(Minor, "input %q added", n.Name)
		default:
			cs.add(Major, "input %q added and required", n.Name)
		}
	}
}

// diffOutputs reads outputs by position as well as by name, because an
// instance's links name an output by its index: only an output added after
// every existing one leaves the others where they were.
func diffOutputs(cs *changes, old, next []Port) {
	oldByName, nextByName := portsByName(old), portsByName(next)
	var kept []string // names in both versions, in the old order
	for _, o := range old {
		n, ok := nextByName[o.Name]
		if !ok {
			cs.add(Major, "output %q removed", o.Name)
			continue
		}
		if o.Type != n.Type {
			cs.add(Major, "output %q changed from %s to %s", o.Name, o.Type, n.Type)
		}
		kept = append(kept, o.Name)
	}

	// last is the position in next of its last output that old also has.
	last := -1
	for i, n := range next {
		if _, ok := oldByName[n.Name]; ok {
			last = i
		}
	}
	reordered := false
	k := 0
	for i, n := range next {
		if _, ok := oldByName[n.Name]; ok {
			reordered = reordered || k >= len(kept) || kept[k] != n.Name
			k++
		} else if i < last {
			reordered = true // added in front of an existing output
		}
	}
	if reordered {
		cs.add(Major, "outputs reordered")
	}
	for i, n := range next {
		if _, ok := oldByName[n.Name]; !ok && i > last {
			cs.add(Minor, "output %q added", n.Name)
		}
	}
}

func diffSettings(cs *changes, old, next []Setting) {
	oldNames := map[string]bool{}
	nextByName := map[string]Setting{}
	for _, s := range old {
		oldNames[s.Name] = true
	}
	for _, s := range next {
		nextByName[s.Name] = s
	}
	for _, o := range old {
		if n, ok := nextByName[o.Name]; ok {
			diffSetting(cs, o, n)
		} else {
			cs.add(Major, "setting %q removed", o.Name)
		}
	}
	for _, n := range next {
		switch {
		case oldNames[n.Name]:
		case n.hasDefault():
			cs.add(Minor, "setting %q added", n.Name)
		default:
			cs.add(Major, "setting %q added without a default", n.Name)
		}
	}
}

// hasDefault reports whether a form can fill the setting in without asking:
// it has a default, or it is an image or a list, which start empty.
func (s Setting) hasDefault() bool {
	return s.Default != nil || s.Kind == KindImage || s.List != nil
}

func diffSetting(cs *changes, o, n Setting) {
	retyped := o.Kind != n.Kind
	if retyped {
		cs.add(Major, "setting %q changed from %s to %s", o.Name, o.Kind, n.Kind)
	}
	switch {
	case o.List == nil && n.List != nil:
		cs.add(Major, "setting %q is now a list", o.Name)
	case o.List != nil && n.List == nil:
		cs.add(Major, "setting %q is no longer a list", o.Name)
	}
	if retyped || (o.List == nil) != (n.List == nil) {
		return // what a range or an option means depends on the kind
	}

	sourceChanged := o.Kind == KindCombo && !sameSource(o.Source, n.Source)
	if sourceChanged {
		cs.add(Major, "setting %q source changed", o.Name)
	}
	narrowed, widened := boundsMoved(o, n)
	if o.Kind == KindCombo && !sourceChanged {
		optNarrowed, optWidened := optionsMoved(o.Options, n.Options)
		narrowed, widened = narrowed || optNarrowed, widened || optWidened
	}
	if narrowed {
		cs.add(Major, "setting %q range narrowed", o.Name)
	}
	if widened {
		cs.add(Minor, "setting %q range widened", o.Name)
	}

	switch ol, nl := o.IsLinkable(), n.IsLinkable(); {
	case ol && !nl:
		cs.add(Major, "setting %q no longer linkable", o.Name)
	case !ol && nl:
		cs.add(Minor, "setting %q now linkable", o.Name)
	}
}

func sameSource(a, b *ModelSource) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// boundsMoved reports whether the numbers or the list lengths a setting
// accepts shrank, grew, or both (a min raised while the max is raised too). A
// bound that was not there counts as infinitely wide, so adding one narrows
// and dropping one widens.
func boundsMoved(o, n Setting) (narrowed, widened bool) {
	// note takes how a new bound compares with the old one, oriented so that
	// a negative result is a narrower range.
	note := func(c int) {
		narrowed = narrowed || c < 0
		widened = widened || c > 0
	}
	switch o.Kind {
	case KindInt, KindFloat, KindSeed:
		note(cmp.Compare(boundOr(o.Min, math.Inf(-1)), boundOr(n.Min, math.Inf(-1))))
		note(cmp.Compare(boundOr(n.Max, math.Inf(1)), boundOr(o.Max, math.Inf(1))))
	}
	if o.List != nil && n.List != nil {
		note(cmp.Compare(o.List.Min, n.List.Min))
		note(cmp.Compare(n.List.Max, o.List.Max))
	}
	return narrowed, widened
}

func boundOr(p *float64, none float64) float64 {
	if p == nil {
		return none
	}
	return *p
}

// optionsMoved reports whether a combo lost an option, gained one, or both.
func optionsMoved(old, next []string) (narrowed, widened bool) {
	narrowed = slices.ContainsFunc(old, func(opt string) bool { return !slices.Contains(next, opt) })
	widened = slices.ContainsFunc(next, func(opt string) bool { return !slices.Contains(old, opt) })
	return narrowed, widened
}

func diffResult(cs *changes, old, next string) {
	switch {
	case old == next:
	case old == "":
		cs.add(Minor, "result changed")
	default:
		cs.add(Major, "result changed")
	}
}

// version is the three numbers of a MAJOR.MINOR.PATCH string.
type version [3]int

func parseVersion(s string) (version, error) {
	var v version
	if !versionPattern.MatchString(s) {
		return v, fmt.Errorf("version %q is not MAJOR.MINOR.PATCH", s)
	}
	for i, p := range strings.Split(s, ".") {
		n, err := strconv.Atoi(p)
		if err != nil {
			return v, fmt.Errorf("version %q: %w", s, err)
		}
		v[i] = n
	}
	return v, nil
}

// bump is the level of the step from old up to next, which must be above it.
func (v version) bump(old version) Level {
	switch {
	case v[0] > old[0]:
		return Major
	case v[1] > old[1]:
		return Minor
	}
	return Patch
}

// CheckVersion accepts next as the successor of old when it names the same
// blueprint, its version is above old's, and the step up is at least as big as
// the interface difference needs: a major step for a break, a minor step for
// an addition.
func CheckVersion(old, next *Manifest) error {
	if old.ID != next.ID {
		return fmt.Errorf("ids differ: %s and %s", old.ID, next.ID)
	}
	ov, err := parseVersion(old.Version)
	if err != nil {
		return err
	}
	nv, err := parseVersion(next.Version)
	if err != nil {
		return err
	}
	if slices.Compare(nv[:], ov[:]) <= 0 {
		return fmt.Errorf("%s is not above %s", next.Version, old.Version)
	}
	diff := Diff(old, next)
	need := LevelOf(diff)
	if nv.bump(ov) >= need {
		return nil
	}
	// A step below need means need is above Patch, so some change has it.
	first := diff[slices.IndexFunc(diff, func(c Change) bool { return c.Level == need })]
	return fmt.Errorf("%s understates the change: %s needs a %s version", next.Version, first.What, need)
}
