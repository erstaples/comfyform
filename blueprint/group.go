package blueprint

import (
	"maps"
	"slices"

	"github.com/erstaples/comfyform/graph"
)

// binding is one input of a node group's node that a port or a setting
// fills: exactly one of port and setting is set.
type binding struct {
	input   string
	port    string
	setting string
}

// groupBindings lists, for each node of a group, the inputs that ports and
// settings fill: ports first, each in manifest order.
func groupBindings(m *Manifest) map[string][]binding {
	bs := map[string][]binding{}
	for _, p := range m.Inputs {
		for _, t := range p.To {
			bs[t.NodeID] = append(bs[t.NodeID], binding{input: t.Input, port: p.Name})
		}
	}
	for _, s := range m.Settings {
		for _, t := range s.To {
			bs[t.NodeID] = append(bs[t.NodeID], binding{input: t.Input, setting: s.Name})
		}
	}
	return bs
}

// expandGroup copies each node of a node group under the frame's prefix:
// links between the group's nodes point at their copies, port targets take
// the port's socket (an optional port with nothing behind it drops the
// input), and setting targets take the setting's value. A node that is a
// blueprint instance is expanded under the prefix and its id, and a link to
// it takes what its output port resolved to. Each repeat is unrolled into
// its copies, and an output port takes what its socket resolves to, which
// for a carried output is the last copy's.
func (f *frame) expandGroup(pkg *Package, inputs map[string]*ref, settings map[string]any) (map[string]*ref, error) {
	ids := pkg.Group.IDs()
	for _, id := range ids {
		if pkg.Group[id] == nil {
			return nil, f.errorf("", "%s node %s is null", groupFile, id)
		}
	}
	u, err := f.unroll(pkg, inputs, settings)
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		i, repeated := u.owner[id]
		if repeated {
			// A repeat's copies go out together, where its first node is.
			if u.nodes[i][0] == id {
				if err := u.emitCopies(i); err != nil {
					return nil, err
				}
			}
			continue
		}
		n := pkg.Group[id]
		if isInstance(n) {
			if _, err := u.nested(-1, 0, id); err != nil {
				return nil, err
			}
			continue
		}
		in, err := f.groupInputs(pkg.Group, id, u.bindings[id], u.linkTo(-1, 0), inputs, settings)
		if err != nil {
			return nil, err
		}
		if _, err := f.emit(id, &graph.Node{Class: n.Class, Inputs: in, Meta: n.Meta}); err != nil {
			return nil, err
		}
	}

	outs := make(map[string]*ref, len(pkg.Manifest.Outputs))
	for _, o := range pkg.Manifest.Outputs {
		if o.From == nil || pkg.Group[o.From.NodeID] == nil {
			return nil, f.errorf("", "output %s comes from no node of %s", o.Name, groupFile)
		}
		r, err := u.resolve(o.From.NodeID, o.From.Output, nil)
		if err != nil {
			return nil, err
		}
		outs[o.Name] = r
	}
	return outs, nil
}

// groupInputs builds the inputs of the copy of group node id. linkTo gives
// the compiled socket for a link to another node of the group; a link with
// nothing behind it is an error.
func (f *frame) groupInputs(group graph.Prompt, id string, bound []binding, linkTo func(node string, out int) (*ref, error),
	inputs map[string]*ref, settings map[string]any) (map[string]any, error) {
	n := group[id]
	in := make(map[string]any, len(n.Inputs))
	for _, k := range slices.Sorted(maps.Keys(n.Inputs)) {
		if slices.ContainsFunc(bound, func(b binding) bool { return b.input == k }) {
			continue // filled below
		}
		v := n.Inputs[k]
		if node, out, ok := graph.AsLink(v); ok {
			r, err := f.groupRef(group, id, k, node, out, linkTo)
			switch {
			case err != nil:
				return nil, err
			case r == nil:
				return nil, f.errorf("", "node %s input %s links output %d of node %s, which has nothing behind it", id, k, out, node)
			}
			v = r.link()
		}
		in[k] = v
	}
	for _, b := range bound {
		if b.port != "" {
			if r := inputs[b.port]; r != nil {
				in[b.input] = r.link()
			} else {
				delete(in, b.input)
			}
			continue
		}
		switch v := settings[b.setting].(type) {
		case *ref:
			if v == nil {
				return nil, f.errorf(b.setting, "setting %s has nothing behind it", b.setting)
			}
			in[b.input] = v.link()
		default:
			in[b.input] = v
		}
	}
	return in, nil
}

// groupRef is the compiled socket behind input k of group node id, which
// links output out of node: an error unless node is a node of the group,
// else what linkTo gives.
func (f *frame) groupRef(group graph.Prompt, id, k, node string, out int, linkTo func(node string, out int) (*ref, error)) (*ref, error) {
	switch {
	case node == inLinkID:
		return nil, f.errorf("", "node %s input %s links %q, and no input port feeds it", id, k, inLinkID)
	case group[node] == nil:
		return nil, f.errorf("", "node %s input %s links node %s, which is not in %s", id, k, node, groupFile)
	}
	return linkTo(node, out)
}
