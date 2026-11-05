// Package phoenixnap is the host source for phoenixNAP's Bare Metal Cloud (ADR-026),
// over its REST API with a client-credentials token.
//
// WHAT IS PINNED TO A MEASUREMENT, AND WHAT IS NOT. On 2026-11-05 a real account
// answered the token grant, the server catalogue, the stock per location, the quota
// list and an empty server list, and refused an order with 403 (the bare-metal hosts
// record). Those shapes are what this package reads and its tests serve. A server's
// own JSON, an accepted order and a delete have not been answered by the real API
// yet: their fields are the API reference's, and the opt-in real test is what will
// pin them.
//
// STOCK AND QUOTA ARE READ BEFORE AN ORDER, NOT INFERRED FROM ITS REFUSAL, because
// the refusal's wording for either has never been seen: the quota list says whether
// the account may hold another server, and the availability list whether the shape
// is in stock where it is asked for.
package phoenixnap

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/hostsource"
)

// Name is this source's kind.
const Name = "phoenixnap"

const (
	defaultAuth = "https://auth.phoenixnap.com/auth/realms/BMC/protocol/openid-connect/token"
	defaultAPI  = "https://api.phoenixnap.com"

	// serversQuota is the quota whose limit is how many servers the account may hold
	// (measured: `bmc.servers.max_count`, limit 1 on a new account).
	serversQuota = "bmc.servers.max_count"

	// hostnamePrefix marks a server this source ordered. The rest of the hostname is
	// the pool's order, which is how Find reads an order back.
	hostnamePrefix = "billet-"

	// osImage is what a pool host runs: the node's install path is Ubuntu's.
	osImage = "ubuntu/noble"

	// maxBody bounds what is read of any answer; the catalogue, the largest, was 777 KB.
	maxBody = 8 << 20

	// measuredRefusal is the one API message quoted back, as the real API answered an
	// order on 2026-11-05. Every other message is the API's text and may echo what it
	// was sent, so it is not quoted.
	measuredRefusal = "The request failed since this resource cannot be accessed by the provided credentials."
)

