package ec2

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// errNoCredentials is what every source reports when it has nothing, so a caller
// can tell "there are none" from "the lookup broke".
var errNoCredentials = errors.New("ec2: no aws credentials")

// Credentials are what SigV4 needs, and the secret half must never be rendered.
type Credentials struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
	// Expires is when these stop working. The zero time means never, which is
	// what a long-lived access key from the environment is.
	Expires time.Time
}

// REDACTED ON EVERY RENDERING PATH BILLET CAN REACH, the same way the GitHub App
// key is. A value receiver on String and GoString because a pointer receiver is
// not consulted when a value is formatted; Format so that no verb falls through
// to the raw struct; LogValue because billet standardizes on log/slog, whose JSON
// handler ignores fmt entirely; and MarshalJSON for anything that serializes.
//
// A secret access key is a durable credential for a whole AWS account. It reaches
// a log through one careless %v on a struct that happens to contain it, and
// nothing about that line looks wrong in review.
func (c Credentials) String() string { return c.redacted() }

// GoString covers %#v, which otherwise prints every field.
func (c Credentials) GoString() string { return c.redacted() }

// Format catches every verb, so %s, %v, %q and %d all render the redaction
// rather than falling back to the struct.
func (c Credentials) Format(f fmt.State, _ rune) {
	_, _ = io.WriteString(f, c.redacted()) //nolint:errcheck // fmt.State swallows write errors by design
}

// MarshalJSON keeps the secret out of anything that serializes a struct holding
// these.
func (c Credentials) MarshalJSON() ([]byte, error) {
	return json.Marshal(c.redacted())
}

// LogValue is what slog consults, and slog does not consult fmt.
func (c Credentials) LogValue() slog.Value { return slog.StringValue(c.redacted()) }

func (c Credentials) redacted() string {
	if c.AccessKeyID == "" {
		return "ec2.Credentials{none}"
	}

	// The access key ID is an identifier rather than a secret, and printing it is
	// the difference between "billet used the wrong role" and an unactionable
	// "authentication failed". The secret and the session token are never shown at
	// all, not even a prefix: a prefix of a secret is still bytes of a secret.
	return "ec2.Credentials{key=" + c.AccessKeyID + ", secret=REDACTED}"
}

// expired reports whether these credentials are past the point where billet
// should still be using them.
//
// THE MARGIN IS THE POINT. Credentials that expire during a request in flight
// fail it, and the request this signs may be the RunInstances that a job is
// waiting on, so they are replaced while there is still time for the call to
// complete.
func (c Credentials) expired(now time.Time) bool {
	if c.Expires.IsZero() {
		return false
	}

	return !now.Add(credentialRefreshMargin).Before(c.Expires)
}

const credentialRefreshMargin = 5 * time.Minute

// CredentialSource yields credentials, refreshing them when they expire.
type CredentialSource interface {
	Credentials(ctx context.Context) (Credentials, error)
}

// StaticCredentials is a fixed set, for a test or for credentials billet was
// handed directly.
//
// A DEFINED TYPE DOES NOT INHERIT THE METHODS OF THE TYPE IT IS DEFINED FROM, so
// every redaction above has to be restated here. It is not a formality: without
// them this type carried the same exported secret fields and none of the
// protection, and `%v` on one printed the secret access key in full — through a
// type whose whole purpose is to hold a credential. The test covering
// Credentials said nothing about it, which is why the test is now a table over
// every credential-carrying type in this package.
type StaticCredentials Credentials

// String redacts, as on Credentials. See the type's comment for why this is not
// inherited.
func (s StaticCredentials) String() string { return Credentials(s).redacted() }

// GoString covers %#v, which does not consult String.
func (s StaticCredentials) GoString() string { return Credentials(s).redacted() }

// Format catches every verb, so none falls back to the raw struct.
func (s StaticCredentials) Format(f fmt.State, _ rune) {
	_, _ = io.WriteString(f, Credentials(s).redacted()) //nolint:errcheck // fmt.State swallows write errors by design
}

// MarshalJSON keeps the secret out of anything that serializes a struct holding
// these.
func (s StaticCredentials) MarshalJSON() ([]byte, error) {
	return json.Marshal(Credentials(s).redacted())
}

// LogValue is what slog consults, and slog does not consult fmt.
func (s StaticCredentials) LogValue() slog.Value {
	return slog.StringValue(Credentials(s).redacted())
}

