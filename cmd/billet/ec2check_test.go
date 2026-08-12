package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/config"
)

// fakeEC2 answers DescribeInstances, so the pre-flight's live call has somewhere
// to go that is not somebody's AWS account.
func fakeEC2(t *testing.T, status int) string {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)

		body := `<DescribeInstancesResponse><reservationSet/></DescribeInstancesResponse>`
		if status != http.StatusOK {
			body = `<Response><Errors><Error><Code>UnauthorizedOperation</Code>` +
				`<Message>no</Message></Error></Errors></Response>`
		}

		if _, err := io.WriteString(w, body); err != nil {
			t.Errorf("write: %v", err)
		}
	}))

	t.Cleanup(srv.Close)

	return srv.URL
}

// capture redirects stdout for the duration of fn and returns what was written.
//
// `billet check` REPORTS to an operator, so what it prints is the whole product
// and asserting only its error return would leave the interesting half untested.
func capture(t *testing.T, fn func()) string {
	t.Helper()

	saved := os.Stdout

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}

	os.Stdout = w

	done := make(chan string, 1)

	go func() {
		var b strings.Builder

		_, _ = io.Copy(&b, r) //nolint:errcheck // the write end is closed below, ending the copy

		done <- b.String()
	}()

	fn()

	os.Stdout = saved

	if err := w.Close(); err != nil {
		t.Fatalf("close pipe: %v", err)
	}

	return <-done
}

// THE PRE-FLIGHT REPORTS WHICH IDENTITY BILLET WILL USE, AND NEVER THE SECRET.
//
// The access key id is an identifier, and printing it is the difference between
// "billet is using the wrong role" and an operator staring at a config that looks
// right. The secret is a durable credential for a whole AWS account and must not
// reach a terminal, a scrollback buffer, or the paste of a support request.
func TestTheCloudPreflightNamesTheIdentityAndNotTheSecret(t *testing.T) {
	const secret = "wJalrXUtnFEMI-thisIsTheSecret"

	t.Setenv("AWS_ACCESS_KEY_ID", "AKIDEXAMPLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", secret)

	cfg := &config.EC2Config{
		Region:           "us-west-2",
		Endpoint:         fakeEC2(t, http.StatusOK),
		SubnetID:         "subnet-0abc",
		SecurityGroupIDs: []string{"sg-0abc"},
		InstanceTypes:    []config.EC2InstanceType{{Type: "c7i.2xlarge", VCPU: 8, Memory: 16 * config.GiB}},
	}

	var err error

	out := capture(t, func() { err = checkEC2Credentials(t.Context(), cfg) })
	if err != nil {
		t.Fatalf("a host with credentials in its environment failed the pre-flight: %v", err)
	}

	if strings.Contains(out, secret) {
		t.Fatalf("the pre-flight printed the secret access key:\n%s", out)
	}

	for _, want := range []string{"AKIDEXAMPLE", "us-west-2", "subnet-0abc", "on-demand"} {
		if !strings.Contains(out, want) {
			t.Errorf("the pre-flight does not report %q:\n%s", want, out)
		}
	}

	// SAID OUT LOUD RATHER THAN INFERRED FROM AN ABSENT KEY. A deployment that
	// expected to run fork pull requests on rented machines and finds them queuing
	// forever has no other way to see why.
	if !strings.Contains(out, "untrusted") {
		t.Errorf("the pre-flight does not say untrusted work will be refused:\n%s", out)
	}
}

// SPOT IS NAMED AS WHAT IT IS. An operator reading a pre-flight should not have
// to know that a reclaimed instance is a failed build rather than a retry.
func TestTheCloudPreflightSaysWhatSpotCosts(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIDEXAMPLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")

	cfg := &config.EC2Config{
		Region:                    "us-west-2",
		Endpoint:                  fakeEC2(t, http.StatusOK),
		SubnetID:                  "subnet-0abc",
		SecurityGroupIDs:          []string{"sg-0abc"},
		UntrustedSecurityGroupIDs: []string{"sg-fork"},
		InstanceTypes:             []config.EC2InstanceType{{Type: "c7i.2xlarge", VCPU: 8, Memory: 16 * config.GiB}},
		Spot:                      true,
	}

	var err error

	out := capture(t, func() { err = checkEC2Credentials(t.Context(), cfg) })
	if err != nil {
		t.Fatalf("pre-flight: %v", err)
	}

	if !strings.Contains(out, "spot") || !strings.Contains(out, "requeue") {
		t.Errorf("the pre-flight does not say what buying spot costs:\n%s", out)
	}

	// And with a network described for it, the refusal notice is absent — a
	// warning that never goes away is one nobody reads.
	if strings.Contains(out, "untrusted work will be refused") {
		t.Errorf("untrusted work was reported as refused despite having its own group:\n%s", out)
	}
}

// RESOLVING A CREDENTIAL IS NOT THE SAME AS BEING ABLE TO USE ONE.
//
// An expired key, a key for the wrong account, or a role without ec2 permissions
// all resolve perfectly and then fail on the first job of the day, with a 403
// that names neither. The pre-flight uses the credentials it just reported, so
// what is proved is what was named.
func TestTheCloudPreflightFailsWhenTheCredentialsCannotBeUsed(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIDEXAMPLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")

	cfg := &config.EC2Config{
		Region:           "us-west-2",
		Endpoint:         fakeEC2(t, http.StatusForbidden),
		SubnetID:         "subnet-0abc",
		SecurityGroupIDs: []string{"sg-0abc"},
		InstanceTypes:    []config.EC2InstanceType{{Type: "c7i.2xlarge", VCPU: 8, Memory: 16 * config.GiB}},
	}

	var err error

	capture(t, func() { err = checkEC2Credentials(t.Context(), cfg) })

	if err == nil {
		t.Fatal("credentials that the api refuses were reported as usable")
	}

	if !strings.Contains(err.Error(), "us-west-2") {
		t.Errorf("the error does not say where the call failed: %v", err)
	}
}
