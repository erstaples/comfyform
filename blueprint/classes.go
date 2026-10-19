package blueprint

import "github.com/erstaples/comfyform/schema"

// Classes tells compile and the checks what node classes a server has.
// comfyform's schema.Schema satisfies it through SchemaClasses; another tool
// adapts its own node library.
type Classes interface {
	// Class returns what the server knows of the named class, and false
	// when it has no such class.
	Class(classType string) (ClassInfo, bool)
}

// ClassInfo is one node class on a server: its inputs and outputs in the
// order the server lists them.
type ClassInfo struct {
	Inputs  []InputInfo
	Outputs []OutputInfo
}

// InputInfo is one input of a class. Widget is true for a value the node
// holds (INT, FLOAT, STRING, BOOLEAN, COMBO) and false for a socket a link
// fills.
type InputInfo struct {
	Name, Type       string
	Widget, Required bool
	Default          any
	HasDefault       bool
}

// OutputInfo is one output of a class.
type OutputInfo struct {
	Name, Type string
}

// SchemaClasses reads classes from a parsed /object_info. A nil schema has no
// classes.
func SchemaClasses(s *schema.Schema) Classes { return schemaClasses{s} }

type schemaClasses struct{ s *schema.Schema }

func (c schemaClasses) Class(classType string) (ClassInfo, bool) {
	if c.s == nil {
		return ClassInfo{}, false
	}
	sc := c.s.Classes[classType]
	if sc == nil {
		return ClassInfo{}, false
	}
	info := ClassInfo{
		Inputs:  make([]InputInfo, len(sc.Inputs)),
		Outputs: make([]OutputInfo, len(sc.Outputs)),
	}
	for i, in := range sc.Inputs {
		info.Inputs[i] = InputInfo{
			Name:       in.Name,
			Type:       in.Type,
			Widget:     in.Kind != schema.KindLink,
			Required:   in.Required,
			Default:    in.Default,
			HasDefault: in.HasDefault,
		}
	}
	for i, out := range sc.Outputs {
		info.Outputs[i] = OutputInfo{Name: out.Name, Type: out.Type}
	}
	return info, true
}
