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
	"reflect"
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

// organizationPermissions is the complete set billet requests for an
// organization target, and repositoryPermissions for a repository target.
//
// Unexported and copied on the way out: each is a security claim, and an
// exported map is one an importing package could rewrite at runtime — changing
// the manifest, the CLI's disclosure and the post-install validation together.
// Read ONLY through permissionsFor, so no code path can request both sets: a
// test parses this package and holds that to be so.
//
// Deliberately absent from both: `actions: read`. It would expose workflow runs,
// logs and artifacts, and billet needs none of them. Its absence is what makes
// "billet cannot read your code" true rather than reassuring.
//
// THE REPOSITORY SET IS WIDER, AND THAT IS A STATED TRADE. Registering a
// repository's runners needs the repository permission `administration: write`
// (measured against docs.github.com's endpoint table for "Create a registration
// token for a repository", 2026-09-04); there is no repository form of
// organization_self_hosted_runners. That permission also covers the repository's
// settings, collaborators and branch protection, none of which billet touches:
// it is accepted because it is the only permission GitHub offers for the job,
// and ADR-011 records what billet must never use it for.
var (
	organizationPermissions = map[string]string{
		"metadata":                         "read",
		"organization_self_hosted_runners": "write",
	}

	repositoryPermissions = map[string]string{
		"metadata":       "read",
		"administration": "write",
	}
)

// permissionsFor is the ONE reader of the two sets.
func permissionsFor(scope Scope) map[string]string {
	if scope == ScopeRepository {
		return repositoryPermissions
	}

	return organizationPermissions
}

