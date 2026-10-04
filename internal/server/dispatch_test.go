package server

import (
	"errors"
	"reflect"
	"testing"

	"github.com/junioryono/billet/internal/dispatch"
)

// THE OLD NAMES ARE THE NEW VALUES, NOT COPIES OF THEM. The node returns
// dispatch.ErrCustody and the plane tests for server.ErrCustody, so a sentinel
// declared afresh here would make errors.Is answer no across the two, and a
// lease the runner holds would be released.
func TestTheDispatchSentinelsAreTheSameValues(t *testing.T) {
	t.Parallel()

	for name, pair := range map[string][2]error{
		"ErrCustody":           {ErrCustody, dispatch.ErrCustody},
		"ErrHolderUnavailable": {ErrHolderUnavailable, dispatch.ErrHolderUnavailable},
	} {
		if pair[0] != pair[1] || !errors.Is(pair[0], pair[1]) { //nolint:errorlint // identity is the property under test
			t.Errorf("server.%s is not dispatch.%s itself", name, name)
		}
	}
}

// AND THE OLD TYPE NAMES ARE ALIASES, NOT NEW TYPES. For the interfaces the
// compiler would not say so: one re-declared as `type Runner dispatch.Runner` is
// still satisfied by every implementation, because satisfaction is structural,
// yet it is a type of its own that a type switch or a reflect comparison tells
// apart. A struct re-declared that way fails to compile at the listener's calls,
// and is checked here only so all five read the same.
func TestTheDispatchTypesAreAliases(t *testing.T) {
	t.Parallel()

	for name, pair := range map[string][2]reflect.Type{
		"Runner":                     {reflect.TypeFor[Runner](), reflect.TypeFor[dispatch.Runner]()},
		"CompletionAwareRunner":      {reflect.TypeFor[CompletionAwareRunner](), reflect.TypeFor[dispatch.CompletionAwareRunner]()},
		"BoundCompletionAwareRunner": {reflect.TypeFor[BoundCompletionAwareRunner](), reflect.TypeFor[dispatch.BoundCompletionAwareRunner]()},
		"Job":                        {reflect.TypeFor[Job](), reflect.TypeFor[dispatch.Job]()},
		"CacheAuthority":             {reflect.TypeFor[CacheAuthority](), reflect.TypeFor[dispatch.CacheAuthority]()},
	} {
		if pair[0] != pair[1] {
			t.Errorf("server.%s is %v, a type of its own, not dispatch.%s", name, pair[0], name)
		}
	}
}
