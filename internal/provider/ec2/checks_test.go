package ec2

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/awscreds"
	"github.com/junioryono/billet/internal/awssig"
)

func checksCreds() awscreds.Source {
	return awscreds.Static(awssig.Credentials{AccessKeyID: "AKIDEXAMPLE", SecretAccessKey: "secret"})
}

// THE QUEUE PROBE READS ATTRIBUTES AND NOTHING ELSE: a ReceiveMessage would
// consume a real interruption warning. The fake refuses any other target.
func TestCheckInterruptionQueueReadsWithoutConsuming(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Amz-Target"); got != "AmazonSQS.GetQueueAttributes" {
			t.Errorf("the probe called %q; only GetQueueAttributes may run", got)
			w.WriteHeader(http.StatusBadRequest)

			return
		}
		if _, err := w.Write([]byte(`{"Attributes":{"QueueArn":"arn:aws:sqs:us-west-2:123456789012:billet"}}`)); err != nil {
			t.Errorf("write: %v", err)
		}
	}))
	t.Cleanup(srv.Close)

	arn, err := CheckInterruptionQueue(t.Context(), "us-west-2", checksCreds(),
		srv.URL+"/123456789012/billet")
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if arn != "arn:aws:sqs:us-west-2:123456789012:billet" {
		t.Errorf("arn = %q", arn)
	}
}

// THE REFUSAL NAMES THE WIRE'S OWN ERROR TYPE — "the queue does not exist"
// and "this identity may not read it" are different remedies.
func TestCheckInterruptionQueueNamesTheRefusal(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		if _, err := w.Write([]byte(`{"__type":"com.amazonaws.sqs#QueueDoesNotExist","message":"The specified queue does not exist."}`)); err != nil {
			t.Errorf("write: %v", err)
		}
	}))
	t.Cleanup(srv.Close)

	_, err := CheckInterruptionQueue(t.Context(), "us-west-2", checksCreds(),
		srv.URL+"/123456789012/billet")
	if err == nil || !strings.Contains(err.Error(), "QueueDoesNotExist") {
		t.Fatalf("the refusal does not carry the wire's error type: %v", err)
	}
}

// THE PROFILE PROBE IS THREE-VALUED, and the bands do not collapse: a missing
// profile is a launch-killing misconfiguration, while AccessDenied is a fact
// about the CHECKER and must not read as either verdict.
func TestCheckInstanceProfileBands(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		status int
		body   string
		want   ProfileCheck
	}{
		"found": {http.StatusOK,
			`<GetInstanceProfileResponse xmlns="https://iam.amazonaws.com/doc/2010-05-08/"><GetInstanceProfileResult></GetInstanceProfileResult></GetInstanceProfileResponse>`,
			ProfileFound},
		"portal 200": {http.StatusOK, `<html>hotel wifi</html>`, ProfileUnknown},
		"missing": {http.StatusNotFound,
			// The xmlns is what real IAM sends; the parser must match on the
			// local name regardless.
			`<ErrorResponse xmlns="https://iam.amazonaws.com/doc/2010-05-08/"><Error><Code>NoSuchEntity</Code><Message>Instance Profile jobs cannot be found.</Message></Error></ErrorResponse>`,
			ProfileMissing},
		"denied": {http.StatusForbidden,
			`<ErrorResponse><Error><Code>AccessDenied</Code><Message>not authorized to perform iam:GetInstanceProfile</Message></Error></ErrorResponse>`,
			ProfileUnknown},
		"unclassifiable": {http.StatusBadGateway, `<html>upstream</html>`, ProfileUnknown},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := r.ParseForm(); err != nil || r.PostForm.Get("Action") != "GetInstanceProfile" {
					t.Errorf("unexpected request action %q", r.PostForm.Get("Action"))
				}
				if got := r.PostForm.Get("Version"); got != "2010-05-08" {
					t.Errorf("iam api version %q", got)
				}
				// THE SIGNING SCOPE IS THE WIRE'S MOST LIKELY MISTAKE: pin
				// region and service, or signing as ec2/us-west-2 passes.
				if auth := r.Header.Get("Authorization"); !strings.Contains(auth, "/us-east-1/iam/aws4_request") {
					t.Errorf("iam request signed with the wrong scope: %q", auth)
				}
				w.WriteHeader(tc.status)
				if _, err := w.Write([]byte(tc.body)); err != nil {
					t.Errorf("write: %v", err)
				}
			}))
			t.Cleanup(srv.Close)

			got, reason, err := CheckInstanceProfile(t.Context(), "us-west-2", srv.URL, checksCreds(), "jobs")
			if err != nil {
				t.Fatalf("probe errored instead of classifying: %v", err)
			}
			if got != tc.want {
				t.Errorf("verdict = %v (reason %q), want %v", got, reason, tc.want)
			}
			if tc.want != ProfileFound && reason == "" {
				t.Error("a non-Found verdict carries no reason")
			}
		})
	}
}

// THE MEASURED OPT-IN-REGION SHAPE: with valid credentials, a region that is
// not enabled on the account answers AuthFailure with credential-shaped prose
// — measured twice against af-south-1 with an IAM user and an SSO role that
// both work in us-east-1. A genuine permission refusal has a different code
// and must not trip this.
func TestRegionMayBeDisabled(t *testing.T) {
	t.Parallel()

	disabled := &apiError{
		Code:    "AuthFailure",
		Message: "AWS was not able to validate the provided access credentials",
		Status:  401,
	}
	if !RegionMayBeDisabled(disabled) {
		t.Error("the measured opted-out shape was not recognized")
	}

	denied := &apiError{
		Code:    "UnauthorizedOperation",
		Message: "You are not authorized to perform this operation",
		Status:  403,
	}
	if RegionMayBeDisabled(denied) {
		t.Error("a genuine permission refusal was blamed on the region")
	}

	if RegionMayBeDisabled(http.ErrServerClosed) {
		t.Error("a non-API error was blamed on the region")
	}
}