// Permissions returns a copy of the permission set billet requests for a
// target of the given scope.
func Permissions(scope Scope) map[string]string {
	set := permissionsFor(scope)

	out := make(map[string]string, len(set))
	for k, v := range set {
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

// NewManifest builds billet's manifest for a target of the given scope.
// redirectURL and setupURL point at the loopback server the CLI runs for the
// duration of onboarding.
func NewManifest(name, redirectURL, setupURL string, scope Scope) Manifest {
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
		Permissions: Permissions(scope),
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

// RegistrationURL is where the browser POSTs the manifest form: an
// organization's App registration page, or a user's own.
//
// It must be a form POST, not a redirect: the manifest travels in the request
// body as a `manifest` field, which is why the CLI serves a self-submitting page
// rather than simply opening a URL.
//
// THE FORM FOLLOWS THE OWNER'S ACCOUNT TYPE, NOT THE TARGET'S SCOPE. A
// repository owned by an organization registers its App on the organization's
// page; a repository owned by a person registers it on that person's page,
// which is the one form with no owner in its path.
func RegistrationURL(owner string, ownerType OwnerType, state string) string {
	if ownerType == OwnerUser {
		return fmt.Sprintf("%s/settings/apps/new?state=%s", webBase, urlQueryEscape(state))
	}

	return fmt.Sprintf("%s/organizations/%s/settings/apps/new?state=%s",
		webBase, urlPathEscape(owner), urlQueryEscape(state))
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
// fmt consults Stringer only for %v, %s, %q, %x and %X. An unrecognised verb for a
// struct — %d is the easy one to reach for — falls back to formatting the fields
// recursively and prints the private key inside its own bad-verb diagnostic.
// fmt.Formatter takes precedence for all verbs, so no verb renders the raw struct.
//
// It does NOT cover an App reached through an unexported field of another struct:
// fmt uses reflection there and cannot call methods on a value it may not
// interface. The claim is "every direct formatting is safe", not "an App can never
// be printed".
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
// fmt.Formatter covers direct formatting and nothing else. slog's JSON handler uses
// encoding/json, which reads the exported fields and emits `pem`, `webhook_secret`
// and `client_secret` verbatim — so `logger.Info("created", "app", app)` was a full
// private-key disclosure into wherever the logs go.
//
// Only MARSHALING is redirected. Decoding GitHub's response still populates every
// field, which is what onboarding needs.
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
		return nil, redactCode(err, base, code)
	}

	return app, nil
}

func convertManifest(ctx context.Context, client *http.Client, base, code string) (*App, error) {
	if code == "" {
		return nil, fmt.Errorf("github: empty manifest code")
	}

	endpoint := conversionEndpoint(base, code)

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
		return nil, fmt.Errorf("github: convert manifest: %w", scrubberFor(base, code).sanitize(err, 0))
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

// conversionEndpoint is the URL the code is redeemed at. Built in one place so
// the scrubber can match the exact string a transport would have captured.
func conversionEndpoint(base, code string) string {
	return fmt.Sprintf("%s/app-manifests/%s/conversions", base, urlPathEscape(code))
}

// scrubber removes a conversion's secrets from error text.
//
// It carries the endpoint as well as the code because a caller-supplied
// RoundTripper composes its own messages: an opaque error whose Error() embeds
// r.URL.String() has no Unwrap and no structure to rebuild, so the only thing
// that can be done with it is to match text — and the ENDPOINT is an exact
// literal, immune to the encoding games that defeat matching the code alone.
type scrubber struct {
	code     string
	endpoint string
}

func scrubberFor(base, code string) scrubber {
	return scrubber{code: code, endpoint: conversionEndpoint(base, code)}
}

// clean removes both the endpoint and the bare code from a rendered message.
func (s scrubber) clean(text string) string {
	return redactString(redactString(text, s.endpoint), s.code)
}

// errorIdentity returns a comparable identity for err, and reports whether it
// has one.
//
// Only pointer-shaped dynamic types do. Comparing the interfaces directly would
// be simpler and can PANIC: an error struct holding a slice or a map is not
// comparable, and `a == b` on two such values is a runtime error rather than a
// false. Reflection answers the question without ever comparing the values.
func errorIdentity(err error) (uintptr, bool) {
	v := reflect.ValueOf(err)

	switch v.Kind() { //nolint:exhaustive // Only the reference kinds carry an identity; everything else is correctly "no identity".
	case reflect.Pointer, reflect.UnsafePointer, reflect.Chan, reflect.Map, reflect.Func:
		return v.Pointer(), true
	default:
		return 0, false
	}
}

// sameError reports whether two errors are the same object, without ever
// comparing two error values directly.
//
// `a == b` on error interfaces panics when the dynamic type is not comparable,
// so every place that wants object identity has to go through here.
func sameError(a, b error) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}

	aPtr, aOK := errorIdentity(a)
	bPtr, bOK := errorIdentity(b)

	// No identity to compare means the answer is "assume different". That is the
	// safe direction: the caller rebuilds a node it did not have to, which costs
	// a concrete type and never a credential.
	return aOK && bOK && aPtr == bPtr && reflect.TypeOf(a) == reflect.TypeOf(b)
}

// maxErrorDepth is the backstop for a chain that is merely absurd rather than
// cyclic. Cycles are caught by identity (see sanitize), which is what keeps a
// legitimately deep chain inspectable: a bare depth cut truncated the tail into
// an unwrappable node, so errors.Is against a sentinel below it stopped working
// even when nothing down there held a secret.
const maxErrorDepth = 512

// sanitize rebuilds an error chain so that NO error reachable from the result
// renders the one-time code — not merely the outermost one.
//
// Sanitizing only the outer message leaks through Unwrap: an errors.As for a
// *url.Error hands back the original with the live URL in it, and so does any error
// reporter that walks causes.
//
// Identity is preserved wherever it can be, because errors.Is and errors.As depend
// on it — but NOT at a node whose own text carries the secret. A leaf that renders
// the code is replaced even though that costs its type.
func (s scrubber) sanitize(err error, depth int) error {
	return s.sanitizeSeen(err, depth, nil)
}

// sanitizeSeen carries the ancestors of the current node so a cycle is cut at
// the point it closes rather than by running out of stack.
func (s scrubber) sanitizeSeen(err error, depth int, seen []uintptr) error {
	if err == nil {
		return nil
	}

	// Both cuts fail CLOSED: nothing below an unexplored node stays reachable, so it is
	// replaced by a scrubbed leaf rather than returned as-is.
	//
	// Cycles are tracked by POINTER, never by comparing error values. `ancestor == err`
	// on two interfaces panics when the dynamic type is not comparable — an error
	// struct containing a slice or a map — so the cycle guard could crash the process
	// it protects. Only pointer-shaped errors are tracked, which is no loss: a cycle
	// needs a node that can refer back to itself.
	if ptr, ok := errorIdentity(err); ok {
		for _, ancestor := range seen {
			if ancestor == ptr {
				return &redactedError{msg: s.clean(err.Error())}
			}
		}

		seen = append(seen, ptr)
	}

	if depth >= maxErrorDepth {
		return &redactedError{msg: s.clean(err.Error())}
	}

	// Rendered ONCE. Error() is not required to be deterministic, and calling it
	// twice let a stateful error return clean text to the comparison and dirty
	// text to whatever printed it afterwards.
	rendered := err.Error()

	// *url.Error is the structured carrier — net/http stores the whole request
	// URL, and the code is one of its path segments. REBUILT rather than
	// pattern-matched: a fixed path cannot be defeated by an encoding the matcher
	// does not know about, and the scheme and host are the parts that carry the
	// diagnostic value anyway.
	//nolint:errorlint // Deliberately the DIRECT type, not errors.As. This walks the chain one level at a time; errors.As searches it, so a nested *url.Error would be rebuilt at the wrong level and the wrappers between them dropped.
	if urlErr, ok := err.(*url.Error); ok {
		// EVERY string field is scrubbed, not just URL. Op is caller-controlled —
		// a transport composing url.Error{Op: r.URL.String()} put the live
		// endpoint in a field the rebuild copied verbatim — and the rebuilt URL
		// keeps the parsed scheme and host, which is another place a hostile or
		// merely odd URL can carry it.
		return &url.Error{
			Op:  s.clean(urlErr.Op),
			URL: s.clean(redactedConversionURL(urlErr.URL)),
			Err: s.sanitizeSeen(urlErr.Err, depth+1, seen),
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
			sanitizedBranches = append(sanitizedBranches, s.sanitizeSeen(branch, depth+1, seen))
		}

		return errors.Join(sanitizedBranches...)
	}

	inner := errors.Unwrap(err)

	// The child's renderings are captured ONCE each and reused below. Calling
	// inner.Error() again to test and then again to substitute gave a stateful
	// error three chances to show different text, which is exactly the hole the
	// single snapshot was meant to close.
	innerRendered := ""
	if inner != nil {
		innerRendered = inner.Error()
	}

	sanitized := s.sanitizeSeen(inner, depth+1, seen)

	sanitizedRendered := ""
	if sanitized != nil {
		sanitizedRendered = sanitized.Error()
	}

	cleaned := s.clean(rendered)

	// Same hazard as the cycle guard, and it took a panicking test to notice the
	// second instance: `sanitized == inner` compares two error INTERFACES, which
	// is a runtime panic when the dynamic type is not comparable. sameError does
	// it through identity where identity exists and answers "not the same"
	// otherwise, which is the conservative direction — it rebuilds a node that
	// did not need rebuilding rather than crashing.
	if sameError(sanitized, inner) && cleaned == rendered {
		// Neither this level's own text nor anything below it carries the secret,
		// so the error is returned untouched and keeps its identity.
		return err
	}

	// An arbitrary wrapper cannot be reconstructed (its format string is gone).
	// Where its rendering does contain the child's, the child's replacement is
	// substituted so the added context survives; where it does not — a wrapper
	// that TRANSFORMS rather than embeds — the scrubbed text is all that is left,
	// and the scrub is what keeps it safe.
	msg := cleaned
	if innerRendered != "" && strings.Contains(rendered, innerRendered) {
		msg = s.clean(strings.Replace(rendered, innerRendered, sanitizedRendered, 1))
	}

	return &redactedError{msg: msg, err: sanitized}
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
// This is the one endpoint in GitHub's API that returns the App's private key, and
// apiError prints either the JSON message or 200 raw bytes of whatever arrived. An
// intermediary that receives the credential-bearing 201 and forwards it under a
// rewritten status would put the only copy of the private key on the operator's
// terminal while reporting the conversion as failed.
func conversionError(status int, _ []byte) error {
	// NOTHING derived from the body, not even a filtered `message`.
	//
	// Filtering cannot work: a secret is an opaque string, and out of its field there
	// is nothing to recognise it by — {"message":"whsec-<the actual secret>"} carries
	// no marker at all.
	//
	// The asymmetry decides it. A false positive costs GitHub's explanation and keeps
	// the status; a false negative puts an unrepeatable private key on the operator's
	// terminal. Every other endpoint still uses apiError — this is the only one whose
	// 201 carries a credential.
	var rendered error

	switch status {
	case http.StatusNotFound:
		rendered = fmt.Errorf(
			"HTTP %d: GitHub does not recognise that registration. The one-time code is valid for %s "+
				"and can be redeemed once, so this usually means it expired or was already used",
			status, ManifestTTL)
	case http.StatusUnprocessableEntity:
		// Not treated as a rejected code — see codeRejectionStatuses — so say why
		// the operator might be seeing it, because the two causes need different
		// responses from them.
		rendered = fmt.Errorf(
			"HTTP %d: GitHub would not accept that registration. It documents this status as either a "+
				"validation failure or the endpoint having been spammed, so if nothing about the App is "+
				"unusual, wait a few minutes and run the command again",
			status)
	case http.StatusUnauthorized, http.StatusForbidden:
		rendered = fmt.Errorf("HTTP %d: GitHub refused the conversion request", status)
	default:
		rendered = fmt.Errorf(
			"HTTP %d (the response body is not shown: this endpoint returns the App private key, "+
				"so billet never renders it)", status)
	}

	// An explicit list, and it is neither {404, 422} nor "every 4xx".
	//
	// {404, 422} was too narrow: an injected code long enough to draw a 414, or
	// malformed enough for a proxy to answer 400, was read as "the exchange could
	// not be attempted" and aborted the flow, discarding an honest code queued
	// behind it. Widening to all of 4xx then went too far in the other direction
	// and swallowed 429 — which says nothing about the code, only that billet
	// asked too often. Discarding a VALID code on a rate limit is credential
	// loss: the App exists, its key is gone, and the loop waits out ManifestTTL
	// for a redirect that already arrived.
	//
	// So: only statuses that mean THIS CODE is unusable. Everything else — 429,
	// 408, 425, 426, 431, and all of 5xx — is fatal, because retrying with a
	// different code cannot help and no second redirect is coming.
	if codeRejectionStatuses[status] {
		return fmt.Errorf("%w: %w", errCodeRejected, rendered)
	}

	// An AMBIGUOUS status keeps the code and tries it again.
	//
	// Making 422 fatal is credential loss with extra steps: the code lives in a local
	// variable and the loopback server exits with the function, so "run the command
	// again" creates a SECOND App rather than recovering the first one's key.
	//
	// EVERYTHING that is not a definitive rejection is ambiguous. An enumerated list of
	// ambiguous statuses leaves every status in NEITHER set falling through as fatal,
	// which is how removing 414 from the rejection set preserved the failure it was
	// removed to fix.
	//
	// The default has to be "keep the code": retrying a status that really did mean the
	// code was bad costs a wait bounded by GitHub's own window, and discarding one that
	// did not costs a key that cannot be re-issued.
	return fmt.Errorf("%w: %w", errCodeAmbiguous, rendered)
}

// errCodeAmbiguous marks a response that did not establish anything about the
// code, so the same code is worth presenting again.
var errCodeAmbiguous = errors.New("github: the conversion did not resolve the code")

// codeRejectionStatuses are the responses that mean the presented code itself
// is unusable, so moving on to the next queued callback is safe.
//
// The list is short because the bar is high: discarding a code is irreversible,
// and discarding the HONEST one costs a key GitHub will not re-issue. Two wider
// versions shipped and both were wrong.
//
// GitHub documents exactly three outcomes for this endpoint — 201, 404 and 422 —
// and only 404 is unambiguous. **422 is documented as "Validation failed, or the
// endpoint has been spammed"**, so an attacker feeding forged codes can trip
// abuse protection and make the honest code's 422 look like a rejection. 400 is
// not code-specific either: a proxy returns it for header, policy and protocol
// reasons that have nothing to do with what billet sent.
//
// A forged code is a random string, so GitHub answers 404 and the flow moves on
// — which is the case that actually needed handling. 414 stays because the
// callback bounds code length before queueing, making it unreachable from a
// queued code and therefore harmless to list.
// 414 is deliberately NOT here. The callback bounds code length before
// queueing, so a 414 for a code billet accepted cannot be about the code — it is
// an intermediary's own limit, and discarding the honest code on it would orphan
// an App that already exists. "Unreachable, therefore harmless to list" had the
// reasoning backwards: if it is ever observed, that is evidence the response is
// NOT classifying this code.
var codeRejectionStatuses = map[int]bool{
	http.StatusNotFound: true, // GitHub does not know this code
}

// errCodeRejected marks a conversion that failed because GitHub refused the
// code itself, rather than because the request could not be completed.
var errCodeRejected = errors.New("github: the manifest code was rejected")

// redactCode is the boundary guard: it re-runs the sanitizer over whatever
// comes back from the conversion, whichever path produced it.
//
// The code redeems for the App's private key, webhook secret and client secret,
// and it is still LIVE when the exchange fails, so it must not reach a terminal
// scrollback, a journal, or a CI log where someone could redeem it first.
//
// The real work happens where each error is CREATED — a wrapper renders the
// message beneath it, so cleaning the innermost error means no wrapper can
// carry the secret and no later stage has to recognise the encoding it arrived
// in. This exists so that a future return path added inside convertManifest
// cannot bypass that by forgetting.
func redactCode(err error, base, code string) error {
	return scrubberFor(base, code).sanitize(err, 0)
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

// APIError is a non-2xx GitHub response, keeping the status so a caller can
// tell a verdict about the credential (401, 403) from GitHub being unable to
// answer (5xx) — a distinction `billet check` must not collapse.
type APIError struct {
	Status  int
	Message string
	// RateLimited marks a throttle: GitHub's primary limit answers 403 and its
	// secondary limit 429, and neither says anything about the credential —
	// reporting a throttled 403 as "GitHub refused the App credential" sends
	// the operator to their key file over a wait. Detected from the message,
	// because the body is what this type carries; the message shapes are
	// GitHub's documented rate-limit responses.
	RateLimited bool
}

func (e *APIError) Error() string { return fmt.Sprintf("HTTP %d: %s", e.Status, e.Message) }

// apiError renders GitHub's error body, which usually explains the failure far
// better than the status code alone.
func apiError(status int, body []byte) error {
	var parsed struct {
		Message          string `json:"message"`
		DocumentationURL string `json:"documentation_url"`
	}

	var message string
	if err := json.Unmarshal(body, &parsed); err == nil && parsed.Message != "" {
		message = parsed.Message
	} else {
		trimmed := bytes.TrimSpace(body)
		if len(trimmed) > 200 {
			trimmed = trimmed[:200]
		}
		message = string(trimmed)
	}

	lower := strings.ToLower(message)

	return &APIError{
		Status:  status,
		Message: message,
		RateLimited: status == http.StatusTooManyRequests ||
			strings.Contains(lower, "rate limit") ||
			strings.Contains(lower, "secondary rate") ||
			strings.Contains(lower, "abuse detection"),
	}
}
