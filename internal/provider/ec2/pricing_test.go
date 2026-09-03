package ec2

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/awscreds"
	"github.com/junioryono/billet/internal/config"
)

// THE PUBLISHED RATE IS ROUNDED INTO THE CONFIG'S PRECISION AS IT IS READ, not
// left to overflow the six-decimal grammar later with nothing pointing back here.
func TestMicrosFromUSD(t *testing.T) {
	cases := []struct {
		usd    string
		micros int64
		ok     bool
	}{
		{"0.17", 170000, true},
		{"0.1700000000", 170000, true}, // ten decimals collapse to the same rate
		{"0.1234565", 123457, true},    // half rounds up
		{"0.1234564", 123456, true},    // below half truncates
		{"1.5", 1500000, true},
		{"0", 0, true},                                      // parsed, but the caller skips a zero rate
		{"-0.5", 0, false},                                  // a negative rate is not a price
		{"not-a-number", 0, false},                          // unparseable
		{"9223372036854.775807", 9223372036854775807, true}, // exactly MaxInt64 micros
		{"9223372036854.775808", 0, false},                  // one past int64 micros overflows
	}

	for _, c := range cases {
		micros, ok := microsFromUSD(c.usd)
		if ok != c.ok || (ok && micros != c.micros) {
			t.Errorf("microsFromUSD(%q) = (%d, %v), want (%d, %v)", c.usd, micros, ok, c.micros, c.ok)
		}
	}
}

// DISTINCT RATES ARE JUDGED ON THE EXACT PUBLISHED VALUE, before rounding: a
// product repeating one rate is one price, two rates that merely round alike are
// still two, and that is what makes an ambiguous shape fall back to --price rather
// than silently pick one.
func TestDistinctOnDemandRates(t *testing.T) {
	// One product, its single rate repeated across offer/rate-code keys.
	single := priceDoc(t, map[string]map[string]priceDim{
		"OFFER1": {"RATE1": {Unit: "Hrs", USD: "0.170000"}},
		"OFFER2": {"RATE2": {Unit: "Hrs", USD: "0.170000"}},
	})
	got, err := distinctOnDemandRates([]string{single})
	if err != nil {
		t.Fatalf("distinctOnDemandRates: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("repeated equal rate not deduped: %d rates", len(got))
	}

	// A non-hourly dimension and a zero rate are both ignored.
	noise := priceDoc(t, map[string]map[string]priceDim{
		"OFFER1": {
			"RATE1": {Unit: "Quantity", USD: "0.500000"},
			"RATE2": {Unit: "Hrs", USD: "0"},
		},
	})
	got, err = distinctOnDemandRates([]string{noise})
	if err != nil {
		t.Fatalf("distinctOnDemandRates noise: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("non-hourly and zero rates leaked through: %d rates", len(got))
	}

	// Two rates that ROUND to the same millionth (0.12345641 and 0.12345649 both
	// -> 123456 micros) are still exactly distinct, so this is ambiguous, not one.
	a := priceDoc(t, map[string]map[string]priceDim{"O": {"R": {Unit: "Hrs", USD: "0.12345641"}}})
	b := priceDoc(t, map[string]map[string]priceDim{"O": {"R": {Unit: "Hrs", USD: "0.12345649"}}})
	got, err = distinctOnDemandRates([]string{a, b})
	if err != nil {
		t.Fatalf("distinctOnDemandRates round-collision: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("rates distinct only past the sixth decimal were merged: %d rates", len(got))
	}
}

// THE PRICE LIST IS NOT REGIONAL THE WAY EC2 IS: a commercial region signs at
// us-east-1, a China region at cn-north-1.
func TestPricingEndpoint(t *testing.T) {
	if r, e := pricingEndpoint("us-west-2"); r != "us-east-1" ||
		!strings.Contains(e, "api.pricing.us-east-1.amazonaws.com") {
		t.Errorf("us-west-2 -> (%q, %q)", r, e)
	}
	if r, e := pricingEndpoint("cn-north-1"); r != "cn-north-1" ||
		!strings.Contains(e, "api.pricing.cn-north-1.amazonaws.com.cn") {
		t.Errorf("cn-north-1 -> (%q, %q)", r, e)
	}
}

// THE JSON POST CARRIES THE TARGET AND CONTENT TYPE the Price List API requires,
// and is SIGNED FOR THE RIGHT SERVICE AND REGION — a signature scoped to the ec2
// service would pass a non-empty-Authorization check yet fail live.
func TestJSONCallSignsAndDecodes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Amz-Target"); got != pricingTarget {
			t.Errorf("X-Amz-Target = %q, want %q", got, pricingTarget)
		}
		if got := r.Header.Get("Content-Type"); got != pricingContentType {
			t.Errorf("Content-Type = %q, want %q", got, pricingContentType)
		}
		if auth := r.Header.Get("Authorization"); !strings.Contains(auth, "/us-east-1/pricing/aws4_request") {
			t.Errorf("not signed for the pricing service in us-east-1: %q", auth)
		}
		if err := json.NewEncoder(w).Encode(map[string]any{"PriceList": []string{
			priceDoc(t, map[string]map[string]priceDim{"O": {"R": {Unit: "Hrs", USD: "0.170000"}}}),
		}}); err != nil {
			t.Errorf("encode response: %v", err)
		}
	}))
	t.Cleanup(srv.Close)

	c := discoveryClient("us-east-1", srv.URL, awscreds.Static{AccessKeyID: "AKID", SecretAccessKey: "s"})

	var out struct {
		PriceList []string `json:"PriceList"`
	}
	if err := c.jsonCall(t.Context(), srv.URL, "us-east-1", pricingTarget,
		map[string]any{"ServiceCode": "AmazonEC2"}, &out); err != nil {
		t.Fatalf("jsonCall: %v", err)
	}
	if len(out.PriceList) != 1 {
		t.Fatalf("PriceList = %v", out.PriceList)
	}
}

