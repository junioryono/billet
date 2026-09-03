package ec2

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/awscreds"
)

// A DESCRIBE READS BACK WHAT THE SHAPE HOLDS, and follows pagination — the vcpu
// and memory it returns are what a generated instance_types entry must declare,
// so a test that stopped at page one would miss a shape the operator named.
func TestDescribeInstanceTypesPaginates(t *testing.T) {
	var pages int

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
		}

		// The request must name the action and the shapes it asks for.
		if !strings.Contains(string(body), "Action=DescribeInstanceTypes") ||
			!strings.Contains(string(body), "InstanceType.1=") {
			t.Errorf("request does not name the action and shapes: %s", body)
		}

		pages++
		if pages == 1 {
			fmt.Fprint(w, `<DescribeInstanceTypesResponse>`+
				`<instanceTypeSet><item>`+
				`<instanceType>c7i.xlarge</instanceType>`+
				`<vCpuInfo><defaultVCpus>4</defaultVCpus></vCpuInfo>`+
				`<memoryInfo><sizeInMiB>8192</sizeInMiB></memoryInfo>`+
				`</item></instanceTypeSet>`+
				`<nextToken>page2</nextToken>`+
				`</DescribeInstanceTypesResponse>`)

			return
		}

		if !strings.Contains(string(body), "NextToken=page2") {
			t.Errorf("page 2 did not carry the pagination token: %s", body)
			w.WriteHeader(http.StatusBadRequest)

			return
		}

		fmt.Fprint(w, `<DescribeInstanceTypesResponse>`+
			`<instanceTypeSet><item>`+
			`<instanceType>c7i.2xlarge</instanceType>`+
			`<vCpuInfo><defaultVCpus>8</defaultVCpus></vCpuInfo>`+
			`<memoryInfo><sizeInMiB>16384</sizeInMiB></memoryInfo>`+
			`</item></instanceTypeSet>`+
			`</DescribeInstanceTypesResponse>`)
	}))
	t.Cleanup(srv.Close)

	infos, err := DescribeInstanceTypes(t.Context(), "us-west-2", srv.URL,
		awscreds.Static{AccessKeyID: "AKID", SecretAccessKey: "s"},
		[]string{"c7i.xlarge", "c7i.2xlarge"})
	if err != nil {
		t.Fatalf("DescribeInstanceTypes: %v", err)
	}

	if pages != 2 {
		t.Errorf("followed %d pages, want 2", pages)
	}
	if len(infos) != 2 {
		t.Fatalf("got %d shapes, want 2: %+v", len(infos), infos)
	}
	if infos[0] != (InstanceTypeInfo{Type: "c7i.xlarge", VCPU: 4, MemoryMiB: 8192}) {
		t.Errorf("shape[0] = %+v", infos[0])
	}
	if infos[1] != (InstanceTypeInfo{Type: "c7i.2xlarge", VCPU: 8, MemoryMiB: 16384}) {
		t.Errorf("shape[1] = %+v", infos[1])
	}
}

// AN API ERROR IS SURFACED, not swallowed into an empty list a caller would read
// as "no such shape". A typo'd shape must fail loudly rather than silently drop a
// tier the operator asked for.
func TestDescribeInstanceTypesSurfacesAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `<Response><Errors><Error><Code>InvalidInstanceType</Code>`+
			`<Message>not a valid type</Message></Error></Errors></Response>`)
	}))
	t.Cleanup(srv.Close)

	_, err := DescribeInstanceTypes(t.Context(), "us-west-2", srv.URL,
		awscreds.Static{AccessKeyID: "AKID", SecretAccessKey: "s"},
		[]string{"c7i.nope"})
	if err == nil {
		t.Fatal("DescribeInstanceTypes accepted an API error as an empty result")
	}
	if !strings.Contains(err.Error(), "InvalidInstanceType") {
		t.Errorf("error does not carry the API code: %v", err)
	}
}

// A MALFORMED REGION IS REFUSED BEFORE ANY REQUEST, because it is interpolated
// into the endpoint host — an unchecked `x@attacker.example/?` would send a signed
// request to attacker.example. No server is stood up: a request here is the bug.
func TestDescribeInstanceTypesValidatesTheRegion(t *testing.T) {
	_, err := DescribeInstanceTypes(t.Context(), "x@attacker.example/?", "",
		awscreds.Static{AccessKeyID: "AKID", SecretAccessKey: "s"}, []string{"c7i.xlarge"})
	if err == nil {
		t.Fatal("accepted a malformed region that derives a foreign host")
	}
	if !strings.Contains(err.Error(), "aws region") {
		t.Errorf("refusal does not name the region rule: %v", err)
	}
}
