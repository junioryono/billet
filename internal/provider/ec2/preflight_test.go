package ec2

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/awscreds"
)

func preflightCreds() awscreds.Static {
	return awscreds.Static{AccessKeyID: "AKID", SecretAccessKey: "s"}
}

// assertPreflight parses a fake request body and pins the exact action, the
// signing identity, and any required parameters — so a primitive that sent the
// wrong action, dropped an id, or used the wrong credentials fails the test
// rather than passing against a fake that answered regardless.
func assertPreflight(t *testing.T, r *http.Request, action string, params map[string]string) {
	t.Helper()

	body := readBody(t, r)
	got, err := url.ParseQuery(body)
	if err != nil {
		t.Fatalf("parse request body: %v", err)
	}

	if got.Get("Action") != action {
		t.Errorf("action = %q, want %q", got.Get("Action"), action)
	}
	if auth := r.Header.Get("Authorization"); !strings.Contains(auth, "Credential=AKID/") {
		t.Errorf("request not signed by the test identity: %q", auth)
	}
	for k, v := range params {
		if got.Get(k) != v {
			t.Errorf("param %s = %q, want %q", k, got.Get(k), v)
		}
	}
}

// THE SUBNET'S VPC AND ZONE ARE READ BACK; a subnet that resolves to nothing is a
// hard error, because the config named a specific one.
func TestDescribeSubnet(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertPreflight(t, r, "DescribeSubnets", map[string]string{"SubnetId.1": "subnet-1"})
		fmt.Fprint(w, `<DescribeSubnetsResponse><subnetSet><item>`+
			`<subnetId>subnet-1</subnetId><vpcId>vpc-9</vpcId>`+
			`<availabilityZone>us-west-2a</availabilityZone><state>available</state>`+
			`</item></subnetSet></DescribeSubnetsResponse>`)
	}))
	t.Cleanup(srv.Close)

	got, err := DescribeSubnet(t.Context(), "us-west-2", srv.URL, preflightCreds(), "subnet-1")
	if err != nil {
		t.Fatalf("DescribeSubnet: %v", err)
	}
	if got.VPCID != "vpc-9" || got.AvailabilityZone != "us-west-2a" || got.State != "available" {
		t.Errorf("subnet = %+v", got)
	}
}

func TestDescribeSubnetNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertPreflight(t, r, "DescribeSubnets", map[string]string{"SubnetId.1": "subnet-x"})
		fmt.Fprint(w, `<DescribeSubnetsResponse><subnetSet></subnetSet></DescribeSubnetsResponse>`)
	}))
	t.Cleanup(srv.Close)

	_, err := DescribeSubnet(t.Context(), "us-west-2", srv.URL, preflightCreds(), "subnet-x")
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("want a not-found error, got %v", err)
	}
}

// GROUPS COME BACK IN THE ORDER ASKED, and a group AWS omits is a hard error, not
// a silently short list.
func TestDescribeSecurityGroups(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := readBody(t, r)
		if !strings.Contains(body, "GroupId.1=sg-a") || !strings.Contains(body, "GroupId.2=sg-b") {
			t.Errorf("request did not name both groups: %s", body)
		}
		// Returned out of order to prove the function re-orders.
		fmt.Fprint(w, `<DescribeSecurityGroupsResponse><securityGroupInfo>`+
			`<item><groupId>sg-b</groupId><vpcId>vpc-9</vpcId></item>`+
			`<item><groupId>sg-a</groupId><vpcId>vpc-9</vpcId></item>`+
			`</securityGroupInfo></DescribeSecurityGroupsResponse>`)
	}))
	t.Cleanup(srv.Close)

	got, err := DescribeSecurityGroups(t.Context(), "us-west-2", srv.URL, preflightCreds(),
		[]string{"sg-a", "sg-b"})
	if err != nil {
		t.Fatalf("DescribeSecurityGroups: %v", err)
	}
	if len(got) != 2 || got[0].GroupID != "sg-a" || got[1].GroupID != "sg-b" {
		t.Errorf("groups = %+v, want sg-a then sg-b", got)
	}
	if got[0].VPCID != "vpc-9" {
		t.Errorf("vpc = %q", got[0].VPCID)
	}
}

func TestDescribeSecurityGroupsMissingIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertPreflight(t, r, "DescribeSecurityGroups", map[string]string{"GroupId.2": "sg-missing"})
		fmt.Fprint(w, `<DescribeSecurityGroupsResponse><securityGroupInfo>`+
			`<item><groupId>sg-a</groupId><vpcId>vpc-9</vpcId></item>`+
			`</securityGroupInfo></DescribeSecurityGroupsResponse>`)
	}))
	t.Cleanup(srv.Close)

	_, err := DescribeSecurityGroups(t.Context(), "us-west-2", srv.URL, preflightCreds(),
		[]string{"sg-a", "sg-missing"})
	if err == nil || !strings.Contains(err.Error(), "sg-missing") {
		t.Fatalf("want an error naming the missing group, got %v", err)
	}
}

// A GROUP ON A LATER PAGE IS FOUND, not reported missing: the missing-id check
// runs only after every page, or a valid config would be a fatal false failure.
func TestDescribeSecurityGroupsPaginates(t *testing.T) {
	var page int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := readBody(t, r)
		if !strings.Contains(body, "GroupId.1=sg-a") || !strings.Contains(body, "GroupId.2=sg-b") {
			t.Errorf("a page did not carry both group ids: %s", body)
		}
		page++
		if page == 1 {
			fmt.Fprint(w, `<DescribeSecurityGroupsResponse><securityGroupInfo>`+
				`<item><groupId>sg-a</groupId><vpcId>vpc-9</vpcId></item>`+
				`</securityGroupInfo><nextToken>page2</nextToken></DescribeSecurityGroupsResponse>`)

			return
		}
		if !strings.Contains(body, "NextToken=page2") {
			t.Errorf("page 2 did not carry the token: %s", body)
		}
		fmt.Fprint(w, `<DescribeSecurityGroupsResponse><securityGroupInfo>`+
			`<item><groupId>sg-b</groupId><vpcId>vpc-9</vpcId></item>`+
			`</securityGroupInfo></DescribeSecurityGroupsResponse>`)
	}))
	t.Cleanup(srv.Close)

	got, err := DescribeSecurityGroups(t.Context(), "us-west-2", srv.URL, preflightCreds(),
		[]string{"sg-a", "sg-b"})
	if err != nil {
		t.Fatalf("DescribeSecurityGroups: %v", err)
	}
	if page != 2 {
		t.Errorf("followed %d pages, want 2", page)
	}
	if len(got) != 2 || got[0].GroupID != "sg-a" || got[1].GroupID != "sg-b" {
		t.Errorf("groups across pages = %+v", got)
	}
}

// AN AVAILABLE AMI IS FOUND; A MALFORMED/PLACEHOLDER ONE IS A NOT-FOUND RESULT
// (not an error), so the check reports "not built yet" instead of failing itself.
func TestDescribeImageStates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := readBody(t, r)
		switch {
		case strings.Contains(body, "ImageId.1=ami-good"):
			fmt.Fprint(w, `<DescribeImagesResponse><imagesSet><item>`+
				`<imageId>ami-good</imageId><imageState>available</imageState>`+
				`</item></imagesSet></DescribeImagesResponse>`)
		case strings.Contains(body, "ImageId.1=ami-REPLACE"):
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `<Response><Errors><Error><Code>InvalidAMIID.Malformed</Code>`+
				`<Message>bad id</Message></Error></Errors></Response>`)
		default:
			t.Errorf("unexpected image query: %s", body)
		}
	}))
	t.Cleanup(srv.Close)

	got, err := DescribeImageStates(t.Context(), "us-west-2", srv.URL, preflightCreds(),
		[]string{"ami-good", "ami-REPLACE"})
	if err != nil {
		t.Fatalf("DescribeImageStates: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("images = %+v", got)
	}
	if !got[0].Found || got[0].State != "available" {
		t.Errorf("ami-good = %+v, want found+available", got[0])
	}
	if got[1].Found || !strings.Contains(got[1].State, "InvalidAMIID") {
		t.Errorf("ami-REPLACE = %+v, want not-found with the AWS code", got[1])
	}
}

