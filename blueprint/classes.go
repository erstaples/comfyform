package blueprint

// Classes tells a check what node classes a server has.
type Classes interface {
	// Class returns what the server knows of the named class, and false
	// when it has no such class.
	Class(name string) (ClassInfo, bool)
}

// ClassInfo is one node class on a server: its inputs and outputs, and the
// interface hash. It has no fields until class checks read them.
type ClassInfo struct{}