// AN AWS ERROR STATUS IS SURFACED with its type and message rather than decoded
// as an empty price list.
func TestJSONCallSurfacesAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"__type":"ValidationException","message":"bad filter"}`)
	}))
	t.Cleanup(srv.Close)

	c := discoveryClient("us-east-1", srv.URL, awscreds.Static{AccessKeyID: "AKID", SecretAccessKey: "s"})

	err := c.jsonCall(t.Context(), srv.URL, "us-east-1", pricingTarget, map[string]any{}, nil)
	if err == nil {
		t.Fatal("jsonCall accepted an error status")
	}
	if !strings.Contains(err.Error(), "ValidationException") || !strings.Contains(err.Error(), "bad filter") {
		t.Errorf("error does not carry the AWS type/message: %v", err)
	}
}

// THE TOP-LEVEL FETCH sends the whole filter set, follows pagination, and resolves
// exactly-one to a value while zero and several are refused. Driven through the
// injectable endpoint so the request body and multi-page selection are pinned.
func TestOnDemandPriceFrom(t *testing.T) {
	creds := awscreds.Static{AccessKeyID: "AKID", SecretAccessKey: "s"}

	// A single rate, delivered across two pages, resolves to that rate.
	t.Run("single across two pages", func(t *testing.T) {
		var page int
		var sawFilters bool
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body := readBody(t, r)
			for _, want := range []string{`"instanceType"`, `"c7i.xlarge"`, `"regionCode"`,
				`"us-west-2"`, `"operatingSystem"`, `"Linux"`, `"tenancy"`, `"Shared"`,
				`"preInstalledSw"`, `"NA"`, `"capacitystatus"`, `"Used"`} {
				if !strings.Contains(body, want) {
					t.Errorf("filter body missing %s: %s", want, body)
				}
			}
			sawFilters = true

			page++
			if page == 2 && !strings.Contains(body, `"NextToken":"next"`) {
				t.Errorf("page 2 did not carry the pagination token: %s", body)
			}
			resp := map[string]any{"PriceList": []string{
				priceDoc(t, map[string]map[string]priceDim{"O": {"R": {Unit: "Hrs", USD: "0.170000"}}}),
			}}
			if page == 1 {
				resp["NextToken"] = "next"
			}
			if err := json.NewEncoder(w).Encode(resp); err != nil {
				t.Errorf("encode: %v", err)
			}
		}))
		t.Cleanup(srv.Close)

		price, err := onDemandPriceFrom(t.Context(), srv.URL, "us-east-1", "us-west-2", "c7i.xlarge", creds)
		if err != nil {
			t.Fatalf("onDemandPriceFrom: %v", err)
		}
		if price != config.USDPerHour(170000) {
			t.Errorf("price = %v, want 170000", price)
		}
		if page != 2 || !sawFilters {
			t.Errorf("pages=%d sawFilters=%v", page, sawFilters)
		}
	})

	// A second, different rate on page two makes the shape ambiguous.
	t.Run("ambiguous across pages", func(t *testing.T) {
		var page int
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			page++
			usd := "0.170000"
			resp := map[string]any{}
			if page == 1 {
				resp["NextToken"] = "next"
			} else {
				usd = "0.340000"
			}
			resp["PriceList"] = []string{
				priceDoc(t, map[string]map[string]priceDim{"O": {"R": {Unit: "Hrs", USD: usd}}}),
			}
			if err := json.NewEncoder(w).Encode(resp); err != nil {
				t.Errorf("encode: %v", err)
			}
		}))
		t.Cleanup(srv.Close)

		_, err := onDemandPriceFrom(t.Context(), srv.URL, "us-east-1", "us-west-2", "c7i.xlarge", creds)
		if err == nil || !strings.Contains(err.Error(), "distinct on-demand rates") {
			t.Fatalf("want an ambiguity refusal, got %v", err)
		}
	})

	// No hourly rate at all is refused with a prompt for --price.
	t.Run("no rate", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if err := json.NewEncoder(w).Encode(map[string]any{"PriceList": []string{}}); err != nil {
				t.Errorf("encode: %v", err)
			}
		}))
		t.Cleanup(srv.Close)

		_, err := onDemandPriceFrom(t.Context(), srv.URL, "us-east-1", "us-west-2", "c7i.xlarge", creds)
		if err == nil || !strings.Contains(err.Error(), "no on-demand hourly USD rate") {
			t.Fatalf("want a no-rate refusal, got %v", err)
		}
	})

	// A repeated pagination token is refused rather than looped on forever.
	t.Run("repeated token", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if err := json.NewEncoder(w).Encode(map[string]any{
				"PriceList": []string{},
				"NextToken": "stuck",
			}); err != nil {
				t.Errorf("encode: %v", err)
			}
		}))
		t.Cleanup(srv.Close)

		_, err := onDemandPriceFrom(t.Context(), srv.URL, "us-east-1", "us-west-2", "c7i.xlarge", creds)
		if err == nil || !strings.Contains(err.Error(), "cycled its pagination token") {
			t.Fatalf("want a cycled-token refusal, got %v", err)
		}
	})
}

// readBody reads a request body for assertion.
func readBody(t *testing.T, r *http.Request) string {
	t.Helper()
	data, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}

	return string(data)
}

// priceDim and priceDoc build a Price List document string in the shape GetProducts
// returns (each PriceList entry is itself a JSON string).
type priceDim struct {
	Unit string
	USD  string
}

func priceDoc(t *testing.T, onDemand map[string]map[string]priceDim) string {
	t.Helper()

	terms := map[string]any{}
	for offer, dims := range onDemand {
		pd := map[string]any{}
		for rate, d := range dims {
			pd[rate] = map[string]any{
				"unit":         d.Unit,
				"pricePerUnit": map[string]string{"USD": d.USD},
			}
		}
		terms[offer] = map[string]any{"priceDimensions": pd}
	}

	doc, err := json.Marshal(map[string]any{"terms": map[string]any{"OnDemand": terms}})
	if err != nil {
		t.Fatalf("marshal price doc: %v", err)
	}

	return string(doc)
}
