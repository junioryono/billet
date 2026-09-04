package a

import (
	"io"
	"log/slog"

	"github.com/junioryono/godi/v5"
)

// EVERY `want` BELOW IS A FIELD THE ANALYZER MUST FIND, and the cases after them
// are shapes it must stay quiet about.

type unexplained struct {
	godi.In

	Log   *slog.Logger
	Store io.Closer `optional:"true"` // want `Store is optional`
}

type tagAmongOthers struct {
	godi.In

	Store io.Closer `name:"x" optional:"true"` // want `Store is optional`
}

// A REQUIRED FIELD IS THE ORDINARY CASE, and an optional:"false" is not optional.
type required struct {
	godi.In

	Log   *slog.Logger
	Store io.Closer `optional:"false"`
}

// A STRUCT THAT EMBEDS NOTHING GODI READS carries the tag for no reason godi
// cares about, so it is not this analyzer's.
type plain struct {
	Store io.Closer `optional:"true"`
}

// A REASONED DIRECTIVE ON ITS OWN LINE COVERS THE FIELD BELOW IT.
type explained struct {
	godi.In

	//billet:ignore godioptional // the zero value refuses: a nil store keeps the authority as files
	Store io.Closer `optional:"true"`
}

// A TRAILING DIRECTIVE COVERS ITS OWN LINE.
type explainedTrailing struct {
	godi.In

	Store io.Closer `optional:"true"` //billet:ignore godioptional // the zero value refuses: nil means no store
}

// A BARE DIRECTIVE IS ITSELF REPORTED, and that case lives in the suppress
// package's own tests: an expectation marker here would read as the reason.

// AN UNUSED DIRECTIVE IS REPORTED TOO.
type stale struct {
	godi.In

	//billet:ignore godioptional // the zero value refuses: nothing, this field is required now // want `suppresses nothing`
	Store io.Closer
}

var (
	_ = unexplained{}
	_ = tagAmongOthers{}
	_ = required{}
	_ = plain{}
	_ = explained{}
	_ = explainedTrailing{}
	_ = stale{}
)
