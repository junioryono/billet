// Package awssig signs AWS Signature Version 4 requests without an SDK dependency.
package awssig

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	algorithm     = "AWS4-HMAC-SHA256"
	terminator    = "aws4_request"
	amzDateFormat = "20060102T150405Z"
	amzDayFormat  = "20060102"
)

// ErrNoCredentials means a request cannot be signed because a key pair is absent.
var ErrNoCredentials = errors.New("aws: no credentials")

// Credentials are the material SigV4 needs. Every rendering path redacts secrets.
type Credentials struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
}

// String renders only the diagnostic key identifier.
func (c Credentials) String() string { return c.redacted() }

// GoString covers %#v.
func (c Credentials) GoString() string { return c.redacted() }

// Format catches every fmt verb.
func (c Credentials) Format(f fmt.State, _ rune) {
	if _, err := io.WriteString(f, c.redacted()); err != nil {
		return
	}
}

// MarshalJSON prevents structural serializers from exposing the secret fields.
func (c Credentials) MarshalJSON() ([]byte, error) { return json.Marshal(c.redacted()) }

// LogValue is the slog-safe rendering.
func (c Credentials) LogValue() slog.Value { return slog.StringValue(c.redacted()) }

func (c Credentials) redacted() string {
	if c.AccessKeyID == "" {
		return "awssig.Credentials{none}"
	}

	return "awssig.Credentials{key=" + c.AccessKeyID + ", secret=REDACTED}"
}

// Sign adds SigV4 headers for one AWS service and region.
//
// Its output is exercised by the EC2 package's vector generated with AWS's own
// signer. Keeping services on this one implementation prevents a second reading
// of the signing specification from becoming a second security boundary.
func Sign(
	req *http.Request,
	body []byte,
	creds Credentials,
	region, service string,
	now time.Time,
) error {
	if creds.AccessKeyID == "" || creds.SecretAccessKey == "" {
		return ErrNoCredentials
	}
	if req.URL == nil {
		return errors.New("aws: cannot sign a request with no url")
	}
	if strings.TrimSpace(region) == "" || strings.TrimSpace(service) == "" {
		return errors.New("aws: signing needs a region and service")
	}

	now = now.UTC()
	amzDate := now.Format(amzDateFormat)
	day := now.Format(amzDayFormat)
	req.Header.Set("X-Amz-Date", amzDate)
	if creds.SessionToken != "" {
		req.Header.Set("X-Amz-Security-Token", creds.SessionToken)
	}

	host := req.Host
	if host == "" {
		host = req.URL.Host
	}
	payloadHash := SHA256Hex(body)
	canonicalHeaders, signedHeaders := canonicalizeHeaders(req.Header, host, req.ContentLength)
	query, err := CanonicalQuery(req.URL)
	if err != nil {
		return err
	}
	canonicalRequest := strings.Join([]string{
		req.Method,
		canonicalURI(req.URL),
		query,
		canonicalHeaders,
		signedHeaders,
		payloadHash,
	}, "\n")
	scope := strings.Join([]string{day, region, service, terminator}, "/")
	stringToSign := strings.Join([]string{
		algorithm,
		amzDate,
		scope,
		SHA256Hex([]byte(canonicalRequest)),
	}, "\n")
	signature := hex.EncodeToString(hmacSHA256(
		signingKeyFor(creds.SecretAccessKey, day, region, service), stringToSign))
	req.Header.Set("Authorization", fmt.Sprintf(
		"%s Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		algorithm, creds.AccessKeyID, scope, signedHeaders, signature))

	return nil
}

func signingKeyFor(secret, day, region, service string) []byte {
	key := hmacSHA256([]byte("AWS4"+secret), day)
	key = hmacSHA256(key, region)
	key = hmacSHA256(key, service)

	return hmacSHA256(key, terminator)
}

var unsignedHeaders = map[string]struct{}{
	"authorization":     {},
	"user-agent":        {},
	"x-amzn-trace-id":   {},
	"expect":            {},
	"transfer-encoding": {},
}

func canonicalizeHeaders(h http.Header, host string, contentLength int64) (string, string) {
	values := map[string]string{"host": host}
	if contentLength > 0 {
		values["content-length"] = strconv.FormatInt(contentLength, 10)
	}
	for name, vs := range h {
		lower := strings.ToLower(name)
		if _, skip := unsignedHeaders[lower]; skip {
			continue
		}
		if lower == "host" || lower == "content-length" {
			continue
		}
		trimmed := make([]string, 0, len(vs))
		for _, value := range vs {
			trimmed = append(trimmed, collapseSpace(value))
		}
		values[lower] = strings.Join(trimmed, ",")
	}

	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)

	var b strings.Builder
	for _, name := range names {
		b.WriteString(name)
		b.WriteByte(':')
		b.WriteString(values[name])
		b.WriteByte('\n')
	}

	return b.String(), strings.Join(names, ";")
}

func canonicalURI(u *url.URL) string {
	if u.EscapedPath() == "" {
		return "/"
	}

	return u.EscapedPath()
}

// CanonicalQuery renders a query exactly as AWS's signer does.
func CanonicalQuery(u *url.URL) (string, error) {
	if u.RawQuery == "" {
		return "", nil
	}

	values, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return "", fmt.Errorf("aws: refusing to sign a query that cannot be read faithfully: %w", err)
	}
	for _, vs := range values {
		slices.Sort(vs)
	}

	return strings.ReplaceAll(values.Encode(), "+", "%20"), nil
}

func collapseSpace(value string) string { return strings.Join(strings.Fields(value), " ") }

func hmacSHA256(key []byte, data string) []byte {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(data))

	return mac.Sum(nil)
}

// SHA256Hex returns the lowercase payload digest SigV4 uses.
func SHA256Hex(body []byte) string {
	sum := sha256.Sum256(body)

	return hex.EncodeToString(sum[:])
}
