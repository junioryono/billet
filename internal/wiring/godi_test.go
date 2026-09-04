package wiring_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/junioryono/godi/v5"

	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/wiring"
)

// EVERY RULE BELOW IS ABOUT AN API BILLET DOES NOT OWN, so each is measured
// against the pinned godi rather than read from its documentation, with the
// date: godi v5.1.0, 2026-09-04. The shutdown ordering, the lifetime rule and
// the diagnostic rule are what the controller-term design and the secret types
// rest on; a godi upgrade that changed any of them fails here first.

type closeLog struct{ order []string }

type first struct{ log *closeLog }

func (f *first) Close() error {
	f.log.order = append(f.log.order, "first")

	return nil
}

type second struct{ log *closeLog }

func (s *second) Close() error {
	s.log.order = append(s.log.order, "second")

	return nil
}

type third struct{ log *closeLog }

func (t *third) Close() error {
	t.log.order = append(t.log.order, "third")

	return nil
}

type scopedThing struct{ log *closeLog }

func (s *scopedThing) Close() error {
	s.log.order = append(s.log.order, "scoped")

	return nil
}

// A CONTAINER CLOSES ITS SCOPES BEFORE ITS SINGLETONS, AND ITS SINGLETONS IN
// REVERSE CREATION ORDER. That is the order billet needs: the controller term
// (listeners, plane) is a scope and goes first; the ledger is constructed before
// everything that writes through it and so is closed after them, which is what
// keeps "writes stopped before the claim is released" true when the claim is
// released by DB.Close.
func TestGodiClosesScopesBeforeSingletonsInReverseCreationOrder(t *testing.T) {
	t.Parallel()

	log := &closeLog{}

	provider, err := wiring.Build(t.Context(),
		godi.AddSingleton(func() *closeLog { return log }),
		godi.AddSingleton(func(l *closeLog) *first { return &first{log: l} }),
		godi.AddSingleton(func(l *closeLog, _ *first) *second { return &second{log: l} }),
		godi.AddSingleton(func(l *closeLog, _ *second) *third { return &third{log: l} }),
		godi.AddScoped(func(l *closeLog, _ *third) *scopedThing { return &scopedThing{log: l} }),
	)
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	scope, err := provider.CreateScope(t.Context())
	if err != nil {
		t.Fatalf("create scope: %v", err)
	}

	if _, err := godi.Resolve[*scopedThing](scope); err != nil {
		t.Fatalf("resolve the scoped thing: %v", err)
	}

	if err := provider.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	want := []string{"scoped", "third", "second", "first"}
	if strings.Join(log.order, ",") != strings.Join(want, ",") {
		t.Fatalf("close order = %v, want %v", log.order, want)
	}
}

type eager struct{}

// SINGLETONS ARE CONSTRUCTED AT Build, NOT ON FIRST USE. A constructor that
// refuses (a ledger open meeting the release watermark) therefore refuses in
// Build, before any caller has a handle to anything, and a standby's claim
// cannot be a singleton constructor without blocking the build.
func TestGodiBuildsSingletonsEagerly(t *testing.T) {
	t.Parallel()

	constructed := 0

	provider, err := wiring.Build(t.Context(),
		godi.AddSingleton(func() *eager { constructed++; return &eager{} }),
	)
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	defer provider.Close()

	if constructed != 1 {
		t.Fatalf("the singleton was constructed %d times by Build, want 1", constructed)
	}
}

type needsDSN struct{}

type needsNothingRegistered struct{}

type unregistered struct{}

// A DIAGNOSTIC NAMES A SERVICE'S TYPE AND NEVER ITS VALUE. A constructor that
// took a state.DSN and failed produces an error naming state.DSN and not the
// password; a missing registration names the missing type. This is what makes
// registering a secret as its own type safe: the container has no path that
// renders the value.
func TestGodiDiagnosticsRenderTypesAndNeverValues(t *testing.T) {
	t.Parallel()

	const password = "hunter2-the-password"

	dsn := state.DSN("postgres://billet:" + password + "@db/billet")

	_, err := wiring.Build(t.Context(),
		godi.AddSingleton(func() state.DSN { return dsn }),
		godi.AddSingleton(func(state.DSN) (*needsDSN, error) {
			return nil, errors.New("the ledger refused")
		}),
	)
	if err == nil {
		t.Fatal("a failing constructor built")
	}

	for name, rendered := range map[string]string{
		"Error()": err.Error(),
		"%v":      fmt.Sprintf("%v", err),
		"%+v":     fmt.Sprintf("%+v", err),
		"%#v":     fmt.Sprintf("%#v", err),
	} {
		if strings.Contains(rendered, password) {
			t.Errorf("%s rendered the password: %s", name, rendered)
		}
	}

	if !strings.Contains(err.Error(), "needsDSN") {
		t.Errorf("the failure does not name the service that failed: %v", err)
	}

	if !strings.Contains(err.Error(), "the ledger refused") {
		t.Errorf("the failure does not carry the constructor's own error: %v", err)
	}

	// A MISSING REGISTRATION IS REFUSED AT BUILD, naming the missing type.
	_, err = wiring.Build(t.Context(),
		godi.AddSingleton(func(*unregistered) *needsNothingRegistered { return nil }),
	)
	if err == nil {
		t.Fatal("a graph with a missing registration built")
	}

	if !strings.Contains(err.Error(), "unregistered") {
		t.Errorf("the missing registration is not named: %v", err)
	}
}

