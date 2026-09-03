package codebuild

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/awsquota"
	"github.com/junioryono/billet/internal/config"
)

// emptyQuotaEndpoint answers a listing with no quotas in it, which is what a
// service billet has no published limits for looks like.
func emptyQuotaEndpoint(t *testing.T) string {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/x-amz-json-1.1")

		if _, err := w.Write([]byte(`{"Quotas":[]}`)); err != nil {
			t.Errorf("write: %v", err)
		}
	}))

	t.Cleanup(srv.Close)

	return srv.URL + "/"
}

// THE CONCURRENCY LIMIT IS FOUND BY WHAT AWS CALLS IT, not by a code billet
// shipped.
//
// CodeBuild has one limit per environment and compute type and their identifiers
// are not derivable from anything billet knows, so a table of them would be
// billet inventing identifiers for somebody else's API — and a wrong one reads as
// "no limit", which is the direction that costs somebody's build. Matching AWS's
// own name fails the other way: a rename makes billet report a limit it could not
// find, which is what an operator can act on.
func TestTheConcurrencyLimitIsMatchedOnAWSsOwnName(t *testing.T) {
	t.Parallel()

	published := []awsquota.Quota{
		{Code: "L-AAA", Name: "Concurrently running builds for BUILD_GENERAL1_MEDIUM", Value: 1},
		{Code: "L-BBB", Name: "Concurrently running builds for BUILD_GENERAL1_SMALL", Value: 5},
	}

	q, found := concurrencyQuotaFor(published, "BUILD_GENERAL1_MEDIUM")
	if !found {
		t.Fatal("the limit AWS published was not found")
	}

	if q.Code != "L-AAA" || q.Value != 1 {
		t.Errorf("matched the wrong limit: %+v", q)
	}
}

// AND A LIMIT ABOUT SOMETHING ELSE IS NOT MISTAKEN FOR IT.
//
// The compute type appearing in a name is not enough on its own: a future
// "Queued builds for BUILD_GENERAL1_MEDIUM" is about a different thing entirely,
// and reporting it as the concurrency limit would tell an operator their account
// runs thirty concurrent builds when it runs one.
func TestALimitAboutSomethingElseIsNotMistakenForConcurrency(t *testing.T) {
	t.Parallel()

	published := []awsquota.Quota{
		{Code: "L-CCC", Name: "Queued builds for BUILD_GENERAL1_MEDIUM", Value: 30},
	}

	if q, found := concurrencyQuotaFor(published, "BUILD_GENERAL1_MEDIUM"); found {
		t.Fatalf("a queue limit was reported as a concurrency limit: %+v", q)
	}
}

// A RENAME MAKES BILLET SAY IT COULD NOT FIND ONE, which is the safe direction:
// "billet could not find a limit for this compute type" and "the account has no
// limit on it" are different facts, and only the second would be safe to say
// nothing about.
func TestAnUnmatchedComputeTypeIsReportedRatherThanSkipped(t *testing.T) {
	t.Parallel()

	f := newFakeAWS(t)
	p := newTestProvider(t, f, func(cfg *config.CodeBuildConfig) {
		cfg.ComputeTypes = []config.RemoteShape{
			{Type: "BUILD_GENERAL1_MEDIUM", VCPU: 4, Memory: 7 * config.GiB, PriceUSDPerHour: 1},
		}
	})

	// A listing that answers with nothing about this shape.
	p.api.quotas = emptyQuotaEndpoint(t)

	quotas, err := p.Quotas(t.Context())
	if err == nil {
		t.Fatal("a compute type with no published limit was passed over in silence")
	}

	if !strings.Contains(err.Error(), "BUILD_GENERAL1_MEDIUM") {
		t.Errorf("the report does not name the shape it could not answer for: %v", err)
	}

	if !errors.Is(err, awsquota.ErrUnavailable) {
		t.Errorf("a limit billet could not find is not reported as unavailable: %v", err)
	}

	// AND THE PARTIAL ANSWER STILL COMES BACK. One shape billet could not answer
	// for must not discard the account-wide ceiling it can always state.
	if len(quotas) == 0 {
		t.Error("a failed lookup discarded the findings that did not depend on it")
	}
}

// THE ACCOUNT-WIDE QUEUE IS REPORTED WITH NO SHAPE.
//
// It bounds a BURST across every project in the account rather than this node's
// budget, so it carries no Shape and nothing compares it to this node's
// concurrency — which would be two numbers that are not about the same thing.
// It is also not a limit Service Quotas lists, which is why it is a measured
// constant rather than a read.
func TestTheAccountQueueCeilingIsReportedWithoutAComparison(t *testing.T) {
	t.Parallel()

	f := newFakeAWS(t)
	p := newTestProvider(t, f, nil)

	// The listing answers nothing, so the only finding is the one billet states
	// from its own measurement. A provider must declare at least one compute type
	// — New refuses otherwise, correctly — so this cannot be arranged by removing
	// the catalogue.
	p.api.quotas = emptyQuotaEndpoint(t)

	// The error is deliberately not asserted: this test is about what the report
	// CONTAINS when a listing answers nothing, and the missing per-shape limit is
	// TestAnUnmatchedComputeTypeIsReportedRatherThanSkipped's subject.
	quotas, quotaErr := p.Quotas(t.Context())
	_ = quotaErr

	var account *struct {
		limit float64
		shape string
	}

	for i := range quotas {
		if quotas[i].Limit == AccountQueueCeiling && quotas[i].Code == "" {
			account = &struct {
				limit float64
				shape string
			}{quotas[i].Limit, quotas[i].Shape}
		}
	}

	if account == nil {
		t.Fatalf("the account queue ceiling was not reported: %+v", quotas)

		// staticcheck reads t.Fatalf as a call that may return, so without this
		// every dereference below is flagged SA5011.
		return
	}

	if account.shape != "" {
		t.Errorf("the account ceiling names a shape (%q), so something would compare it to "+
			"one node's budget — and thirty builds across every project in the account is "+
			"not about this node's concurrency", account.shape)
	}
}