// Credentials satisfies CredentialSource.
func (s StaticCredentials) Credentials(context.Context) (Credentials, error) {
	// THE SAME DISTINCTION THE ENVIRONMENT SOURCE MAKES, because ChainCredentials
	// now relies on it: errNoCredentials means "nothing here, try the next one",
	// and anything else stops the chain. A half-filled source reporting an absence
	// would fall through to a different AWS identity, which is the failure that
	// distinction exists to prevent — so it has to hold for EVERY source rather
	// than the one where it was noticed.
	switch {
	case s.AccessKeyID == "" && s.SecretAccessKey == "" && s.SessionToken == "":
		return Credentials{}, errNoCredentials

	case s.AccessKeyID == "" || s.SecretAccessKey == "":
		return Credentials{}, errors.New("ec2: these credentials are missing an access key id " +
			"or a secret access key; a session token signs nothing on its own, and falling " +
			"through to another source would run billet as a different aws identity")
	}

	return Credentials(s), nil
}

// EnvCredentials reads the standard AWS environment variables.
type EnvCredentials struct{}

// Credentials satisfies CredentialSource.
//
// READ ON EVERY CALL rather than once at construction, so an operator who
// rewrites a service's environment and restarts nothing is not served a value
// from before. It is three map lookups.
func (EnvCredentials) Credentials(context.Context) (Credentials, error) {
	id := strings.TrimSpace(os.Getenv("AWS_ACCESS_KEY_ID"))
	secret := strings.TrimSpace(os.Getenv("AWS_SECRET_ACCESS_KEY"))

	// NOTHING SET IS AN ABSENCE; HALF SET IS A MISTAKE, and the difference decides
	// which AWS account billet acts as.
	//
	// Reporting an absence for a half-configured environment lets the chain fall
	// through to the instance role — so a typo in one variable silently switches
	// billet to a DIFFERENT and often more privileged identity, and it launches and
	// terminates machines as that one. An operator who set these had said which
	// identity they meant, so the answer is to stop and say which half is missing.
	// A SESSION TOKEN ON ITS OWN IS NOT AN ABSENCE EITHER. It is half a credential
	// like any other, and reading it as "nothing set" lets the chain move on to a
	// different identity than the one somebody configured.
	token := os.Getenv("AWS_SESSION_TOKEN")

	// PRESENCE IS NON-EMPTY, NOT NON-BLANK, for the token alone. It is opaque and
	// presented byte for byte, so billet has no basis for deciding that a value
	// made of spaces was not meant — and treating it as absent would let the chain
	// move on to a different identity, which is what all of this is about. The two
	// beside it are from restricted alphabets, so trimming them is a rescue rather
	// than a guess.
	switch {
	case id == "" && secret == "" && token == "":
		return Credentials{}, errNoCredentials

	case id == "" && secret == "":
		return Credentials{}, errors.New("ec2: AWS_SESSION_TOKEN is set and neither " +
			"AWS_ACCESS_KEY_ID nor AWS_SECRET_ACCESS_KEY is; a session token signs nothing on " +
			"its own, and billet will not fall back to this instance's role, which would run " +
			"it as a different aws identity than the one you configured")

	case id == "":
		return Credentials{}, errors.New("ec2: AWS_ACCESS_KEY_ID is not set while another " +
			"AWS_ credential variable is; refusing to fall back to this instance's role, which " +
			"would run billet as a different aws identity than the one you configured")

	case secret == "":
		return Credentials{}, errors.New("ec2: AWS_SECRET_ACCESS_KEY is not set while another " +
			"AWS_ credential variable is; refusing to fall back to this instance's role, which " +
			"would run billet as a different aws identity than the one you configured")
	}

	return Credentials{
		AccessKeyID:     id,
		SecretAccessKey: secret,
		// NOT TRIMMED. A session token is an opaque blob billet must present
		// byte-for-byte; trimming is safe for the two above because an access key
		// id and a secret are both from a restricted alphabet, and it is exactly
		// what rescues a value pasted into a unit file with a trailing space.
		SessionToken: token,
	}, nil
}

// defaultIMDSEndpoint is the link-local address every EC2 instance serves its
// own metadata on.
const defaultIMDSEndpoint = "http://169.254.169.254"