// A NON-AMI-ID API ERROR IS PROPAGATED, not swallowed as a not-found result: an
// AccessDenied on DescribeImages is a real problem the operator must see.
func TestDescribeImageStatesPropagatesRealErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertPreflight(t, r, "DescribeImages", map[string]string{"ImageId.1": "ami-good"})
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `<Response><Errors><Error><Code>UnauthorizedOperation</Code>`+
			`<Message>no</Message></Error></Errors></Response>`)
	}))
	t.Cleanup(srv.Close)

	_, err := DescribeImageStates(t.Context(), "us-west-2", srv.URL, preflightCreds(),
		[]string{"ami-good"})
	if err == nil || !strings.Contains(err.Error(), "UnauthorizedOperation") {
		t.Fatalf("want the auth error propagated, got %v", err)
	}
}

// A PAGINATION CYCLE IS REFUSED rather than looped on forever: an A->B->A token
// sequence stops at the repeat with a cycle error, in a bounded number of calls.
func TestDescribeSecurityGroupsRefusesAPaginationCycle(t *testing.T) {
	tokens := []string{"A", "B", "A"} // the third repeats the first
	var count int

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = readBody(t, r)
		next := tokens[count]
		count++
		fmt.Fprintf(w, `<DescribeSecurityGroupsResponse><securityGroupInfo></securityGroupInfo>`+
			`<nextToken>%s</nextToken></DescribeSecurityGroupsResponse>`, next)
	}))
	t.Cleanup(srv.Close)

	_, err := DescribeSecurityGroups(t.Context(), "us-west-2", srv.URL, preflightCreds(),
		[]string{"sg-a"})
	if err == nil || !strings.Contains(err.Error(), "cycled") {
		t.Fatalf("want a cycle refusal, got %v", err)
	}
	if count > 3 {
		t.Errorf("the cycle guard did not stop promptly: %d requests", count)
	}
}

// EVERY HOST-DERIVING DISCOVERY ENTRY POINT VALIDATES THE REGION before a host is
// derived from it — a guard in only one of them is a guard the others route
// around. OnDemandPriceUSDPerHour is excluded on purpose: the region selects
// between two FIXED partition endpoints and is otherwise a filter value — it is
// never interpolated into a host — so it legitimately dials without validating.
func TestDiscoveryEntryPointsValidateRegion(t *testing.T) {
	const hostile = "x@attacker.example/?"
	creds := preflightCreds()

	cases := map[string]func() error{
		"DescribeInstanceTypes": func() error {
			_, err := DescribeInstanceTypes(t.Context(), hostile, "", creds, []string{"c7i.xlarge"})

			return err
		},
		"DescribeSubnet": func() error {
			_, err := DescribeSubnet(t.Context(), hostile, "", creds, "subnet-1")

			return err
		},
		"DescribeSecurityGroups": func() error {
			_, err := DescribeSecurityGroups(t.Context(), hostile, "", creds, []string{"sg-1"})

			return err
		},
		"DescribeImageStates": func() error {
			_, err := DescribeImageStates(t.Context(), hostile, "", creds, []string{"ami-1"})

			return err
		},
	}

	for name, call := range cases {
		err := call()
		if err == nil || !strings.Contains(err.Error(), "aws region") {
			t.Errorf("%s did not refuse the hostile region before dialing: %v", name, err)
		}
	}
}
