package node

import (
	"strings"
	"sync"
	"time"

	"github.com/junioryono/billet/internal/config"
)

// policyRememberedFor is how long the kill switch's last "allowed" for a cache
// of a repository stands for serving a job while the control plane cannot be
// asked. It covers a controller's restart: on 2026-11-09 an upgrade left the
// plane unable to answer the node for 100 s, and from stopping to serving again
// took 3 min 40 s, and every cache read in that window went to the internet.
const policyRememberedFor = 10 * time.Minute

// policyMemory is the kill switch's last answer that allowed a cache, per
// cache and repository, for this process only.
//
// ONLY AN ALLOWED ANSWER IS KEPT, and a refusal read later removes it, so an
// operator's block that has reached the node is never outlived by an older
// "allowed". What it covers is a switch the node could not have read anyway:
// one set while the plane was not answering takes effect at the plane's next
// answer, or policyRememberedFor after the last "allowed" at the latest.
type policyMemory struct {
	mu      sync.Mutex
	allowed map[policyKey]time.Time
	// refusals counts the refusals noted. An ask records the count before it
	// goes out, and its "allowed" is kept only if none was noted meanwhile: an
	// answer that left the plane before a block reached it must not outlive a
	// refusal that left after it.
	refusals uint64
}

// policyKey names one cache of one repository. A block names organisations and
// repositories case-insensitively, so the key folds case.
type policyKey struct {
	kind       config.CacheKind
	owner      string
	repository string
}

func newPolicyKey(kind config.CacheKind, owner, repository string) policyKey {
	return policyKey{kind: kind, owner: strings.ToLower(owner), repository: strings.ToLower(repository)}
}

// policyAsk is one question to the plane: when it was asked, and how many
// refusals had been noted by then.
type policyAsk struct {
	at       time.Time
	refusals uint64
}

// begin is taken before an ask goes out.
func (m *policyMemory) begin(at time.Time) policyAsk {
	m.mu.Lock()
	defer m.mu.Unlock()

	return policyAsk{at: at, refusals: m.refusals}
}

// note records what the plane answered to an ask. An "allowed" is dated when
// it was asked, not when it arrived, since the plane answered for that moment.
func (m *policyMemory) note(kind config.CacheKind, owner, repository string, allowed bool, ask policyAsk) {
	key := newPolicyKey(kind, owner, repository)
	m.mu.Lock()
	defer m.mu.Unlock()
	if !allowed {
		delete(m.allowed, key)
		m.refusals++

		return
	}
	if m.refusals != ask.refusals {
		return
	}
	if m.allowed == nil {
		m.allowed = make(map[policyKey]time.Time)
	}
	for other, when := range m.allowed {
		if ask.at.Sub(when) >= policyRememberedFor {
			delete(m.allowed, other)
		}
	}
	if previous, ok := m.allowed[key]; !ok || ask.at.After(previous) {
		m.allowed[key] = ask.at
	}
}

// standing is when the last "allowed" for a cache of a repository was given,
// and whether it still stands at now. One from a clock now behind it does not.
func (m *policyMemory) standing(kind config.CacheKind, owner, repository string, now time.Time) (time.Time, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	at, ok := m.allowed[newPolicyKey(kind, owner, repository)]
	if !ok {
		return time.Time{}, false
	}
	age := now.Sub(at)

	return at, age >= 0 && age < policyRememberedFor
}

// allowedAt is when a session's cache counts its casPolicyAge without asking
// the kill switch from: now, for an answer the plane gave now, and for a
// remembered answer never later than lets that window end where the remembered
// answer stops standing.
func allowedAt(now, answered time.Time) time.Time {
	if latest := answered.Add(policyRememberedFor - casPolicyAge); latest.Before(now) {
		return latest
	}

	return now
}
