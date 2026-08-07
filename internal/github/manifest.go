// Package github talks to GitHub's App and Runner Scale Set APIs.
//
// Onboarding uses the App Manifest flow rather than asking an operator to click
// through app creation by hand. Two reasons, and the second is the important
// one: hand registration is roughly fifteen steps and a known adoption barrier
// for comparable tools, and — because the manifest declares the permission set —
// every billet deployment ends up with provably identical, minimal permissions
// instead of whatever the operator happened to tick.
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	apiBase       = "https://api.github.com"
	webBase       = "https://github.com"
	apiVersion    = "2022-11-28"
	acceptJSON    = "application/vnd.github+json"
	requestTimout = 30 * time.Second
)

// ManifestTTL is GitHub's limit on the whole handshake: register, redirect,
// exchange. The exchange fails outright past it, so the CLI says so up front
// rather than letting an operator wander off mid-flow.
const ManifestTTL = time.Hour

// permissions is the complete set billet requests.
//
// Unexported and copied on the way out: it is a security claim, and an exported
// map is one an importing package could rewrite at runtime — changing the
// manifest, the CLI's own disclosure, and the post-install validation together.
//
// Deliberately absent: `actions: read`. It would expose workflow runs, logs and
// artifacts, and billet needs none of them — it is handed jobs by the scale-set
// API rather than discovering them. Its absence is what makes "billet cannot
// read your code" true rather than merely reassuring.
var permissions = map[string]string{
	"metadata":                         "read",
	"organization_self_hosted_runners": "write",
}

// Permissions returns a copy of the permission set billet requests.
func Permissions() map[string]string {
	out := make(map[string]string, len(permissions))
	for k, v := range permissions {
		out[k] = v
	}

	return out
}

// Manifest is the app registration billet asks GitHub to create.
// Field order follows GitHub's documented parameter table.
type Manifest struct {
	Name string `json:"name,omitempty"`
	// URL is required by GitHub even for an app that serves no web surface.
	URL            string            `json:"url"`
	HookAttributes *HookAttributes   `json:"hook_attributes,omitempty"`
	RedirectURL    string            `json:"redirect_url,omitempty"`
	SetupURL       string            `json:"setup_url,omitempty"`
	Description    string            `json:"description,omitempty"`
	Public         bool              `json:"public"`
	DefaultEvents  []string          `json:"default_events,omitempty"`
	Permissions    map[string]string `json:"default_permissions,omitempty"`
	// SetupOnUpdate keeps the installation callback firing when an operator
	// changes which repositories the app can see, so a re-scoped install still
	// lands back here.
	SetupOnUpdate bool `json:"setup_on_update,omitempty"`
}

// HookAttributes configures the app's webhook.
type HookAttributes struct {
	URL    string `json:"url,omitempty"`
	Active bool   `json:"active"`
}

// NewManifest builds billet's manifest. redirectURL and setupURL point at the
// loopback server the CLI runs for the duration of onboarding.
func NewManifest(name, redirectURL, setupURL string) Manifest {
	return Manifest{
		Name:        name,
		URL:         "https://github.com/junioryono/billet",
		Description: "Self-hosted GitHub Actions runners on your own hardware.",
		RedirectURL: redirectURL,
		SetupURL:    setupURL,
		// An app registered for one organization's runners has no reason to be
		// installable by strangers.
		Public: false,
		// billet receives work by long-polling the Runner Scale Set API, so it
		// needs no inbound webhook at all. That is what lets a deployment run with
		// no public ingress, and Active:false keeps the registration honest.
		//
		// The URL is still required: GitHub marks hook_attributes.url mandatory
		// whenever the object is present, and omitting it rejects the whole
		// registration. It is a placeholder that is never called — nothing is
		// delivered to it while Active is false.
		HookAttributes: &HookAttributes{
			URL:    webhookPlaceholderURL,
			Active: false,
		},
		Permissions: Permissions(),
		// Deliberately NOT setting SetupOnUpdate. It would redirect a future
		// repository-access change back to setup_url — a loopback port that
		// stopped existing the moment onboarding finished, so the operator would
		// land on a connection error with no explanation.
		SetupOnUpdate: false,
	}
}

// webhookPlaceholderURL satisfies GitHub's requirement that hook_attributes
// carry a URL. Nothing is ever delivered to it: the hook is registered inactive,
// and billet has no webhook receiver by design.
const webhookPlaceholderURL = "https://github.com/junioryono/billet#billet-uses-no-webhooks"