// IMDSCredentials reads the instance profile of the EC2 instance billet is
// running on.
//
// THE PREFERRED WAY TO RUN THIS BACKEND. The alternative is a long-lived access
// key in a unit file, which is a credential that does not expire sitting on disk
// on a host that launches machines. An instance profile rotates on its own and
// never exists outside the instance.
//
// IMDSv2 ONLY. v1 is an unauthenticated GET, so anything that can make the node
// issue a request to an attacker-chosen URL — an SSRF in any HTTP client in the
// process — reads the role's credentials out of it. v2 needs a PUT to obtain a
// token first, which a request-forgery primitive generally cannot perform.
type IMDSCredentials struct {
	// Endpoint overrides the link-local address, for tests.
	Endpoint string
	// HTTP is the client used, so a test can supply one and a caller can bound
	// the wait. Nil means a client with imdsTimeout.
	HTTP *http.Client

	// now is time.Now, replaceable so a test can age a cached value.
	now func() time.Time

	mu     sync.Mutex
	cached Credentials
}

// REDACTED LIKE THE CREDENTIAL IT HOLDS, and for a reason that is not obvious:
// `cached` is UNEXPORTED, and fmt cannot call methods on an unexported field. So
// without these, `%+v` on a fetched source printed the secret access key and the
// session token in full — through a struct whose whole job is holding one, and
// past a redaction that looked complete because the field's TYPE redacts.
//
// That is the "struct CONTAINING Credentials" hole the security skill names,
// found by a reviewer after the type-level redaction was already in place. A type
// that stores a credential needs its own methods; inheriting the field's is not a
// thing Go does.
func (i *IMDSCredentials) String() string { return i.redacted() }

// GoString covers %#v, which does not consult String.
func (i *IMDSCredentials) GoString() string { return i.redacted() }

// Format catches every verb, so none falls back to the raw struct.
func (i *IMDSCredentials) Format(f fmt.State, _ rune) {
	_, _ = io.WriteString(f, i.redacted()) //nolint:errcheck // fmt.State swallows write errors by design
}

// MarshalJSON keeps the cached secret out of anything that serializes this.
func (i *IMDSCredentials) MarshalJSON() ([]byte, error) { return json.Marshal(i.redacted()) }

// LogValue is what slog consults, and slog does not consult fmt.
func (i *IMDSCredentials) LogValue() slog.Value { return slog.StringValue(i.redacted()) }

// redacted TOUCHES NOTHING GUARDED BY THE MUTEX, deliberately.
//
// The obvious implementation reads `cached` under `i.mu` so it can name the key
// id it holds. That builds a deadlock and hands somebody the loaded gun: the
// whole reason this type has a String is so it can be printed, and
// `Credentials()` holds `i.mu` across the entire fetch — so the first
// `log.Warn("...", "source", i)` added inside that critical section wedges the
// node, at the moment credentials are being resolved. Reading the field without
// the lock instead is a data race.
//
// So it says nothing about the cached value at all. Which key is held is a
// question for the resolved Credentials, which redacts and shows its own key id;
// the SOURCE only has to be safe to print. Endpoint is written once at
// construction and never after.
func (i *IMDSCredentials) redacted() string {
	endpoint := i.Endpoint
	if endpoint == "" {
		endpoint = defaultIMDSEndpoint
	}

	return "ec2.IMDSCredentials{endpoint=" + endpoint + ", cached=REDACTED}"
}

// imdsTimeout bounds the metadata lookup.
//
// SHORT, because the common failure is that billet is NOT on EC2 at all: the
// link-local address is not routed, and the request hangs until something gives
// up. A node whose credentials are misconfigured should say so in a second
// rather than making the first job of the day look like a slow launch.
const imdsTimeout = 2 * time.Second

// Credentials satisfies CredentialSource, caching until shortly before expiry.
func (i *IMDSCredentials) Credentials(ctx context.Context) (Credentials, error) {
	i.mu.Lock()
	defer i.mu.Unlock()

	now := time.Now
	if i.now != nil {
		now = i.now
	}

	if i.cached.AccessKeyID != "" && !i.cached.expired(now()) {
		return i.cached, nil
	}

	creds, err := i.fetch(ctx)
	if err != nil {
		return Credentials{}, err
	}

	i.cached = creds

	return creds, nil
}

