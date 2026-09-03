package ec2

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/junioryono/billet/internal/awscreds"
	"github.com/junioryono/billet/internal/config"
)

const (
	pricingService     = "pricing"
	pricingContentType = "application/x-amz-json-1.1"
	pricingTarget      = "AWSPriceListService.GetProducts"
	// pricingHourlyUnit is the priceDimensions unit an hourly compute rate carries;
	// anything else (a per-request or per-GB dimension) is not what billet reports.
	pricingHourlyUnit = "Hrs"
	// microsPerUSD is the config's price scale: config.USDPerHour is int64
	// millionths of a dollar, and a fetched rate is rounded into that unit here.
	microsPerUSD = 1_000_000
)

// pricingEndpoint returns the (signing region, endpoint URL) the Price List API
// is signed and served from for the partition an EC2 region belongs to. The API
// is not regional the way EC2 is: the commercial partition is served from
// us-east-1 regardless of which region's prices are asked for, and the China
// partition from cn-north-1. GovCloud has no Price List endpoint of its own, so it
// is pointed at us-east-1; a GovCloud credential cannot sign a commercial-partition
// request, so that fetch fails auth and the caller falls back to --price.
func pricingEndpoint(ec2Region string) (string, string) {
	if strings.HasPrefix(ec2Region, "cn-") {
		return "cn-north-1", "https://api.pricing.cn-north-1.amazonaws.com.cn/"
	}

	return "us-east-1", "https://api.pricing.us-east-1.amazonaws.com/"
}

// pricingFilter is one TERM_MATCH clause of a GetProducts query.
type pricingFilter struct {
	Type  string `json:"Type"`
	Field string `json:"Field"`
	Value string `json:"Value"`
}

// OnDemandPriceUSDPerHour fetches the on-demand hourly rate of one shape in one
// region from the AWS Price List API, so `billet init` can fill the
// price_usd_per_hour a config must carry rather than leaving the operator to look
// it up.
//
// THE PRICE IS NOT AN ADMISSION GATE (config records it only to REPORT the
// maximum configured exposure), so a fetch that cannot answer unambiguously is not
// fatal to billet — it is fatal to guessing. This returns an error the caller
// turns into a prompt for an explicit --price rather than writing a number it is
// unsure of: a single positive hourly USD rate across the returned products is
// used; zero, or more than one DISTINCT rate, is refused. Distinctness is judged
// on the exact published rational, BEFORE rounding, so two rates that would round
// to the same millionth are still seen as two — only the single surviving rate is
// then rounded into the six decimals the config grammar admits.
func OnDemandPriceUSDPerHour(
	ctx context.Context, ec2Region, instanceType string, creds awscreds.Source,
) (config.USDPerHour, error) {
	signRegion, endpoint := pricingEndpoint(ec2Region)

	return onDemandPriceFrom(ctx, endpoint, signRegion, ec2Region, instanceType, creds)
}

// onDemandPriceFrom is OnDemandPriceUSDPerHour with the endpoint and signing
// region supplied, so a test can point it at a local server. It follows the
// response's pagination to the end before selecting a rate: a rate that appears
// only on a later page must not be missed, or a genuinely ambiguous shape would
// read as unambiguous.
func onDemandPriceFrom(
	ctx context.Context, endpoint, signRegion, ec2Region, instanceType string, creds awscreds.Source,
) (config.USDPerHour, error) {
	c := discoveryClient(signRegion, endpoint, creds)

	var docs []string

	seen := make(map[string]bool)
	token := ""
	for {
		input := map[string]any{
			"ServiceCode":   "AmazonEC2",
			"FormatVersion": "aws_v1",
			"MaxResults":    100,
			"Filters": []pricingFilter{
				{Type: "TERM_MATCH", Field: "instanceType", Value: instanceType},
				{Type: "TERM_MATCH", Field: "regionCode", Value: ec2Region},
				{Type: "TERM_MATCH", Field: "operatingSystem", Value: "Linux"},
				{Type: "TERM_MATCH", Field: "tenancy", Value: "Shared"},
				{Type: "TERM_MATCH", Field: "preInstalledSw", Value: "NA"},
				{Type: "TERM_MATCH", Field: "capacitystatus", Value: "Used"},
			},
		}
		if token != "" {
			input["NextToken"] = token
		}

		var out struct {
			PriceList []string `json:"PriceList"`
			NextToken string   `json:"NextToken"`
		}
		if err := c.jsonCall(ctx, endpoint, signRegion, pricingTarget, input, &out); err != nil {
			return 0, fmt.Errorf("ec2: fetch price for %s in %s: %w", instanceType, ec2Region, err)
		}

		docs = append(docs, out.PriceList...)

		if out.NextToken == "" {
			break
		}
		if seen[out.NextToken] {
			return 0, fmt.Errorf("ec2: the price list cycled its pagination token for %s in %s",
				instanceType, ec2Region)
		}

		seen[out.NextToken] = true
		token = out.NextToken
	}

	rates, err := distinctOnDemandRates(docs)
	if err != nil {
		return 0, fmt.Errorf("ec2: price for %s in %s: %w", instanceType, ec2Region, err)
	}

	switch len(rates) {
	case 0:
		return 0, fmt.Errorf("ec2: the price list returned no on-demand hourly USD rate for %s in "+
			"%s; pass its rate as --price", instanceType, ec2Region)
	case 1:
		micros, ok := microsFromRat(rates[0])
		if !ok {
			return 0, fmt.Errorf("ec2: the on-demand rate for %s in %s cannot be represented; pass "+
				"--price", instanceType, ec2Region)
		}

		return config.USDPerHour(micros), nil
	default:
		return 0, fmt.Errorf("ec2: the price list returned %d distinct on-demand rates for %s in %s; "+
			"pass the one you mean as --price", len(rates), instanceType, ec2Region)
	}
}