// orderPattern bounds an order identifier, which becomes the server's hostname.
var orderPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,47}$`)

// Shape is one server type a pool may order, with its architecture, which the API's
// catalogue does not say in a field.
type Shape struct {
	Type string
	Arch config.Arch
}

// Credentials are an API client's id and secret.
type Credentials struct {
	ClientID string
	Secret   hostsource.Secret
}

func (c Credentials) String() string   { return "phoenixnap.Credentials{client=" + c.ClientID + "}" }
func (c Credentials) GoString() string { return c.String() }

// Format catches every verb.
func (c Credentials) Format(f fmt.State, _ rune) {
	_, _ = io.WriteString(f, c.String()) //nolint:errcheck // fmt.State swallows write errors by design
}

// MarshalJSON keeps the secret out of anything that serialises it structurally.
func (c Credentials) MarshalJSON() ([]byte, error) { return json.Marshal(c.String()) }

// LogValue is what slog consults.
func (c Credentials) LogValue() slog.Value { return slog.StringValue(c.String()) }

// Source orders and releases phoenixNAP servers.
type Source struct {
	creds  Credentials
	shapes map[string]config.Arch
	http   *http.Client
	auth   string
	api    string
	now    func() time.Time

	mu      sync.Mutex
	token   string
	expires time.Time
}

// Option configures a Source.
type Option func(*Source)

// WithEndpoints points the source at other token and API origins, for a test.
func WithEndpoints(auth, api string) Option {
	return func(s *Source) { s.auth, s.api = auth, strings.TrimSuffix(api, "/") }
}

// WithHTTPClient replaces the HTTP client.
func WithHTTPClient(c *http.Client) Option { return func(s *Source) { s.http = c } }

// WithClock replaces the clock, for a test.
func WithClock(now func() time.Time) Option { return func(s *Source) { s.now = now } }

// New builds a source that orders only the shapes named; it calls nothing until asked.
func New(creds Credentials, shapes []Shape, opts ...Option) (*Source, error) {
	if creds.ClientID == "" || creds.Secret.Empty() {
		return nil, errors.New("phoenixnap: an API client id and secret are both required")
	}
	byType := make(map[string]config.Arch, len(shapes))
	for _, sh := range shapes {
		if sh.Type == "" || sh.Arch == "" {
			return nil, fmt.Errorf("phoenixnap: shape %+v needs a type and an architecture", sh)
		}
		byType[sh.Type] = sh.Arch
	}
	if len(byType) == 0 {
		return nil, errors.New("phoenixnap: a source needs at least one shape")
	}
	s := &Source{
		creds:  creds,
		shapes: byType,
		http:   &http.Client{Timeout: 60 * time.Second},
		auth:   defaultAuth,
		api:    defaultAPI,
		now:    time.Now,
	}
	for _, o := range opts {
		o(s)
	}
	// NO REDIRECT IS FOLLOWED, on the default client or one a caller handed in. An
	// order's body carries the join token, and a 307 or 308 would send it again to
	// wherever the answer pointed; a redirect is answered as the refusal it is.
	c := *s.http
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	s.http = &c

	return s, nil
}

var _ hostsource.Source = (*Source)(nil)

// A SOURCE HOLDS THE CREDENTIALS AND A LIVE TOKEN IN UNEXPORTED FIELDS, which fmt
// renders structurally without consulting their own methods, so it redacts itself. On
// a pointer receiver, as awscreds.IMDS does, because it holds a mutex.
func (s *Source) String() string { return "phoenixnap.Source{client=" + s.creds.ClientID + "}" }

// GoString covers %#v.
func (s *Source) GoString() string { return s.String() }

// Format catches every verb.
func (s *Source) Format(f fmt.State, _ rune) {
	_, _ = io.WriteString(f, s.String()) //nolint:errcheck // fmt.State swallows write errors by design
}

// MarshalJSON keeps a source out of anything that serialises it structurally.
func (s *Source) MarshalJSON() ([]byte, error) { return json.Marshal(s.String()) }

// LogValue is what slog consults.
func (s *Source) LogValue() slog.Value { return slog.StringValue(s.String()) }

// Name is the source's kind.
func (s *Source) Name() string { return Name }

// Terms are an hourly plan's: billed by the started hour, no minimum term. The lead
// time is not measured, since no order has been accepted, so it is zero, which the
// pool reads as unknown. An order is NOT idempotent: the API takes no idempotency key,
// so an order whose answer was lost is never placed again without a person.
func (s *Source) Terms(shape string) (hostsource.Terms, error) {
	if _, ok := s.shapes[shape]; !ok {
		return hostsource.Terms{}, fmt.Errorf("phoenixnap: %s is not a declared shape", shape)
	}

	return hostsource.Terms{BillingUnit: time.Hour}, nil
}

type orderBody struct {
	Hostname              string          `json:"hostname"`
	Description           string          `json:"description"`
	OS                    string          `json:"os"`
	Type                  string          `json:"type"`
	Location              string          `json:"location"`
	PricingModel          string          `json:"pricingModel"`
	InstallDefaultSSHKeys bool            `json:"installDefaultSshKeys"`
	OSConfiguration       osConfiguration `json:"osConfiguration"`
}

type osConfiguration struct {
	CloudInit cloudInit `json:"cloudInit"`
}

type cloudInit struct {
	UserData string `json:"userData"`
}

// server is the API reference's server, not yet a measured one.
type server struct {
	ID                string   `json:"id"`
	Status            string   `json:"status"`
	Hostname          string   `json:"hostname"`
	Type              string   `json:"type"`
	Location          string   `json:"location"`
	PublicIPAddresses []string `json:"publicIpAddresses"`
}

// Order checks the quota and the stock, then orders one hourly server whose first
// boot runs req.Boot through cloud-init.
func (s *Source) Order(ctx context.Context, req hostsource.Request) (hostsource.Host, error) {
	arch, declared := s.shapes[req.Shape]
	switch {
	case !orderPattern.MatchString(req.Order):
		return hostsource.Host{}, fmt.Errorf("phoenixnap: order %q is not 1 to 48 lowercase letters, digits and dashes", req.Order)
	case req.Location == "":
		return hostsource.Host{}, errors.New("phoenixnap: an order needs a location")
	case !declared:
		return hostsource.Host{}, fmt.Errorf("phoenixnap: %s is not a declared shape", req.Shape)
	case req.Arch != "" && req.Arch != arch:
		return hostsource.Host{}, fmt.Errorf("phoenixnap: %s is %s, not %s", req.Shape, arch, req.Arch)
	}
	if err := s.checkQuota(ctx); err != nil {
		return hostsource.Host{}, err
	}
	if err := s.checkStock(ctx, req.Shape, req.Location); err != nil {
		return hostsource.Host{}, err
	}
	body, err := json.Marshal(orderBody{
		Hostname:     hostnamePrefix + req.Order,
		Description:  "billet pool order " + req.Order,
		OS:           osImage,
		Type:         req.Shape,
		Location:     req.Location,
		PricingModel: "HOURLY",
		OSConfiguration: osConfiguration{CloudInit: cloudInit{
			UserData: base64.StdEncoding.EncodeToString(req.Boot.Reveal()),
		}},
	})
	if err != nil {
		return hostsource.Host{}, fmt.Errorf("phoenixnap: encode the order: %w", err)
	}
	var got server
	if err := s.call(ctx, http.MethodPost, "/bmc/v1/servers", body, &got); err != nil {
		return hostsource.Host{}, err
	}
	if got.ID == "" {
		return hostsource.Host{}, errors.New("phoenixnap: the order was answered without a server id, so whether a server was ordered cannot be told")
	}

	return got.host(), nil
}

// Describe reads one server.
func (s *Source) Describe(ctx context.Context, id string) (hostsource.Host, error) {
	var got server
	if err := s.call(ctx, http.MethodGet, "/bmc/v1/servers/"+url.PathEscape(id), nil, &got); err != nil {
		return hostsource.Host{}, err
	}
	if got.ID != id {
		return hostsource.Host{}, fmt.Errorf("phoenixnap: asked for server %s and was answered about %q", id, got.ID)
	}

	return got.host(), nil
}

// Find reads back the server an order placed, by its hostname.
func (s *Source) Find(ctx context.Context, order string) (hostsource.Host, bool, error) {
	var all []server
	if err := s.call(ctx, http.MethodGet, "/bmc/v1/servers", nil, &all); err != nil {
		return hostsource.Host{}, false, err
	}
	want := hostnamePrefix + order
	var found []server
	for _, sv := range all {
		if sv.ID == "" {
			return hostsource.Host{}, false, errors.New("phoenixnap: the server list holds a server without an id, so the list cannot be read")
		}
		if sv.Hostname == want {
			found = append(found, sv)
		}
	}
	switch len(found) {
	case 0:
		return hostsource.Host{}, false, nil
	case 1:
		return found[0].host(), true, nil
	}

	return hostsource.Host{}, false, fmt.Errorf("phoenixnap: %d servers carry order %q", len(found), order)
}

// Gone proves a server present, and never proves one gone yet.
//
// NOT FOUND IS COULD-NOT-TELL, HOWEVER LONG IT LASTS. What the API answers for a
// deleted server, and when, has not been measured: no order has been accepted, so no
// delete has been seen. Elapsed time turning a 404 into "gone" is a timer authorising
// a release, so until a delete is measured and its terminal answer pinned here, a pool
// host on this source leaves the pool by an operator's word.
func (s *Source) Gone(ctx context.Context, id string) (hostsource.Presence, error) {
	var got server
	err := s.call(ctx, http.MethodGet, "/bmc/v1/servers/"+url.PathEscape(id), nil, &got)
	switch {
	case err == nil && got.ID == id:
		return hostsource.PresencePresent, nil
	case err == nil:
		return hostsource.PresenceUnknown, fmt.Errorf("phoenixnap: asked for server %s and was answered about %q", id, got.ID)
	case errors.Is(err, errNotFound):
		return hostsource.PresenceUnknown, nil
	}

	return hostsource.PresenceUnknown, err
}

// Release deletes the server the proof names.
func (s *Source) Release(ctx context.Context, proof hostsource.ReleaseProof) error {
	if err := proof.For(proof.Host()); err != nil {
		return err
	}
	err := s.call(ctx, http.MethodDelete, "/bmc/v1/servers/"+url.PathEscape(proof.Host()), nil, nil)
	if errors.Is(err, errNotFound) {
		return nil
	}

	return err
}

type quota struct {
	ID    string `json:"id"`
	Limit *int   `json:"limit"`
	Used  *int   `json:"used"`
}

func (s *Source) checkQuota(ctx context.Context) error {
	var quotas []quota
	if err := s.call(ctx, http.MethodGet, "/bmc/v1/quotas", nil, &quotas); err != nil {
		return err
	}
	for _, q := range quotas {
		if q.ID != serversQuota {
			continue
		}
		if q.Limit == nil || q.Used == nil {
			return fmt.Errorf("phoenixnap: the %s quota came without its limit or use, so whether another server fits cannot be told", serversQuota)
		}
		if *q.Used >= *q.Limit {
			return fmt.Errorf("%w: phoenixnap: the account holds %d of %d servers", hostsource.ErrQuota, *q.Used, *q.Limit)
		}

		return nil
	}

	return fmt.Errorf("phoenixnap: the quota list names no %s, so whether another server fits cannot be told", serversQuota)
}

type availability struct {
	ProductCode string `json:"productCode"`
	// Locations is a pointer so an omitted or null list is told apart from an empty
	// one: the first cannot be read, the second (measured) means none in stock.
	Locations *[]struct {
		Location  string `json:"location"`
		Available *bool  `json:"minQuantityAvailable"`
		Quantity  *int   `json:"availableQuantity"`
	} `json:"locationAvailabilityDetails"`
}

func (s *Source) checkStock(ctx context.Context, shape, location string) error {
	var all []availability
	path := "/billing/v1/product-availability?productCategory=SERVER&showOnlyMinQuantityAvailable=true"
	if err := s.call(ctx, http.MethodGet, path, nil, &all); err != nil {
		return err
	}
	for _, a := range all {
		if a.ProductCode == "" {
			return fmt.Errorf("phoenixnap: the stock list holds an entry without a product, so the stock of %s cannot be told", shape)
		}
		if a.ProductCode != shape {
			continue
		}
		if a.Locations == nil {
			return fmt.Errorf("phoenixnap: the stock of %s came without its locations, so it cannot be told", shape)
		}
		for _, l := range *a.Locations {
			if l.Location == "" {
				return fmt.Errorf("phoenixnap: the stock of %s names an entry without a location, so it cannot be told", shape)
			}
			if l.Location != location {
				continue
			}
			if l.Available == nil || l.Quantity == nil {
				return fmt.Errorf("phoenixnap: the stock of %s in %s came without its quantity, so it cannot be told", shape, location)
			}
			if *l.Available && *l.Quantity > 0 {
				return nil
			}
		}
	}

	return fmt.Errorf("%w: phoenixnap: no %s in %s", hostsource.ErrNoStock, shape, location)
}

func (sv server) host() hostsource.Host {
	h := hostsource.Host{
		ID:              sv.ID,
		Shape:           sv.Type,
		Location:        sv.Location,
		PublicAddresses: sv.PublicIPAddresses,
		State:           stateOf(sv.Status),
	}
	if order, ok := strings.CutPrefix(sv.Hostname, hostnamePrefix); ok {
		h.Order = order
	}

	return h
}

// stateOf maps the API reference's statuses; anything else is could-not-tell.
func stateOf(status string) hostsource.State {
	switch status {
	case "creating", "reinstalling", "resetting":
		return hostsource.StateProvisioning
	case "powered-on", "powered-off", "rebooting":
		return hostsource.StateRunning
	case "error":
		return hostsource.StateFailed
	case "deleting":
		return hostsource.StateReleasing
	}

	return hostsource.StateUnknown
}

var errNotFound = errors.New("phoenixnap: not found")

// errWithheld is a call that failed without an answer; its text is not kept.
var errWithheld = errors.New("the request failed and its error is withheld: a request here carries a credential")

// withheld keeps only the classification of a transport failure. net/http's error
// renders the URL it was answered with, a malformed redirect's Location included,
// before any redirect policy runs, and that can carry the request back.
func withheld(err error) error {
	switch {
	case errors.Is(err, context.Canceled):
		return fmt.Errorf("%w: %w", errWithheld, context.Canceled)
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("%w: %w", errWithheld, context.DeadlineExceeded)
	}

	return errWithheld
}

// apiError is a refusal the API answered. Its message is the API's only when it is the
// one measured refusal; otherwise it is the status's own text, because the API's could
// echo the order's body, which carries the join token.
type apiError struct {
	Status  int
	Message string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("phoenixnap: http %d: %s", e.Status, e.Message)
}

func (s *Source) call(ctx context.Context, method, path string, body []byte, out any) error {
	token, err := s.bearer(ctx)
	if err != nil {
		return err
	}
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, s.api+path, rd)
	if err != nil {
		return fmt.Errorf("phoenixnap: build %s %s: %w", method, path, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return fmt.Errorf("phoenixnap: %s %s: %w", method, path, withheld(err))
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return fmt.Errorf("phoenixnap: read %s %s: %w", method, path, withheld(err))
	}
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return errNotFound
	case resp.StatusCode == http.StatusForbidden:
		// MEASURED 2026-11-05: an order the API client may not place is a 403 whose
		// message says the credentials cannot access the resource. An operator acts on
		// it; ordering again changes nothing.
		return fmt.Errorf("%w: %w", hostsource.ErrRefused, refusal(resp.StatusCode, raw))
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		return refusal(resp.StatusCode, raw)
	}
	if out == nil {
		return nil
	}
	// AN EMPTY OR NULL ANSWER IS NOT AN EMPTY LIST. Decoded as one, a list of servers
	// that said nothing would prove an order was never placed.
	if trimmed := bytes.TrimSpace(raw); len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return fmt.Errorf("phoenixnap: %s %s answered nothing, which cannot be read", method, path)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		// NOT QUOTED: a decode error repeats part of the answer.
		return fmt.Errorf("phoenixnap: %s %s: the answer would not parse, and is not quoted", method, path)
	}

	return nil
}

func refusal(status int, raw []byte) error {
	var m struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &m) == nil && m.Message == measuredRefusal {
		return &apiError{Status: status, Message: m.Message}
	}

	return &apiError{Status: status, Message: http.StatusText(status)}
}

// bearer is a token from the client-credentials grant, reused until a minute before
// it expires.
func (s *Source) bearer(ctx context.Context) (string, error) {
	s.mu.Lock()
	if s.token != "" && s.now().Before(s.expires) {
		t := s.token
		s.mu.Unlock()

		return t, nil
	}
	s.mu.Unlock()

	form := url.Values{"grant_type": {"client_credentials"}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.auth, strings.NewReader(form))
	if err != nil {
		return "", fmt.Errorf("phoenixnap: build the token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(s.creds.ClientID, string(s.creds.Secret.Reveal()))
	resp, err := s.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("phoenixnap: token: %w", withheld(err))
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return "", fmt.Errorf("phoenixnap: read the token: %w", withheld(err))
	}
	if resp.StatusCode != http.StatusOK {
		// The body of a refused grant is not quoted: it is the identity provider's, and
		// nothing here has measured that it never echoes what it was sent.
		return "", fmt.Errorf("phoenixnap: the token grant answered http %d", resp.StatusCode)
	}
	var tok struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(raw, &tok); err != nil || tok.AccessToken == "" {
		return "", errors.New("phoenixnap: the token grant answered without a token")
	}
	life := time.Duration(tok.ExpiresIn)*time.Second - time.Minute
	s.mu.Lock()
	s.token, s.expires = tok.AccessToken, s.now().Add(life)
	s.mu.Unlock()

	return tok.AccessToken, nil
}
