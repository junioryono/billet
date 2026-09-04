package a

import (
	"io"

	"github.com/junioryono/godi/v5"
)

type params struct {
	godi.In

	Out io.Writer
}

type thing struct{ out io.Writer }

func (t *thing) Close() error { return nil }

// Thing is the contract a dependent names.
type Thing interface{ Close() error }

// Exported is a concrete type that IS its contract.
type Exported struct{ out io.Writer }

// THE FINDING: an exported constructor hiding its result behind an unexported
// struct.
func NewHidden(p params) *thing { // want `NewHidden returns \*thing`
	return &thing{out: p.Out}
}

// THE INTERFACE IS THE SEAM.
func NewThing(p params) Thing {
	return &thing{out: p.Out}
}

// AN EXPORTED CONCRETE TYPE IS A CONTRACT TOO.
func NewExported(p params) *Exported {
	return &Exported{out: p.Out}
}

// AN UNEXPORTED CONSTRUCTOR IS REGISTERED ONLY FROM THIS PACKAGE.
func newThing(p params) *thing {
	return &thing{out: p.Out}
}

// A METHOD IS NOT A CONSTRUCTOR.
func (t *thing) NewChild() *thing { return t }

// A REASONED SUPPRESSION.
//
//billet:ignore godiconstructor // the struct is the container's own record and nothing depends on it
func NewRecord(p params) *thing {
	return &thing{out: p.Out}
}

var (
	_ = NewHidden
	_ = NewThing
	_ = NewExported
	_ = newThing
	_ = NewRecord
)