// RegistrationURL is where the browser POSTs the manifest form.
//
// It must be a form POST, not a redirect: the manifest travels in the request
// body as a `manifest` field, which is why the CLI serves a self-submitting page
// rather than simply opening a URL.
func RegistrationURL(org, state string) string {
	return fmt.Sprintf("%s/organizations/%s/settings/apps/new?state=%s",
		webBase, urlPathEscape(org), urlQueryEscape(state))
}

// App is what GitHub hands back once the manifest is converted. It carries
// credentials, so it must never be logged.
//
//nolint:recvcheck // String/GoString MUST take a value receiver: a pointer-receiver String is not consulted when a VALUE is formatted, so %v on an App would print the private key. Forget mutates and therefore needs a pointer. The mix is deliberate and the safety property is the reason.
type App struct {
	ID            int64  `json:"id"`
	Slug          string `json:"slug"`
	NodeID        string `json:"node_id"`
	Name          string `json:"name"`
	HTMLURL       string `json:"html_url"`
	PEM           string `json:"pem"`
	WebhookSecret string `json:"webhook_secret"`
	ClientID      string `json:"client_id"`
	ClientSecret  string `json:"client_secret"`

	Owner struct {
		Login string `json:"login"`
	} `json:"owner"`
}

// String redacts every credential this struct carries.
//
// App is exactly the kind of value that ends up in a debug print, a wrapped
// error or a log line during a bad afternoon, and a plain %v would otherwise
// emit the App's private key. Implementing String and GoString makes the default
// formatting verbs safe; code that deliberately wants a field still reads it.
func (a App) String() string {
	return fmt.Sprintf("github.App{ID:%d Slug:%q Owner:%q credentials:[redacted]}",
		a.ID, a.Slug, a.Owner.Login)
}

// GoString covers %#v, which does not consult String.
func (a App) GoString() string { return a.String() }

// Format makes EVERY verb safe, not just the ones fmt.Stringer covers.
//
// fmt consults Stringer only for %v, %s, %q, %x and %X. A verb it does not
// recognise for a struct — %d is the easy one to reach for — falls back to
// formatting the fields recursively, and prints the private key inside its own
// bad-verb diagnostic. Implementing fmt.Formatter takes precedence over
// Stringer for all verbs, so there is no verb left that renders the raw struct.
//
// This does NOT cover an App reached through an unexported field of another
// struct: fmt uses reflection there and cannot call methods on a value it may
// not interface. That limitation is intrinsic to fmt, so the claim here is
// "every direct formatting of an App is safe", not "an App can never be
// printed".
func (a App) Format(s fmt.State, verb rune) {
	// verb is deliberately ignored: the whole point is that NO verb renders the
	// struct's fields. fmt.State swallows write errors for every other
	// Formatter too, and there is nowhere to report one from here.
	//nolint:errcheck // fmt.State has no error channel; a failed write to it is the caller's output problem.
	io.WriteString(s, a.String())

	_ = verb
}

// MarshalJSON keeps credentials out of anything that serializes this struct.
//
// fmt.Formatter covers direct formatting and nothing else. billet standardizes
// on log/slog, and slog's JSON handler uses encoding/json — which reads the
// exported fields and their tags, emitting `pem`, `webhook_secret` and
// `client_secret` verbatim. `logger.Info("created", "app", app)` was therefore a
// full private-key disclosure into whatever the logs go to.
//
// Only MARSHALING is redirected. Decoding GitHub's conversion response uses the
// default unmarshaler and still populates every field, which is what the
// onboarding flow needs.
func (a App) MarshalJSON() ([]byte, error) {
	// A distinct type, not App, or this method recurses forever.
	type redacted struct {
		ID          int64  `json:"id"`
		Slug        string `json:"slug"`
		Name        string `json:"name"`
		HTMLURL     string `json:"html_url"`
		ClientID    string `json:"client_id"`
		Credentials string `json:"credentials"`
	}

	out, err := json.Marshal(redacted{
		ID:          a.ID,
		Slug:        a.Slug,
		Name:        a.Name,
		HTMLURL:     a.HTMLURL,
		ClientID:    a.ClientID,
		Credentials: "[redacted]",
	})
	if err != nil {
		return nil, fmt.Errorf("github: marshal redacted app: %w", err)
	}

	return out, nil
}

// LogValue is what slog asks for before falling back to reflection, so a
// text handler redacts for the same reason the JSON one does.
func (a App) LogValue() slog.Value {
	return slog.GroupValue(
		slog.Int64("id", a.ID),
		slog.String("slug", a.Slug),
		slog.String("credentials", "[redacted]"),
	)
}

