package node

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/provider"
)

// planeSwitch answers the kill switch as a plane does: allowing, refusing, or
// not answering at all, as a restarting controller does.
type planeSwitch struct {
	mode  atomic.Int32
	asked atomic.Int32
}

const (
	switchAllows int32 = iota
	switchRefuses
	switchUnreachable
)

var errPlaneUnreachable = errors.New("dial tcp 10.3.1.117:7717: connect: no route to host")

func (p *planeSwitch) CacheAllowed(context.Context, config.CacheKind, string, string) (bool, error) {
	p.asked.Add(1)
	switch p.mode.Load() {
	case switchAllows:
		return true, nil
	case switchRefuses:
		return false, nil
	default:
		return false, errPlaneUnreachable
	}
}

func (p *planeSwitch) ActionsCacheAllowed(ctx context.Context, owner, repository string) (bool, error) {
	return p.CacheAllowed(ctx, config.CacheActions, owner, repository)
}

func TestPolicyMemoryKeepsAnAllowedAnswerForItsAgeOnly(t *testing.T) {
	t.Parallel()

	var memory policyMemory
	start := time.Date(2026, 11, 9, 4, 46, 0, 0, time.UTC)
	memory.note(config.CacheNpm, "Acme", "API", true, start)

	if _, ok := memory.standing(config.CacheNpm, "acme", "api", start.Add(policyRememberedFor-time.Second)); !ok {
		t.Fatal("an allowed answer younger than its age does not stand (names folded)")
	}
	if _, ok := memory.standing(config.CacheNpm, "acme", "api", start.Add(policyRememberedFor)); ok {
		t.Fatal("an allowed answer as old as its age still stands")
	}
	if _, ok := memory.standing(config.CacheGo, "acme", "api", start); ok {
		t.Fatal("an answer about npm stands for the Go cache")
	}
	if _, ok := memory.standing(config.CacheNpm, "acme", "web", start); ok {
		t.Fatal("an answer about one repository stands for another")
	}
	if _, ok := memory.standing(config.CacheNpm, "acme", "api", start.Add(-time.Second)); ok {
		t.Fatal("an answer from a clock now behind it stands")
	}
}

func TestPolicyMemoryForgetsAnAllowedAnswerWhenTheSwitchRefuses(t *testing.T) {
	t.Parallel()

	var memory policyMemory
	start := time.Date(2026, 11, 9, 4, 46, 0, 0, time.UTC)
	memory.note(config.CacheNpm, "acme", "api", true, start)
	memory.note(config.CacheNpm, "ACME", "api", false, start.Add(time.Second))

	if _, ok := memory.standing(config.CacheNpm, "acme", "api", start.Add(2*time.Second)); ok {
		t.Fatal("an allowed answer outlived a later refusal")
	}
}

func TestPolicyMemoryDropsAnswersPastTheirAge(t *testing.T) {
	t.Parallel()

	var memory policyMemory
	start := time.Date(2026, 11, 9, 4, 46, 0, 0, time.UTC)
	memory.note(config.CacheNpm, "acme", "old", true, start)
	memory.note(config.CacheNpm, "acme", "new", true, start.Add(policyRememberedFor))

	memory.mu.Lock()
	defer memory.mu.Unlock()
	if _, kept := memory.allowed[newPolicyKey(config.CacheNpm, "acme", "old")]; kept {
		t.Fatal("an answer past its age is still held: the memory grows with every repository ever asked")
	}
}

// SERVING A JOB OUTLIVES A CONTROLLER RESTART; a publication does not. The
// plane allows, then stops answering: serving goes on by the last answer until
// it is policyRememberedFor old, and a publication's ask is could-not-tell
// throughout.
func TestServingStandsOnTheLastAllowedAnswerWhileThePlaneIsDown(t *testing.T) {
	t.Parallel()

	service, _, _, _ := casService(t, provider.TrustTrusted, goBazelCache(), &fakeCacheStore{})
	plane := &planeSwitch{}
	service.SetCachePolicy(plane)
	now := time.Date(2026, 11, 9, 4, 46, 0, 0, time.UTC)
	service.now = func() time.Time { return now }
	ctx := t.Context()

	if got := service.servingPolicy(ctx, config.CacheNpm, "acme", "api"); got != policyAllowed {
		t.Fatalf("serving while the plane allows = %v", got)
	}
	plane.mode.Store(switchUnreachable)
	now = now.Add(policyRememberedFor - time.Second)
	if got := service.servingPolicy(ctx, config.CacheNpm, "acme", "api"); got != policyAllowed {
		t.Fatalf("serving with the plane down inside the age = %v, want allowed", got)
	}
	if got := service.kindPolicy(ctx, config.CacheNpm, "acme", "api"); got != policyUnknown {
		t.Fatalf("a publication's ask with the plane down = %v, want could-not-tell", got)
	}
	if got := service.servingPolicy(ctx, config.CacheNpm, "acme", "web"); got != policyUnknown {
		t.Fatalf("serving another repository the plane never allowed = %v, want could-not-tell", got)
	}
	now = now.Add(time.Second)
	if got := service.servingPolicy(ctx, config.CacheNpm, "acme", "api"); got != policyUnknown {
		t.Fatalf("serving with the plane down past the age = %v, want could-not-tell", got)
	}
	if plane.asked.Load() != 5 {
		t.Fatalf("the plane was asked %d times, want every call to ask it first", plane.asked.Load())
	}
}

