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

// AND THE OLD TYPE NAMES ARE ALIASES, NOT NEW TYPES. The compiler would not say
// so: an interface re-declared as `type Runner dispatch.Runner` is still
// satisfied by every implementation, because interfaces are structural, and a
// struct re-declared the same way converts silently at most call sites. Type
// identity is what a type switch, a reflect comparison and an interface
// holding one of them all depend on.
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
