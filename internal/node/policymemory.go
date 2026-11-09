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

// note records what the plane answered at a moment.
func (m *policyMemory) note(kind config.CacheKind, owner, repository string, allowed bool, at time.Time) {
	key := newPolicyKey(kind, owner, repository)
	m.mu.Lock()
	defer m.mu.Unlock()
	if !allowed {
		delete(m.allowed, key)

		return
	}
	if m.allowed == nil {
		m.allowed = make(map[policyKey]time.Time)
	}
	for other, when := range m.allowed {
		if at.Sub(when) >= policyRememberedFor {
			delete(m.allowed, other)
		}
	}
	if previous, ok := m.allowed[key]; !ok || at.After(previous) {
		m.allowed[key] = at
	}
}

// standing is how old the last "allowed" for a cache of a repository is, and
// whether it still stands at now. One from a clock now behind it does not.
func (m *policyMemory) standing(kind config.CacheKind, owner, repository string, now time.Time) (time.Duration, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	at, ok := m.allowed[newPolicyKey(kind, owner, repository)]
	if !ok {
		return 0, false
	}
	age := now.Sub(at)

	return age, age >= 0 && age < policyRememberedFor
}