func (i *IMDSCredentials) fetch(ctx context.Context) (Credentials, error) {
	endpoint := i.Endpoint
	if endpoint == "" {
		endpoint = defaultIMDSEndpoint
	}

	client := i.HTTP
	if client == nil {
		client = &http.Client{Timeout: imdsTimeout}
	}

	token, err := imdsToken(ctx, client, endpoint)
	if err != nil {
		return Credentials{}, err
	}

	role, err := imdsGet(ctx, client, endpoint, "/latest/meta-data/iam/security-credentials/", token)
	if err != nil {
		return Credentials{}, fmt.Errorf("ec2: read the instance profile name from imds: %w", err)
	}

	// ONE ROLE PER INSTANCE, but the endpoint answers with a newline-separated
	// list and has always been documented that way. Taking the whole body as a
	// name produces a 404 on the next request with no hint as to why.
	name := strings.TrimSpace(role)
	if idx := strings.IndexByte(name, '\n'); idx >= 0 {
		name = strings.TrimSpace(name[:idx])
	}

	if name == "" {
		return Credentials{}, fmt.Errorf(
			"ec2: this instance has no instance profile attached, so imds has no credentials to "+
				"give; attach a role or set AWS_ACCESS_KEY_ID: %w", errNoCredentials)
	}

	body, err := imdsGet(ctx, client, endpoint,
		"/latest/meta-data/iam/security-credentials/"+name, token)
	if err != nil {
		return Credentials{}, fmt.Errorf("ec2: read credentials for instance profile %s: %w", name, err)
	}

	var payload struct {
		Code            string
		AccessKeyID     string `json:"AccessKeyId"`
		SecretAccessKey string
		Token           string
		Expiration      time.Time
	}

	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		// THE BODY IS NOT IN THE ERROR. It is the credential.
		return Credentials{}, fmt.Errorf(
			"ec2: imds returned something that is not a credential document for instance "+
				"profile %s: %w", name, err)
	}

	if payload.Code != "" && payload.Code != "Success" {
		return Credentials{}, fmt.Errorf("ec2: imds refused credentials for instance profile %s: %s",
			name, payload.Code)
	}

	// NOT errNoCredentials. A role that answered with half a credential is
	// MALFORMED, not absent, and wrapping the sentinel would let the chain carry
	// on to another source — running billet as an identity nobody chose, which is
	// exactly what the sentinel's meaning was tightened to prevent.
	// ALL FOUR FIELDS, because the two that were missing fail in ways that do not
	// look like a credential problem. A document with no Token cannot authenticate
	// at all — temporary credentials are only valid with one — and a document with
	// no Expiration is treated as never expiring, so it is cached for the life of
	// the process and every call starts failing the moment AWS rotates it.
	switch {
	case payload.AccessKeyID == "" || payload.SecretAccessKey == "":
		return Credentials{}, fmt.Errorf("ec2: imds returned an incomplete credential document "+
			"for instance profile %s: no access key", name)

	case payload.Token == "":
		return Credentials{}, fmt.Errorf("ec2: imds returned a credential document with no "+
			"session token for instance profile %s, which cannot authenticate anything", name)

	case payload.Expiration.IsZero():
		return Credentials{}, fmt.Errorf("ec2: imds returned a credential document with no "+
			"expiry for instance profile %s; billet would cache it for the life of the process "+
			"and start failing the moment aws rotates it", name)
	}

	return Credentials{
		AccessKeyID:     payload.AccessKeyID,
		SecretAccessKey: payload.SecretAccessKey,
		SessionToken:    payload.Token,
		Expires:         payload.Expiration,
	}, nil
}

// imdsToken performs the PUT that IMDSv2 requires before it will answer anything.
func imdsToken(ctx context.Context, client *http.Client, endpoint string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint+"/latest/api/token", http.NoBody)
	if err != nil {
		return "", fmt.Errorf("ec2: build an imds token request: %w", err)
	}

	req.Header.Set("X-Aws-Ec2-Metadata-Token-Ttl-Seconds", "21600")

	body, err := readIMDS(client, req)
	if err != nil {
		return "", fmt.Errorf("ec2: imds would not issue a session token (billet reads instance "+
			"credentials over IMDSv2 only): %w", err)
	}

	token := strings.TrimSpace(body)
	if token == "" {
		return "", errors.New("ec2: imds issued an empty session token")
	}

	return token, nil
}

func imdsGet(ctx context.Context, client *http.Client, endpoint, path, token string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+path, http.NoBody)
	if err != nil {
		return "", err
	}

	req.Header.Set("X-Aws-Ec2-Metadata-Token", token)

	return readIMDS(client, req)
}

// readIMDS issues a metadata request and returns its body.
//
// The body is BOUNDED. A credential document is under a kilobyte, and the reader
// on the other end is a link-local address billet has not authenticated — an
// unbounded read there is an unbounded allocation driven by whatever answers.
func readIMDS(client *http.Client, req *http.Request) (string, error) {
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}

	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return "", err
	}

	if resp.StatusCode != http.StatusOK {
		// THE BODY IS NOT RENDERED. On the credentials path it is the credential,
		// and one error format string shared by three call sites is how that
		// reaches a log.
		return "", fmt.Errorf("imds %s: http %d", req.URL.Path, resp.StatusCode)
	}

	return string(body), nil
}