type counter struct{ n int }

// A CHILD SCOPE DOES NOT SEE ITS PARENT'S SCOPED INSTANCES. This is the fact
// behind dropping the per-request scope: the controller term is a scope, and a
// request scope opened off it would construct a second plane, a second wire
// and a second claim rather than find the parent's.
func TestGodiChildScopesDoNotSeeTheirParentsScopedInstances(t *testing.T) {
	t.Parallel()

	built := 0

	provider, err := wiring.Build(t.Context(),
		godi.AddScoped(func() *counter { built++; return &counter{n: built} }),
	)
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	defer provider.Close()

	parent, err := provider.CreateScope(t.Context())
	if err != nil {
		t.Fatalf("create the parent scope: %v", err)
	}

	inParent, err := godi.Resolve[*counter](parent)
	if err != nil {
		t.Fatalf("resolve in the parent: %v", err)
	}

	again, err := godi.Resolve[*counter](parent)
	if err != nil {
		t.Fatalf("resolve in the parent again: %v", err)
	}

	if inParent != again {
		t.Fatal("two resolutions in one scope produced two instances")
	}

	child, err := parent.CreateScope(t.Context())
	if err != nil {
		t.Fatalf("create the child scope: %v", err)
	}

	inChild, err := godi.Resolve[*counter](child)
	if err != nil {
		t.Fatalf("resolve in the child: %v", err)
	}

	if inChild == inParent {
		t.Fatal("the child scope handed back the parent's instance; the controller-term " +
			"design assumes it does not, so re-read the ADR before relying on this")
	}

	if built != 2 {
		t.Fatalf("the scoped constructor ran %d times, want 2 (once per scope)", built)
	}
}

// A SINGLETON MAY NOT DEPEND ON A SCOPED SERVICE, and Build says so. This is
// the whole of what enforces "everything authoritative happens after the
// claim": the claim is scoped, so a listener registered as a singleton cannot
// depend on it and is refused here rather than started before it.
func TestGodiRefusesASingletonThatDependsOnAScopedService(t *testing.T) {
	t.Parallel()

	_, err := wiring.Build(t.Context(),
		godi.AddScoped(func() *counter { return &counter{} }),
		godi.AddSingleton(func(*counter) *eager { return &eager{} }),
	)

	conflict, ok := errors.AsType[*godi.LifetimeConflictError](err)
	if !ok {
		t.Fatalf("a singleton depending on a scoped service built, or failed for another "+
			"reason: %v", err)
	}

	if conflict.ServiceLifetime != godi.Singleton || conflict.DependencyLifetime != godi.Scoped {
		t.Fatalf("the conflict is not a singleton on a scoped service: %v", conflict)
	}
}

// A VARIADIC CONSTRUCTOR IS REFUSED, which is why every billet New(...,
// opts ...Option) is registered through a closure and its options through a
// group.
func TestGodiRefusesAVariadicConstructor(t *testing.T) {
	t.Parallel()

	_, err := wiring.Build(t.Context(),
		godi.AddSingleton(func(opts ...int) *eager { return &eager{} }),
	)
	if err == nil {
		t.Fatal("a variadic constructor was accepted; the closure rule in the modules is unnecessary")
	}
}

type groupConsumer struct {
	godi.In

	Members []int `group:"members"`
}

// A GROUP NOBODY JOINED IS AN EMPTY SLICE, not a build error, so a role set
// with no allocator options and no server options builds.
func TestGodiHandsAnEmptyGroupToItsConsumer(t *testing.T) {
	t.Parallel()

	var got []int

	provider, err := wiring.Build(t.Context(),
		godi.AddSingleton(func(p groupConsumer) *eager { got = p.Members; return &eager{} }),
	)
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	defer provider.Close()

	if len(got) != 0 {
		t.Fatalf("an empty group was handed %v", got)
	}

	provider, err = wiring.Build(t.Context(),
		godi.AddSingleton(func() int { return 7 }, godi.Group("members")),
		godi.AddSingleton(func() int { return 9 }, godi.Group("members")),
		godi.AddSingleton(func(p groupConsumer) *eager { got = p.Members; return &eager{} }),
	)
	if err != nil {
		t.Fatalf("build with members: %v", err)
	}

	defer provider.Close()

	if len(got) != 2 {
		t.Fatalf("a group of two was handed %v", got)
	}
}
