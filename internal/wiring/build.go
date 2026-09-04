package wiring

import (
	"context"
	"errors"
	"reflect"

	"github.com/junioryono/godi/v5"
)

// Build assembles a container from modules and constructs every singleton in it.
//
// THE ONE PLACE A COLLECTION IS CREATED. Every role's entry point, every
// operator command and every test that assembles a deployment comes through
// here, which is what makes "the same modules assemble production and tests" a
// fact rather than a rule somebody remembers; forbidigo refuses
// godi.NewCollection anywhere else.
//
// Singletons are constructed EAGERLY, in dependency order, before this returns
// (measured in godi v5.1.0's collection.go, phase 6). A constructor that
// refuses, such as a ledger open that meets the release watermark, therefore
// refuses here, before any caller has a handle to anything.
func Build(ctx context.Context, modules ...godi.ModuleOption) (godi.Provider, error) {
	provider, err := Collect(modules...).BuildWithContext(ctx)
	if err != nil {
		return nil, describe(err)
	}

	return provider, nil
}

// Collect registers modules without building them, so a test can enumerate
// exactly what a role's set declares and resolve every entry.
func Collect(modules ...godi.ModuleOption) godi.Collection {
	c := godi.NewCollection()
	c.AddModules(modules...)

	return c
}

// BuildError is a container that could not be assembled: the service that
// failed, by TYPE, and the constructor's own error underneath.
//
// BY TYPE, NEVER BY VALUE. godi's own diagnostics render a service's type and
// not its value (measured, and pinned in godi_test.go), and this keeps to the
// same rule, so a failure to construct something that took a state.DSN or a
// github.AppKey names those types and prints neither. Unwrap reaches the
// constructor's error, so a caller can still ask errors.Is for
// state.ErrReleaseBehind or ErrNoLedgerYet.
type BuildError struct {
	Service string
	Err     error
}

func (e *BuildError) Error() string {
	if e.Service == "" {
		return "wiring: build: " + e.Err.Error()
	}

	return "wiring: assembling " + e.Service + ": " + e.Err.Error()
}

func (e *BuildError) Unwrap() error { return e.Err }

// describe names the failed service and surfaces its constructor's error.
//
// godi wraps a constructor failure as BuildError → ResolutionError →
// ConstructorInvocationError → cause. The cause is what an operator can act on
// ("server state: another billet process holds this state directory"); the
// wrapping is what a developer reading a graph failure needs, and a missing
// registration has no cause to unwrap to, so it keeps godi's whole text.
func describe(err error) error {
	out := &BuildError{Err: err}

	if resolution, ok := errors.AsType[*godi.ResolutionError](err); ok {
		out.Service = typeName(resolution.ServiceType)
	}

	if invocation, ok := errors.AsType[*godi.ConstructorInvocationError](err); ok &&
		invocation.Cause != nil {
		out.Err = constructorsOwn(invocation.Cause)
	}

	return out
}

// constructorsOwn strips the one layer godi adds around a constructor's error
// ("constructor error: %w", measured in v5.1.0's reflection/builders.go), so an
// operator reads the sentence billet wrote. Anything else is left as it is.
func constructorsOwn(cause error) error {
	inner := errors.Unwrap(cause)
	if inner != nil && cause.Error() == "constructor error: "+inner.Error() {
		return inner
	}

	return cause
}

func typeName(t reflect.Type) string {
	if t == nil {
		return ""
	}

	return t.String()
}