// Forget blanks the credentials billet does not keep.
//
// Onboard returns this struct after the key is on disk, and only the App ID and
// installation ID are ever stored. The webhook secret and client secret have no
// consumer at all — billet registers an INACTIVE webhook and implements no OAuth
// flow — so carrying them further is holding secrets for no reason. The PEM goes
// with them: its one job is finished by the time this is called.
func (a *App) Forget() {
	a.PEM = ""
	a.WebhookSecret = ""
	a.ClientSecret = ""
}

// InstallURL is where the operator installs the freshly created app. Creating an
// app does NOT install it, and the installation is where the installation ID —
// which billet cannot work without — comes from.
func (a *App) InstallURL() string {
	if a.HTMLURL != "" {
		return a.HTMLURL + "/installations/new"
	}
	return fmt.Sprintf("%s/apps/%s/installations/new", webBase, a.Slug)
}

// ConvertManifest exchanges the temporary code for the app's credentials.
//
// The code is single-use and short-lived, so a failure here is terminal for that
// attempt: the operator has to re-run the flow rather than retry the exchange.
func ConvertManifest(ctx context.Context, client *http.Client, code string) (*App, error) {
	return convertManifestAt(ctx, client, apiBase, code)
}

// convertManifestAt is ConvertManifest with an injectable base URL, so the
// exchange can be tested against httptest without reaching GitHub.
//
// EVERY error leaving this function is redacted, not just the transport one.
// Redacting at individual call sites missed the response path entirely: a
// non-201 body is rendered by apiError, and a body that echoes the request route
// — which an intermediary proxy readily produces — carries the still-live code
// straight to the operator's terminal. Wrapping the whole boundary means a
// future return statement cannot forget.
func convertManifestAt(ctx context.Context, client *http.Client, base, code string) (*App, error) {
	app, err := convertManifest(ctx, client, base, code)
	if err != nil {
		return nil, redactCode(err, code)
	}

	return app, nil
}

func convertManifest(ctx context.Context, client *http.Client, base, code string) (*App, error) {
	if code == "" {
		return nil, fmt.Errorf("github: empty manifest code")
	}

	endpoint := fmt.Sprintf("%s/app-manifests/%s/conversions", base, urlPathEscape(code))

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("github: build conversion request: %w", err)
	}

	setAPIHeaders(req)

	resp, err := doWithTimeout(client, req)
	if err != nil {
		// Sanitized HERE, before any wrapping, rather than scrubbed at the
		// boundary. The URL carries the one-time manifest code, and every wrapper
		// added above this line renders the message of the error below it — so
		// cleaning the innermost one means no wrapper can carry the code, and no
		// later stage has to recognise the encoding it arrived in.
		return nil, fmt.Errorf("github: convert manifest: %w", sanitizeConversionChain(err))
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("github: read conversion response: %w", err)
	}

	if resp.StatusCode != http.StatusCreated {
		return nil, fmt.Errorf("github: convert manifest: %w", conversionError(resp.StatusCode, body))
	}

	var app App
	if err := json.Unmarshal(body, &app); err != nil {
		return nil, fmt.Errorf("github: decode conversion response: %w", err)
	}

	// Fail loudly on a response that parsed but is unusable — a missing key here
	// would otherwise surface much later as an inscrutable auth failure.
	if app.ID == 0 {
		return nil, fmt.Errorf("github: conversion response carried no app id")
	}

	if app.PEM == "" {
		return nil, fmt.Errorf("github: conversion response carried no private key")
	}

	return &app, nil
}

// redactedError hides a secret in its message while keeping the original error
// reachable.
//
// Replacing the wrapped error with errors.New destroyed the chain:
// errors.Is(err, context.DeadlineExceeded) and errors.As to *net.OpError both
// stopped working, so the caller could no longer classify a timeout — while a
// comment claimed the underlying error was still unwrappable. Error() is
// sanitized; Unwrap returns the real thing, so inspection works and only the
// rendered text is scrubbed.
type redactedError struct {
	msg string
	err error
}

func (e *redactedError) Error() string { return e.msg }
func (e *redactedError) Unwrap() error { return e.err }

