package b

// A FILE THAT REGISTERS NOTHING WITH GODI keeps its own conventions; returning
// an unexported struct here is the package's business.

type thing struct{}

func NewThing() *thing { return &thing{} }

var _ = NewThing
