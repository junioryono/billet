package lease

import (
	"errors"
	"fmt"
	"strconv"
	"time"
)

// CacheStoreState is whether a node is asking its site's store for caches.
//
// A CLOSED VOCABULARY AND AN OBSERVATION, like what a cache did for a job: it decides
// nothing about capacity, custody or teardown, and a control plane that reads one
// only says it back to an operator.
type CacheStoreState string

const (
	// CacheStoreClosed means jobs use the store's caches.
	CacheStoreClosed CacheStoreState = "closed"
	// CacheStoreOpen means the store stopped answering, so new jobs start cold and
	// the node's caches answer a miss without asking it.
	CacheStoreOpen CacheStoreState = "open"
	// CacheStoreProbation means the store answered a probe, jobs are asking it
	// again, and the next call decides whether it is back.
	CacheStoreProbation CacheStoreState = "probation"
)

// CacheStoreReason is what opened a breaker. No store's own words travel: they are
// the node's to log, and a report carries only which kind of failure it was.
type CacheStoreReason string

const (
	// CacheStoreTimeout means the store did not answer within a call's limit, which
	// says nothing about what the store holds.
	CacheStoreTimeout CacheStoreReason = "timeout"
	// CacheStoreError means the store answered with a failure.
	CacheStoreError CacheStoreReason = "error"
)

// CacheStoreReport is what one node says about its site's store.
type CacheStoreReport struct {
	// Store is the store's kind: ceph or ebs-s3.
	Store string
	State CacheStoreState
	// Since is when the state began, on the node's clock.
	Since time.Time
	// Failures is how many failures opened the breaker, and Reason the last one's
	// kind; both are empty for a closed one.
	Failures int
	Reason   CacheStoreReason
}

// Validate refuses a report billet must not write.
func (r CacheStoreReport) Validate() error {
	switch r.Store {
	case "ceph", "ebs-s3":
	default:
		return fmt.Errorf("lease: %s is not a site store billet records", strconv.Quote(r.Store))
	}
	switch r.State {
	case CacheStoreClosed, CacheStoreOpen, CacheStoreProbation:
	default:
		return fmt.Errorf("lease: %s is not a store state billet records", strconv.Quote(string(r.State)))
	}
	switch r.Reason {
	case "", CacheStoreTimeout, CacheStoreError:
	default:
		return fmt.Errorf("lease: %s is not a store failure billet records", strconv.Quote(string(r.Reason)))
	}
	if r.Since.IsZero() {
		return errors.New("lease: a store's state must say when it began")
	}
	if r.Failures < 0 {
		return fmt.Errorf("lease: a store cannot have failed %d times", r.Failures)
	}

	return nil
}