// sanitizeConversionChain rebuilds an error chain so that NO error reachable
// from the result renders the one-time code — not merely the outermost one.
//
// Sanitizing only the outer message was a boundary that leaked through its own
// back door: Unwrap handed back the original, so
//
//	errors.As(err, &urlErr); log("cause", urlErr)
//
// printed the live URL, and so did any error reporter that walks causes and
// serializes them. The test that guarded this performed exactly that errors.As
// and never looked at what it extracted.
//
// Sentinel identity is preserved at the leaves, which is what errors.Is and
// errors.As need: only the levels that actually carry the code are replaced.
func sanitizeConversionChain(err error) error {
	if err == nil {
		return nil
	}

	// *url.Error is the carrier — net/http stores the whole request URL, and the
	// code is one of its path segments. REBUILT rather than pattern-matched: a
	// fixed path cannot be defeated by an encoding the matcher does not know
	// about (double-encoding and over-encoding both defeat matching), and the
	// scheme and host are the parts that carry the diagnostic value anyway.
	//nolint:errorlint // Deliberately the DIRECT type, not errors.As. This walks the chain one level at a time; errors.As searches it, so a nested *url.Error would be rebuilt at the wrong level and the wrappers between them dropped.
	if urlErr, ok := err.(*url.Error); ok {
		return &url.Error{
			Op:  urlErr.Op,
			URL: redactedConversionURL(urlErr.URL),
			Err: sanitizeConversionChain(urlErr.Err),
		}
	}

	// errors.Join produces a tree, not a chain, and errors.Unwrap returns nil for
	// it — so a walk that knew only the single-error form would stop at the join
	// and leave everything below it untouched. A RoundTripper is caller-supplied
	// and can return whatever it likes, including a joined error.
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		branches := joined.Unwrap()

		sanitizedBranches := make([]error, 0, len(branches))
		for _, branch := range branches {
			sanitizedBranches = append(sanitizedBranches, sanitizeConversionChain(branch))
		}

		return errors.Join(sanitizedBranches...)
	}

	inner := errors.Unwrap(err)

	sanitized := sanitizeConversionChain(inner)

	//nolint:errorlint // An identity comparison, not error matching: it asks whether the recursion produced a new object. errors.Is answers a different question entirely.
	if sanitized == inner {
		// Nothing below this level changed, so this level's rendering — which is
		// derived from it — cannot be carrying the code either. Returned as-is so
		// a sentinel keeps its identity.
		return err
	}

	// An arbitrary wrapper cannot be reconstructed (its format string is gone),
	// so its message is rebuilt from the parts that are known: the text it added,
	// and the sanitized error beneath it. errors.As to this wrapper's own
	// concrete type is lost; errors.Is and errors.As to anything below it survive.
	return &redactedError{
		msg: strings.Replace(err.Error(), inner.Error(), sanitized.Error(), 1),
		err: sanitized,
	}
}

// redactedConversionURL keeps a conversion URL's scheme and host and replaces
// everything that could hold the code.
func redactedConversionURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "[redacted conversion URL]"
	}

	return u.Scheme + "://" + u.Host + "/app-manifests/" + redactedCode + "/conversions"
}

// conversionError renders a failed conversion WITHOUT echoing the response body.
//
// This is the one endpoint in GitHub's whole API that returns the App's private
// key, and apiError prints either the JSON message or 200 raw bytes of whatever
// arrived. An intermediary that receives the credential-bearing 201 and forwards
// it under a rewritten status — a proxy turning an upstream hiccup into a 502 —
// would put the only copy of the private key on the operator's terminal while
// also reporting the conversion as failed.
//
// So nothing here is rendered verbatim. GitHub's own `message` is passed through
// because it is what explains an expired code, and only after it is checked for
// credential material; anything else is withheld and the status stands alone.
func conversionError(status int, body []byte) error {
	var rendered error

	var parsed struct {
		Message string `json:"message"`
	}

	if err := json.Unmarshal(body, &parsed); err == nil && parsed.Message != "" && !carriesCredential(parsed.Message) {
		rendered = fmt.Errorf("HTTP %d: %s", status, parsed.Message)
	} else {
		rendered = fmt.Errorf(
			"HTTP %d (response body withheld: this endpoint returns the App private key, "+
				"so its body is never rendered)", status)
	}

	// A code GitHub does not recognise is reported as 404, and one it will not
	// accept as 422. Marked so onboarding can tell "this particular code is
	// worthless" apart from "the exchange could not be attempted", which are
	// recovered from in completely different ways.
	if status == http.StatusNotFound || status == http.StatusUnprocessableEntity {
		return fmt.Errorf("%w: %w", errCodeRejected, rendered)
	}

	return rendered
}

