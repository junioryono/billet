package awsquota_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/awscreds"
	"github.com/junioryono/billet/internal/awsquota"
	"github.com/junioryono/billet/internal/awssig"
)

// creds is a static identity, so a signature is produced and the fake can see
// one without a credential chain being involved.
var creds = awscreds.Static(awssig.Credentials{
	AccessKeyID: "AKIAEXAMPLE", SecretAccessKey: "secret",
})

// serve builds a client pointed at a handler.
func serve(t *testing.T, h http.HandlerFunc) *awsquota.Client {
	t.Helper()

	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	return awsquota.New("us-west-2", srv.URL+"/", creds)
}

// A QUOTA IS READ, AND THE FIELDS AN OPERATOR ACTS ON SURVIVE.
//
// The code and the adjustable flag are not decoration: the first is what they
// type into a support request, and the second decides whether raising it is even
// possible — a limit that cannot be raised means "change the config", and one
// that can means "ask AWS".
func TestGetReadsAQuota(t *testing.T) {
	t.Parallel()

	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Amz-Target"); !strings.HasSuffix(got, "GetServiceQuota") {
			t.Errorf("target is %q", got)
		}

		if got := r.Header.Get("Authorization"); !strings.Contains(got, "AWS4-HMAC-SHA256") {
			t.Errorf("the request was not signed: %q", got)
		}

		writeJSON(t, w, map[string]any{"Quota": map[string]any{
			"QuotaCode": "L-1216C47A", "QuotaName": "Running On-Demand Standard instances",
			"ServiceCode": "ec2", "Value": 64.0, "Unit": "vCPU", "Adjustable": true,
		}})
	})

	q, err := c.Get(t.Context(), "ec2", "L-1216C47A")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	switch {
	case q.Code != "L-1216C47A":
		t.Errorf("code is %q", q.Code)
	case q.Value != 64:
		t.Errorf("value is %v", q.Value)
	case q.Unit != "vCPU":
		t.Errorf("unit is %q", q.Unit)
	case !q.Adjustable:
		t.Error("the adjustable flag was lost, so a report cannot say whether asking AWS " +
			"would help")
	}
}

// A RESPONSE THAT CARRIED NO QUOTA IS UNAVAILABLE, NOT A LIMIT OF ZERO.
//
// This is the could-not-tell/no collapse in its most expensive form here:
// reading an empty body as zero would report every fleet as over its limit, and
// the operator's response to that is to change a budget that was fine.
func TestAnEmptyAnswerIsUnavailableRatherThanZero(t *testing.T) {
	t.Parallel()

	c := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, map[string]any{})
	})

	_, err := c.Get(t.Context(), "ec2", "L-1216C47A")
	if !errors.Is(err, awsquota.ErrUnavailable) {
		t.Fatalf("an empty response answered %v, want %v", err, awsquota.ErrUnavailable)
	}
}

// AND SO IS A REFUSAL, WITH AWS'S OWN REASON IN IT.
//
// The TYPE is what sends a reader somewhere: AccessDeniedException is a policy
// statement to add, NoSuchResourceException is billet naming something AWS does
// not have, and only the first is theirs to fix.
func TestARefusalCarriesAWSsOwnReason(t *testing.T) {
	t.Parallel()

	c := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(t, w, map[string]any{
			"__type":  "com.amazonaws.servicequotas#AccessDeniedException",
			"message": "not authorized to perform servicequotas:GetServiceQuota",
		})
	})

	_, err := c.Get(t.Context(), "ec2", "L-1216C47A")
	if !errors.Is(err, awsquota.ErrUnavailable) {
		t.Fatalf("a refusal answered %v, want %v", err, awsquota.ErrUnavailable)
	}

	if !strings.Contains(err.Error(), "AccessDeniedException") {
		t.Errorf("the error does not carry AWS's own type, so it does not say whether this "+
			"is a policy to fix or a code billet got wrong: %v", err)
	}

	// THE NAMESPACE IS STRIPPED. `com.amazonaws.servicequotas#AccessDenied...`
	// is not what an operator searches for.
	if strings.Contains(err.Error(), "com.amazonaws") {
		t.Errorf("the error carries AWS's internal namespace: %v", err)
	}
}

// A LISTING FOLLOWS ITS PAGES, because a limit that appears only on a later one
// would otherwise read as absent — and absent is what billet reports as "no
// limit found", which is the wrong answer for a limit that exists.
func TestListFollowsItsPages(t *testing.T) {
	t.Parallel()

	page := 0
	c := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		page++
		if page == 1 {
			writeJSON(t, w, map[string]any{
				"Quotas": []map[string]any{{
					"QuotaCode": "L-1", "QuotaName": "First", "Value": 1.0, "Unit": "None",
				}},
				"NextToken": "more",
			})

			return
		}

		writeJSON(t, w, map[string]any{"Quotas": []map[string]any{{
			"QuotaCode": "L-2", "QuotaName": "Second", "Value": 2.0, "Unit": "None",
		}}})
	})

	quotas, err := c.List(t.Context(), "codebuild")
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	if len(quotas) != 2 || quotas[0].Code != "L-1" || quotas[1].Code != "L-2" {
		t.Fatalf("the listing lost a page: %+v", quotas)
	}
}

// A CYCLING TOKEN IS AN ERROR RATHER THAN A LOOP, and what it read so far still
// comes back — a diagnostic that discarded its findings on the last page would
// report nothing about the limits it had already seen.
func TestAListingThatCyclesIsRefusedAndKeepsWhatItRead(t *testing.T) {
	t.Parallel()

	c := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, map[string]any{
			"Quotas": []map[string]any{{
				"QuotaCode": "L-1", "QuotaName": "First", "Value": 1.0, "Unit": "None",
			}},
			"NextToken": "always-the-same",
		})
	})

	quotas, err := c.List(t.Context(), "codebuild")
	if !errors.Is(err, awsquota.ErrUnavailable) {
		t.Fatalf("a cycling listing answered %v", err)
	}

	if len(quotas) == 0 {
		t.Error("the listing discarded the pages it had already read")
	}
}

// A REDIRECT IS REFUSED, because following one would send a request signed with
// the operator's credentials to a host they did not name.
func TestARedirectIsRefused(t *testing.T) {
	t.Parallel()

	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://elsewhere.invalid/", http.StatusFound)
	})

	if _, err := c.Get(t.Context(), "ec2", "L-1216C47A"); err == nil {
		t.Fatal("a redirect was followed")
	}
}

func writeJSON(t *testing.T, w http.ResponseWriter, v any) {
	t.Helper()

	body, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	w.Header().Set("Content-Type", "application/x-amz-json-1.1")

	if _, err := fmt.Fprint(w, string(body)); err != nil {
		t.Errorf("write: %v", err)
	}
}