// pricingProduct is the slice of one PriceList document billet reads: the
// on-demand terms and, within each, the hourly USD price dimensions.
type pricingProduct struct {
	Terms struct {
		OnDemand map[string]struct {
			PriceDimensions map[string]struct {
				Unit         string `json:"unit"`
				PricePerUnit struct {
					USD string `json:"USD"`
				} `json:"pricePerUnit"`
			} `json:"priceDimensions"`
		} `json:"OnDemand"`
	} `json:"terms"`
}

// distinctOnDemandRates collects the DISTINCT positive on-demand hourly USD rates
// across every returned product, as exact rationals. Distinct rather than every
// occurrence, because one product repeats its single rate across offer- and
// rate-code keys; two genuinely different rates (a different operation code
// slipping past the filters) is the ambiguity the caller refuses. Deduplication
// is on the exact published value, NOT on the rounded one, so two rates that would
// collapse to the same millionth are correctly still two.
func distinctOnDemandRates(priceList []string) ([]*big.Rat, error) {
	seen := make(map[string]struct{})

	var out []*big.Rat

	for _, doc := range priceList {
		var product pricingProduct
		if err := json.Unmarshal([]byte(doc), &product); err != nil {
			return nil, fmt.Errorf("parse a price list entry: %w", err)
		}

		for _, term := range product.Terms.OnDemand {
			for _, dim := range term.PriceDimensions {
				if dim.Unit != pricingHourlyUnit {
					continue
				}

				r, ok := new(big.Rat).SetString(strings.TrimSpace(dim.PricePerUnit.USD))
				if !ok || r.Sign() <= 0 {
					continue
				}

				key := r.RatString()
				if _, dup := seen[key]; dup {
					continue
				}

				seen[key] = struct{}{}
				out = append(out, r)
			}
		}
	}

	return out, nil
}

// microsFromUSD converts a decimal-dollar string to millionths of a dollar,
// rounding to nearest. Kept as the string entry point; the arithmetic is
// microsFromRat's.
func microsFromUSD(usd string) (int64, bool) {
	r, ok := new(big.Rat).SetString(strings.TrimSpace(usd))
	if !ok {
		return 0, false
	}

	return microsFromRat(r)
}

// microsFromRat rounds an exact dollar rational to nearest millionth. The Price
// List publishes rates to ten decimal places and the config grammar admits six;
// exact rational arithmetic keeps the rounding deterministic and unable to drift a
// boundary rate. A negative rate is not a price, and a value too large for int64
// micros is refused rather than wrapped.
func microsFromRat(r *big.Rat) (int64, bool) {
	if r.Sign() < 0 {
		return 0, false
	}

	scaled := new(big.Rat).Mul(r, big.NewRat(microsPerUSD, 1))

	// Round half up: (num + den/2) / den on the reduced fraction. den is positive
	// for a big.Rat, and scaled is non-negative here.
	num := scaled.Num()
	den := scaled.Denom()
	half := new(big.Int).Rsh(den, 1)
	rounded := new(big.Int).Add(num, half)
	rounded.Quo(rounded, den)

	if !rounded.IsInt64() {
		return 0, false
	}

	return rounded.Int64(), true
}

// jsonCall issues one AWS JSON-protocol POST (the shape the Price List and SQS
// APIs speak) and decodes the reply, signing for the given service and region.
// It mirrors the sqs client's call rather than client.call, which speaks the EC2
// query protocol.
func (c *client) jsonCall(
	ctx context.Context, endpoint, signRegion, target string, input, output any,
) error {
	body, err := json.Marshal(input)
	if err != nil {
		return fmt.Errorf("encode %s: %w", target, err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build %s request: %w", target, err)
	}

	req.Header.Set("Content-Type", pricingContentType)
	req.Header.Set("X-Amz-Target", target)
	req.ContentLength = int64(len(body))

	creds, err := c.creds.Credentials(ctx)
	if err != nil {
		return fmt.Errorf("resolve aws credentials: %w", err)
	}

	now := time.Now
	if c.now != nil {
		now = c.now
	}

	if err := signService(req, body, creds, signRegion, pricingService, now()); err != nil {
		return err
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("call %s: %w", target, err)
	}

	defer func() { _ = resp.Body.Close() }()

	payload, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("read %s response: %w", target, err)
	}

	if resp.StatusCode != http.StatusOK {
		return errors.New(pricingErrorMessage(payload, resp.StatusCode))
	}

	if output == nil || len(payload) == 0 {
		return nil
	}

	if err := json.Unmarshal(payload, output); err != nil {
		return fmt.Errorf("parse %s response: %w", target, err)
	}

	return nil
}

// pricingErrorMessage renders a non-200 from a JSON-protocol service. AWS returns
// a __type and message; a body that is not that shape falls back to the status so
// the caller still gets something actionable.
func pricingErrorMessage(payload []byte, status int) string {
	var e struct {
		Type    string `json:"__type"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(payload, &e); err == nil && (e.Type != "" || e.Message != "") {
		return fmt.Sprintf("aws pricing returned http %d: %s %s", status, e.Type, e.Message)
	}

	return fmt.Sprintf("aws pricing returned http %d", status)
}