// errCodeRejected marks a conversion that failed because GitHub refused the
// code itself, rather than because the request could not be completed.
var errCodeRejected = errors.New("github: the manifest code was rejected")

// carriesCredential reports whether a string looks like it contains credential
// material from a conversion response.
//
// Deliberately crude and deliberately over-eager: the cost of a false positive
// is a status code without GitHub's explanation, and the cost of a false
// negative is a private key on a terminal.
func carriesCredential(s string) bool {
	for _, marker := range []string{"PRIVATE KEY", "webhook_secret", "client_secret", `"pem"`} {
		if strings.Contains(s, marker) {
			return true
		}
	}

	return false
}

// redactCode removes the one-time manifest code from an error's message.
//
// The code redeems for the App's private key, webhook secret and client secret,
// and it is still LIVE when the exchange fails. net/http reports transport
// failures as *url.Error, whose Error() embeds the full URL; a non-201 response
// is rendered by apiError from a body that an intermediary may well echo the
// request route into. Either way it reaches the operator's terminal, where
// anyone reading a scrollback, a journal or a CI log could redeem it first.
//
// Scrubbing the rendered TEXT rather than walking the error tree is deliberate:
// http.Client.Do wraps whatever a RoundTripper returns, so a transport that
// itself produces a *url.Error hides another copy inside. Matching the value
// needs no assumption about how errors are composed.
func redactCode(err error, code string) error {
	msg := redactString(err.Error(), code)
	if msg == err.Error() {
		return err
	}

	// The chain is sanitized too, not just this level's text. Redacting only the
	// message left the original reachable through Unwrap, so anything that walked
	// causes — errors.As to *url.Error, an error reporter serializing a cause
	// chain — read the live code straight out of the error this call "redacted".
	return &redactedError{msg: msg, err: sanitizeConversionChain(err)}
}

const redactedCode = "[redacted: contains the one-time manifest code]"

// redactString removes a secret and its percent-encoded spellings.
func redactString(s, secret string) string {
	if secret == "" {
		return s
	}

	s = strings.ReplaceAll(s, secret, redactedCode)

	// Percent-encoding is case-insensitive in its hex digits, so a value escaped
	// elsewhere can arrive as %2f where url.PathEscape produces %2F. Matching
	// one spelling let the other through.
	//
	// Matched case-insensitively rather than by generating variants: uppercasing
	// the escaped string would also fold the literal characters, so a mixed-case
	// code produced a variant matching nothing. Folding over-redacts slightly —
	// a differently-cased copy of the code is also removed — which is the right
	// direction for a scrub.
	for _, escaped := range []string{url.PathEscape(secret), url.QueryEscape(secret)} {
		if escaped != secret {
			s = replaceFold(s, escaped, redactedCode)
		}
	}

	return s
}

// replaceFold is strings.ReplaceAll with case-insensitive matching.
func replaceFold(s, old, replacement string) string {
	if old == "" {
		return s
	}

	var b strings.Builder

	for i := 0; i < len(s); {
		if i+len(old) <= len(s) && strings.EqualFold(s[i:i+len(old)], old) {
			b.WriteString(replacement)

			i += len(old)

			continue
		}

		b.WriteByte(s[i])
		i++
	}

	return b.String()
}

func setAPIHeaders(req *http.Request) {
	req.Header.Set("Accept", acceptJSON)
	// Go canonicalizes header keys, so this goes on the wire as GitHub documents
	// it ("X-GitHub-Api-Version") despite the Go-canonical spelling here.
	req.Header.Set("X-Github-Api-Version", apiVersion)
	req.Header.Set("User-Agent", "billet")
}

func doWithTimeout(client *http.Client, req *http.Request) (*http.Response, error) {
	if client == nil {
		client = &http.Client{Timeout: requestTimout}
	}

	return client.Do(req) //nolint:wrapcheck // callers wrap with the operation name.
}

// apiError renders GitHub's error body, which usually explains the failure far
// better than the status code alone.
func apiError(status int, body []byte) error {
	var parsed struct {
		Message          string `json:"message"`
		DocumentationURL string `json:"documentation_url"`
	}

	if err := json.Unmarshal(body, &parsed); err == nil && parsed.Message != "" {
		return fmt.Errorf("HTTP %d: %s", status, parsed.Message)
	}

	trimmed := bytes.TrimSpace(body)
	if len(trimmed) > 200 {
		trimmed = trimmed[:200]
	}

	return fmt.Errorf("HTTP %d: %s", status, trimmed)
}