// ChainCredentials tries each source in order and takes the first that has any.
//
// EXPLICIT BEFORE AMBIENT: the environment wins over the instance profile,
// because an operator who set AWS_ACCESS_KEY_ID on a machine that also has a role
// meant the key. The reverse order makes that setting silently do nothing.
type ChainCredentials []CredentialSource

// ChainCredentials renders as the TYPES of its sources. Printing the slice would
// print each element through its own methods, which is fine only while every
// source has them — and a CredentialSource can be implemented outside this
// package.
func (c ChainCredentials) String() string {
	// THE TYPES, NOT THE VALUES, and both halves of that matter.
	//
	// Formatting each source with %v recursed forever on a chain that contained
	// itself, and — worse — made this type's safety depend on every source
	// redacting, including one implemented outside this package. The comment here
	// used to claim the opposite: that stating it meant the chain did NOT depend on
	// that. It did.
	//
	// A type name says everything a reader needs from a chain (which sources, in
	// which order) and can carry nothing.
	parts := make([]string, 0, len(c))
	for _, src := range c {
		parts = append(parts, fmt.Sprintf("%T", src))
	}

	return "ec2.ChainCredentials[" + strings.Join(parts, " ") + "]"
}

// GoString covers %#v.
func (c ChainCredentials) GoString() string { return c.String() }

// Format catches every verb.
func (c ChainCredentials) Format(f fmt.State, _ rune) {
	_, _ = io.WriteString(f, c.String()) //nolint:errcheck // fmt.State swallows write errors by design
}

// MarshalJSON keeps a chain out of anything that serializes it structurally.
func (c ChainCredentials) MarshalJSON() ([]byte, error) { return json.Marshal(c.String()) }

// LogValue is what slog consults.
func (c ChainCredentials) LogValue() slog.Value { return slog.StringValue(c.String()) }

// Credentials satisfies CredentialSource.
func (c ChainCredentials) Credentials(ctx context.Context) (Credentials, error) {
	var errs []error

	for _, src := range c {
		creds, err := src.Credentials(ctx)
		if err == nil {
			return creds, nil
		}

		// A CHAIN CONTINUES PAST A SOURCE THAT HAS NOTHING, NOT PAST ONE THAT IS
		// WRONG. Anything other than "there are none here" is a source that was
		// configured and could not be used — a half-set environment, a metadata
		// service that answered with something that is not a credential — and
		// carrying on past it is how billet ends up acting as an identity nobody
		// chose.
		if !errors.Is(err, errNoCredentials) {
			// THE REASONS ARE CARRIED AS TEXT, NOT JOINED, and that distinction is
			// the whole fix rather than a stylistic one.
			//
			// errors.Join keeps every branch reachable by errors.Is — so a chain that
			// stopped at a terminal source still MATCHED errNoCredentials, because an
			// earlier source had reported an absence. ChainCredentials is itself a
			// CredentialSource and DefaultCredentials returns one, so an outer chain
			// read that as "nothing here" and carried on to another AWS identity:
			// exactly the fallthrough this was tightened to prevent, one level up.
			// THE TERMINAL ERROR IS WRAPPED; ONLY THE EARLIER ABSENCES BECOME TEXT.
			//
			// Flattening everything to a string closed the laundering and broke a
			// rule this project already wrote down: identity is preserved wherever it
			// can be, because errors.Is against context.Canceled and
			// context.DeadlineExceeded depends on it — and a cancelled credential
			// lookup is an ordinary way for this to fail. Only the absences need to
			// leave the unwrap graph, and this branch has already established the
			// terminal error is not one.
			if len(errs) == 0 {
				return Credentials{}, err
			}

			reasons := make([]string, 0, len(errs))
			for _, e := range errs {
				reasons = append(reasons, e.Error())
			}

			return Credentials{}, fmt.Errorf("%s; %w", strings.Join(reasons, "; "), err)
		}

		errs = append(errs, err)
	}

	if len(errs) == 0 {
		return Credentials{}, errNoCredentials
	}

	// EVERY SOURCE'S REASON, not just the last one. "No credentials" with one
	// cause attached sends an operator to whichever source happened to be tried
	// last, which on a machine with no role is a link-local timeout that says
	// nothing about the environment variable they forgot.
	return Credentials{}, errors.Join(errs...)
}

// DefaultCredentials is the chain an ordinary deployment wants.
func DefaultCredentials() CredentialSource {
	return ChainCredentials{EnvCredentials{}, &IMDSCredentials{}}
}