// A BLOCK THAT HAS REACHED THE NODE IS NEVER OUTLIVED by an older "allowed".
func TestServingDoesNotOutliveARefusalThePlaneGave(t *testing.T) {
	t.Parallel()

	service, _, _, _ := casService(t, provider.TrustTrusted, goBazelCache(), &fakeCacheStore{})
	plane := &planeSwitch{}
	service.SetCachePolicy(plane)
	now := time.Date(2026, 11, 9, 4, 46, 0, 0, time.UTC)
	service.now = func() time.Time { return now }
	ctx := t.Context()

	service.servingPolicy(ctx, config.CacheNpm, "acme", "api")
	plane.mode.Store(switchRefuses)
	now = now.Add(time.Second)
	if got := service.servingPolicy(ctx, config.CacheNpm, "acme", "api"); got != policyRefused {
		t.Fatalf("serving while the plane refuses = %v", got)
	}
	plane.mode.Store(switchUnreachable)
	now = now.Add(time.Second)
	if got := service.servingPolicy(ctx, config.CacheNpm, "acme", "api"); got != policyUnknown {
		t.Fatalf("serving with the plane down after it refused = %v, want could-not-tell", got)
	}
}

// A PUBLICATION'S OWN ALLOWED ANSWER IS REMEMBERED TOO: the memory is of what
// the plane said, whoever asked it.
func TestAPublicationsAllowedAnswerStandsForServing(t *testing.T) {
	t.Parallel()

	service, _, _, _ := casService(t, provider.TrustTrusted, goBazelCache(), &fakeCacheStore{})
	plane := &planeSwitch{}
	service.SetCachePolicy(plane)
	now := time.Date(2026, 11, 9, 4, 46, 0, 0, time.UTC)
	service.now = func() time.Time { return now }
	ctx := t.Context()

	if got := service.kindPolicy(ctx, config.CacheGo, "acme", "api"); got != policyAllowed {
		t.Fatalf("a publication's ask while the plane allows = %v", got)
	}
	plane.mode.Store(switchUnreachable)
	if got := service.servingPolicy(ctx, config.CacheGo, "acme", "api"); got != policyAllowed {
		t.Fatalf("serving after a publication's allowed answer, plane down = %v", got)
	}
}

// THROUGH A REAL REQUEST: a running Go cache's next write after casPolicyAge
// asks the switch again, and with the plane down it is served by the last
// "allowed" until that is too old, then refused.
func TestACacheInUseIsServedThroughAControllerRestart(t *testing.T) {
	t.Parallel()

	spec := goBazelCache()
	spec.Owner, spec.Repository = "acme", "api"
	service, _, token, _ := casService(t, provider.TrustTrusted, spec, &fakeCacheStore{})
	plane := &planeSwitch{}
	service.SetCachePolicy(plane)
	now := time.Now()
	service.now = func() time.Time { return now }

	if code := putObject(t, service, token, "before the restart"); code != http.StatusOK {
		t.Fatalf("PUT with the plane answering = %d", code)
	}
	plane.mode.Store(switchUnreachable)
	now = now.Add(casPolicyAge)
	asked := plane.asked.Load()
	if code := putObject(t, service, token, "during the restart"); code != http.StatusOK {
		t.Fatalf("PUT with the plane down inside the age = %d, want 200", code)
	}
	if plane.asked.Load() == asked {
		t.Fatal("the write was not asked of the switch again, so the memory decided nothing")
	}
	now = now.Add(policyRememberedFor)
	if code := putObject(t, service, token, "long after"); code == http.StatusOK {
		t.Fatalf("PUT with the plane down past the age = %d, want refused", code)
	}
}

// THE ACTIONS CACHE OF A TRUSTED-ONLY SESSION reads the switch through its own
// reader, and keeps the same memory.
func TestTheActionsCacheKeepsItsLastAllowedAnswer(t *testing.T) {
	t.Parallel()

	service, _, _, _ := casService(t, provider.TrustTrusted, goBazelCache(), &fakeCacheStore{})
	plane := &planeSwitch{}
	service.SetActionsPolicy(plane)
	now := time.Date(2026, 11, 9, 4, 46, 0, 0, time.UTC)
	service.now = func() time.Time { return now }
	session := &cacheSession{trust: provider.TrustTrusted, owner: "acme", repository: "api",
		workflowRef: "acme/api/.github/workflows/ci.yml@refs/heads/main"}

	if _, disposition := service.actionsKeysFor(t.Context(), session); disposition != "" {
		t.Fatalf("with the plane allowing, disposition = %q, want served", disposition)
	}
	plane.mode.Store(switchUnreachable)
	now = now.Add(policyRememberedFor - time.Second)
	if _, disposition := service.actionsKeysFor(t.Context(), session); disposition != "" {
		t.Fatalf("with the plane down inside the age, disposition = %q, want served", disposition)
	}
	now = now.Add(time.Second)
	keys, disposition := service.actionsKeysFor(t.Context(), session)
	if disposition != "spliced" || keys.write != nil {
		t.Fatalf("with the plane down past the age, disposition = %q, want spliced", disposition)
	}
	plane.mode.Store(switchRefuses)
	if _, disposition := service.actionsKeysFor(t.Context(), session); disposition != "disabled" {
		t.Fatalf("with the plane refusing, disposition = %q, want disabled", disposition)
	}
}
