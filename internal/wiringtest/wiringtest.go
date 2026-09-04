// Package wiringtest builds a role's container for a test, the way cmd/billet
// builds it for a process, with the overrides a test needs.
//
// THE SAME MODULES, WITH A FAKE PUT IN PLACE OF ONE REGISTRATION. A test that
// hand-assembled its own graph would be testing a different program from the
// one that ships; the hand-copied adapter the end-to-end suite began with had
// already drifted once. So a test composes the role's own module set and
// replaces exactly the registration it must (the scale-set client with one
// pointed at a fake GitHub, the provider with a fake backend, the release with
// a version of its choosing) through Replace, which removes and re-adds by type.
// A fake registered this way must still return what it is told and judge
// nothing.
package wiringtest

import (
	"testing"

	"github.com/junioryono/godi/v5"

	"github.com/junioryono/billet/internal/wiring"
)

// Build assembles modules through wiring.Build, applies modify to the
// collection first, and closes the container when the test ends.
//
// modify runs BEFORE Build, on the collection, so an override can remove a
// registration and add its own; a fake added without removing is a duplicate
// registration, which godi refuses.
func Build(tb testing.TB, modules []godi.ModuleOption, modify func(c godi.Collection)) godi.Provider {
	tb.Helper()

	c := wiring.Collect(modules...)

	if modify != nil {
		modify(c)
	}

	provider, err := c.BuildWithContext(tb.Context())
	if err != nil {
		tb.Fatalf("build the container: %v", err)
	}

	tb.Cleanup(func() {
		if err := provider.Close(); err != nil {
			tb.Errorf("close the container: %v", err)
		}
	})

	return provider
}

// Replace registers ctor in place of whatever the modules registered for T.
//
// Remove first, because godi fails the build on a duplicate unkeyed
// registration; that refusal is what keeps "one of each" true, and this is the
// one sanctioned way past it.
func Replace[T any](ctor any) godi.ModuleOption {
	return godi.NewModule("replace",
		godi.Remove[T](),
		godi.AddSingleton(ctor),
	)
}

// Value registers a fixed value of type T as a singleton, replacing any
// registration the modules made for it.
func Value[T any](v T) godi.ModuleOption {
	return Replace[T](func() T { return v })
}

// Resolve resolves T from p and fails the test when it cannot.
func Resolve[T any](tb testing.TB, p godi.Provider) T {
	tb.Helper()

	v, err := godi.Resolve[T](p)
	if err != nil {
		tb.Fatalf("resolve %T: %v", v, err)
	}

	return v
}
