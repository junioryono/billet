// Package awscreds resolves the AWS credentials billet signs requests with.
//
// IT LIVES HERE RATHER THAN IN A COMPUTE BACKEND, and that is the point. The chain
// used to live in internal/provider/ec2, and three unrelated packages depended on
// it: internal/archivestore, internal/store/ebss3 and internal/provider/codebuild
// each declared their OWN interface over awssig.Credentials rather than import a
// compute backend, and cmd/billet adapted ec2's chain into all of them with a
// conversion closure at every call site. Three parallel interfaces and four
// closures existed only because the chain was in the wrong place.
//
// THE CREDENTIAL TYPE IS awssig.Credentials, not one of this package's own. That
// is what removes the closure: every consumer already wanted the signer's type,
// and the old ec2.Credentials differed from it in exactly one field — an expiry
// that no caller outside the chain ever read. It is now internal to the source
// that has one, which is where it was always used.
package awscreds

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

	"github.com/junioryono/billet/internal/awssig"
)

// errNoCredentials is what every source reports when it has nothing, so a caller
// can tell "there are none" from "the lookup broke".
//
// UNEXPORTED, AND DELIBERATELY NOT awssig.ErrNoCredentials, which is a different
// statement. That one means a request cannot be signed; this one means one source
// in a chain has nothing and the next should be tried. Sharing a sentinel between
// them would make a signing failure indistinguishable from an absence to anything
// that branches on it — which is the exact fallthrough Chain was tightened to
// prevent, since carrying on past a source that is WRONG runs billet as an
// identity nobody chose.
var errNoCredentials = errors.New("awscreds: no aws credentials")

// credentialRefreshMargin is how long before expiry a cached credential stops
// being used.
//
// THE MARGIN IS THE POINT. Credentials that expire during a request in flight
// fail it, and the request this signs may be the RunInstances that a job is
// waiting on, so they are replaced while there is still time for the call to
// complete.
const credentialRefreshMargin = 5 * time.Minute

// expired reports whether a credential with this expiry is past the point where
// billet should still be using it. A zero expiry means never, which is what a
// long-lived access key from the environment is.
func expired(expires, now time.Time) bool {
	if expires.IsZero() {
		return false
	}

	return !now.Add(credentialRefreshMargin).Before(expires)
}

// EVERY REDACTION METHOD IS ASSERTED TO EXIST — AND, FOR TWO OF THE THREE TYPES, ON
// THE RECEIVER THE RULE REQUIRES.
//
// A BEHAVIOURAL TEST CANNOT DO THIS, and that is the whole reason these lines are
// here rather than another table row. The tests prove each method REDACTS when it
// is called; they cannot prove it EXISTS, because deleting one does not leak — it
// falls through to a sibling that still redacts. fmt drops from Format to String,
// %#v drops from GoString to String, slog drops from LogValue to MarshalJSON. So a
// method can disappear entirely with every assertion still green, and the next
// rendering path added to Go finds nothing there.
//
// THE RECEIVER IS HALF OF WHAT IS ASSERTED FOR Static AND Chain, and NONE of what is
// asserted for IMDS — a distinction worth stating, because the assertion looks the
// same in all three cases and is not.
//
// Static and Chain are checked as VALUES, and that DOES pin the receiver: a
// pointer-receiver method is not in the value's method set, so moving one fails the
// build. It is the mistake a test holding only a pointer cannot see.
//
// IMDS is checked as a POINTER, and a value-receiver method is also in the pointer's
// method set — so this proves the methods EXIST and says nothing about where they
// sit. What actually pins IMDS's receiver is `go vet`: the type holds a sync.Mutex,
// so a value receiver copies a lock and copylocks refuses it. The guarantee is real;
// it just is not this line, and claiming otherwise would leave somebody trusting the
// wrong instrument.
//
// awssig.Credentials is here because this package RETURNS it and its redaction table
// treats it as a credential-carrying type. Its own package may guard it too; a second
// assertion costs a line and removes the dependency on that.
var (
	_ fmt.Stringer   = awssig.Credentials{}
	_ fmt.Formatter  = awssig.Credentials{}
	_ json.Marshaler = awssig.Credentials{}
	_ slog.LogValuer = awssig.Credentials{}
	_ goStringer     = awssig.Credentials{}

	_ fmt.Stringer   = Static{}
	_ fmt.Formatter  = Static{}
	_ json.Marshaler = Static{}
	_ slog.LogValuer = Static{}
	_ goStringer     = Static{}

	_ fmt.Stringer   = Chain{}
	_ fmt.Formatter  = Chain{}
	_ json.Marshaler = Chain{}
	_ slog.LogValuer = Chain{}
	_ goStringer     = Chain{}

	_ fmt.Stringer   = (*IMDS)(nil)
	_ fmt.Formatter  = (*IMDS)(nil)
	_ json.Marshaler = (*IMDS)(nil)
	_ slog.LogValuer = (*IMDS)(nil)
	_ goStringer     = (*IMDS)(nil)
)

// goStringer is what %#v consults. The standard library has no name for it.
type goStringer interface{ GoString() string }

// absent is the ONE representation of "this source has nothing", and it is a type
// this package owns rather than a condition inferred from somebody's error graph.
//
// THREE ROUNDS OF INFERENCE EACH LEFT A WAY TO LIE, which is why it is a type now.
// errors.Is walks every branch of an errors.Join, so `errors.Join(absence, ctx.Err())`
// read as an absence; a hand-written walk fixed that and still classified a FOREIGN
// wrapper around the sentinel, and such a wrapper may implement `Is` and claim to be
// a cancellation while unwrapping to an absence. No walk over arbitrary errors can
// separate an honest wrap from a dishonest one.
//
// So absence is not inferred at all: it is this type, and nothing else. A NESTED
// CHAIN still works because Chain SEALS the aggregate it builds rather than because
// anything looks inside one — an unsealed join is terminal whatever its branches say.
// An error billet did not construct is terminal, which is the safe direction: being
// wrong that way stops the chain instead of letting it act as an AWS identity nobody
// chose. A caller holding the sentinel from an empty Chain can wrap it however they
// like and never manufacture an absence.
type absent struct{ error }

// Unwrap keeps errors.Is working for callers: an absence still matches
// errNoCredentials, and a cancellation carried underneath still matches
// context.Canceled. What it no longer does is decide anything here.
func (a absent) Unwrap() error { return a.error }

// noCredentials builds an absence.
func noCredentials(err error) error { return absent{err} }

// isAbsence reports whether an error means "this source has nothing", which is the
// one classification that lets Chain move on to another AWS identity.
//
// IT LOOKS AT THE TYPE AND NOTHING ELSE. Four rounds of increasingly careful graph
// inference each left a way to lie — errors.Is walking Join branches, a foreign
// wrapper around the sentinel, a wrapper implementing Is that claims to be a
// cancellation, and finally a foreign error implementing Unwrap() []error whose only
// branch is an absence. Every one of them ended the same way: billet acting as an AWS
// identity nobody chose.
//
// So there is no walk. Chain SEALS its own aggregate — when every source reported
// nothing it wraps the join in an absent — which is what lets this be a single type
// assertion and removes the cycle guard, the traversal budget and the branch
// recursion that existed only to make inference safe. An error billet did not
// construct is terminal, and that is the safe direction.
func isAbsence(err error) bool {
	_, ok := err.(absent) //nolint:errorlint // inferring through the graph is the defect

	return ok
}

// Source yields credentials, refreshing them when they expire.
type Source interface {
	Credentials(ctx context.Context) (awssig.Credentials, error)
}

// SourceFunc adapts a function into a Source.
//
// ONE ADAPTER, BESIDE THE INTERFACE. internal/store/ebss3 and internal/archivestore
// each declared their own before the chain moved here, which was two copies of
// three lines and is the same duplication the interface itself was.
//
// IT NEEDS NO REDACTION METHODS, and that is measured rather than assumed, because
// every other type in this file needs five and a closure can obviously CAPTURE a
// secret. It cannot leak one: fmt renders a func value as its address under every
// verb — `%v`, `%+v`, `%#v`, `%s`, and inside a struct — because the captured
// variables are not fields and reflect offers no way to reach them. Adding methods
// here would protect nothing and would suggest the opposite about the types that
// genuinely need them.
type SourceFunc func(context.Context) (awssig.Credentials, error)

// Credentials calls f.
func (f SourceFunc) Credentials(ctx context.Context) (awssig.Credentials, error) {
	return f(ctx)
}

// Static is a fixed set, for a test or for credentials billet was handed
// directly.
//
// A DEFINED TYPE DOES NOT INHERIT THE METHODS OF THE TYPE IT IS DEFINED FROM, so
// every redaction awssig.Credentials carries has to be restated here. It is not a
// formality: without them this type carried the same exported secret fields and
// none of the protection, and `%v` on one printed the secret access key in full —
// through a type whose whole purpose is to hold a credential. The test covering
// the credential type said nothing about it, which is why the redaction test is a
// table over every credential-carrying type in this package.
type Static awssig.Credentials

// redacted renders this credential showing the access key id and never the secret.
//
// THE ACCESS KEY ID IS SHOWN DELIBERATELY: it is an identifier rather than a
// secret, and printing it is the difference between "billet used the wrong role"
// and an unactionable "authentication failed". The secret and the session token
// are never shown at all, not even a prefix — a prefix of a secret is still bytes
// of a secret.
//
// IT NAMES THIS TYPE rather than delegating to awssig.Credentials.String(), which
// would be just as safe and would name the wrong one: a caller printing an
// awscreds.Static wants to be told that is what they have.
func (s Static) redacted() string {
	if s.AccessKeyID == "" {
		return "awscreds.Static{none}"
	}

	return "awscreds.Static{key=" + s.AccessKeyID + ", secret=REDACTED}"
}

// String redacts, as on awssig.Credentials. See the type's comment for why this
// is not inherited.
func (s Static) String() string { return s.redacted() }

// GoString covers %#v, which does not consult String.
func (s Static) GoString() string { return s.redacted() }

// Format catches every verb, so none falls back to the raw struct.
func (s Static) Format(f fmt.State, _ rune) {
	_, _ = io.WriteString(f, s.redacted()) //nolint:errcheck // fmt.State swallows write errors by design
}

// MarshalJSON keeps the secret out of anything that serializes a struct holding
// these.
func (s Static) MarshalJSON() ([]byte, error) {
	return json.Marshal(s.redacted())
}

// LogValue is what slog consults, and slog does not consult fmt.
func (s Static) LogValue() slog.Value {
	return slog.StringValue(s.redacted())
}

// Credentials satisfies Source.
func (s Static) Credentials(context.Context) (awssig.Credentials, error) {
	// THE SAME DISTINCTION THE ENVIRONMENT SOURCE MAKES, because Chain relies on
	// it: errNoCredentials means "nothing here, try the next one", and anything else
	// stops the chain. A half-filled source reporting an absence would fall through
	// to a different AWS identity, which is the failure that distinction exists to
	// prevent — so it has to hold for EVERY source rather than the one where it was
	// noticed.
	switch {
	case s.AccessKeyID == "" && s.SecretAccessKey == "" && s.SessionToken == "":
		return awssig.Credentials{}, noCredentials(errNoCredentials)

	case s.AccessKeyID == "" || s.SecretAccessKey == "":
		return awssig.Credentials{}, errors.New("awscreds: these credentials are missing an " +
			"access key id or a secret access key; a session token signs nothing on its own, and " +
			"falling through to another source would run billet as a different aws identity")
	}

	return awssig.Credentials(s), nil
}

// Env reads the standard AWS environment variables.
type Env struct{}

// Credentials satisfies Source.
//
// READ ON EVERY CALL rather than captured once at construction, which costs three
// map lookups and keeps this source from holding a credential of its own.
//
// IT IS NOT AN OPERATOR-FACING REFRESH, and the comment here used to say it was:
// nothing outside a process can change its environment, so an operator editing a
// unit file still has to restart the service. What re-reading actually buys is that
// EnvCredentials carries no cached secret between calls, and that a process which
// sets its own environment — a test, or a wrapper that resolves credentials before
// exec — is not served a value from before it did.
func (Env) Credentials(context.Context) (awssig.Credentials, error) {
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
		return awssig.Credentials{}, noCredentials(errNoCredentials)

	case id == "" && secret == "":
		return awssig.Credentials{}, errors.New("awscreds: AWS_SESSION_TOKEN is set and neither " +
			"AWS_ACCESS_KEY_ID nor AWS_SECRET_ACCESS_KEY is; a session token signs nothing on " +
			"its own, and billet will not fall back to this instance's role, which would run " +
			"it as a different aws identity than the one you configured")

	case id == "":
		return awssig.Credentials{}, errors.New("awscreds: AWS_ACCESS_KEY_ID is not set while " +
			"another AWS_ credential variable is; refusing to fall back to this instance's role, " +
			"which would run billet as a different aws identity than the one you configured")

	case secret == "":
		return awssig.Credentials{}, errors.New("awscreds: AWS_SECRET_ACCESS_KEY is not set while " +
			"another AWS_ credential variable is; refusing to fall back to this instance's role, " +
			"which would run billet as a different aws identity than the one you configured")
	}

	return awssig.Credentials{
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

// IMDS reads the instance profile of the EC2 instance billet is running on.
//
// THE PREFERRED WAY TO RUN AGAINST AWS. The alternative is a long-lived access
// key in a unit file, which is a credential that does not expire sitting on disk
// on a host that launches machines. An instance profile rotates on its own and
// never exists outside the instance.
//
// IMDSv2 ONLY. v1 is an unauthenticated GET, so anything that can make the node
// issue a request to an attacker-chosen URL — an SSRF in any HTTP client in the
// process — reads the role's credentials out of it. v2 needs a PUT to obtain a
// token first, which a request-forgery primitive generally cannot perform.
type IMDS struct {
	// Endpoint overrides the link-local address, for tests.
	Endpoint string
	// HTTP is the client used, so a test can supply one and a caller can bound
	// the wait. Nil means a client with imdsTimeout.
	HTTP *http.Client

	// now is time.Now, replaceable so a test can age a cached value.
	now func() time.Time

	mu sync.Mutex
	// cached and cachedExpires are one fact in two fields. THE EXPIRY LIVES HERE
	// rather than on the credential type, because it is the only thing that ever
	// read it: awssig.Credentials is what every caller wants and carries no expiry,
	// and the old shared type's Expires field was consulted by nothing outside this
	// chain.
	cached        awssig.Credentials
	cachedExpires time.Time
}

// REDACTED LIKE THE CREDENTIAL IT HOLDS, and for a reason that is not obvious:
// `cached` is UNEXPORTED, and fmt cannot call methods on an unexported field. So
// without these, `%+v` on a fetched source printed the secret access key and the
// session token in full — through a struct whose whole job is holding one, and
// past a redaction that looked complete because the field's TYPE redacts.
//
// That is the "struct CONTAINING a credential" hole the security skill names,
// found by a reviewer after the type-level redaction was already in place. A type
// that stores a credential needs its own methods; inheriting the field's is not a
// thing Go does.
func (i *IMDS) String() string { return i.redacted() }

// GoString covers %#v, which does not consult String.
func (i *IMDS) GoString() string { return i.redacted() }

// Format catches every verb, so none falls back to the raw struct.
func (i *IMDS) Format(f fmt.State, _ rune) {
	_, _ = io.WriteString(f, i.redacted()) //nolint:errcheck // fmt.State swallows write errors by design
}

// MarshalJSON keeps the cached secret out of anything that serializes this.
func (i *IMDS) MarshalJSON() ([]byte, error) { return json.Marshal(i.redacted()) }

// LogValue is what slog consults, and slog does not consult fmt.
func (i *IMDS) LogValue() slog.Value { return slog.StringValue(i.redacted()) }

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
// question for the resolved credential, which redacts and shows its own key id;
// the SOURCE only has to be safe to print.
//
// THE ENDPOINT IS SHOWN, and that is the same judgement the access key id gets: it
// is an identifier rather than a secret, it is a value the OPERATOR configured
// rather than one a peer supplied, and printing it is the difference between "billet
// asked the wrong address" and an unactionable "credentials failed". It is written
// once at construction and never after.
func (i *IMDS) redacted() string {
	endpoint := i.Endpoint
	if endpoint == "" {
		endpoint = defaultIMDSEndpoint
	}

	return "awscreds.IMDS{endpoint=" + endpoint + ", cached=REDACTED}"
}

// imdsTimeout bounds the metadata lookup.
//
// SHORT, because the common failure is that billet is NOT on EC2 at all: the
// link-local address is not routed, and the request hangs until something gives
// up. A node whose credentials are misconfigured should say so in a second
// rather than making the first job of the day look like a slow launch.
const imdsTimeout = 2 * time.Second

// Credentials satisfies Source, caching until shortly before expiry.
func (i *IMDS) Credentials(ctx context.Context) (awssig.Credentials, error) {
	i.mu.Lock()
	defer i.mu.Unlock()

	now := time.Now
	if i.now != nil {
		now = i.now
	}

	if i.cached.AccessKeyID != "" && !expired(i.cachedExpires, now()) {
		return i.cached, nil
	}

	creds, expires, err := i.fetch(ctx)
	if err != nil {
		return awssig.Credentials{}, err
	}

	i.cached, i.cachedExpires = creds, expires

	return creds, nil
}

func (i *IMDS) fetch(ctx context.Context) (awssig.Credentials, time.Time, error) {
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
		return awssig.Credentials{}, time.Time{}, err
	}

	role, err := imdsGet(ctx, client, endpoint, "/latest/meta-data/iam/security-credentials/", token)

	// A 404 ON THE LISTING IS "NO ROLE ATTACHED", WHICH IS AN ABSENCE.
	//
	// MEASURED BEHAVIOUR OF THE SERVICE, not of the fake: real IMDS answers 404 for
	// the whole iam/security-credentials category on an instance with no profile — it
	// does not answer 200 with an empty body, which is the only shape the code below
	// treated as an absence. So the ordinary no-role instance produced a TERMINAL
	// error, which stops a chain that should have fallen through, and told an operator
	// "http 404" for a path they have never heard of instead of naming the two ways to
	// fix it. The fake was answering 200-and-a-newline, so nothing here could see it.
	//
	// ONLY THE LISTING. A 404 on the token PUT or on the credential document is not an
	// absence — the first means IMDSv2 is off and the second means the profile named
	// by the listing is not there, and both are things an operator has to know about
	// rather than fall past.
	if status, ok := errors.AsType[statusError](err); ok && status.code == http.StatusNotFound {
		return awssig.Credentials{}, time.Time{}, noCredentials(fmt.Errorf(
			"awscreds: this instance has no instance profile attached, so imds has no "+
				"credentials to give; attach a role or set AWS_ACCESS_KEY_ID: %w", errNoCredentials))
	}

	if err != nil {
		return awssig.Credentials{}, time.Time{},
			fmt.Errorf("awscreds: read the instance profile name from imds: %w", err)
	}

	// ONE ROLE PER INSTANCE, but the endpoint answers with a newline-separated
	// list and has always been documented that way. Taking the whole body as a
	// name produces a 404 on the next request with no hint as to why.
	name := strings.TrimSpace(role)
	if idx := strings.IndexByte(name, '\n'); idx >= 0 {
		name = strings.TrimSpace(name[:idx])
	}

	if name == "" {
		return awssig.Credentials{}, time.Time{}, noCredentials(fmt.Errorf(
			"awscreds: this instance has no instance profile attached, so imds has no credentials "+
				"to give; attach a role or set AWS_ACCESS_KEY_ID: %w", errNoCredentials))
	}

	// THE NAME CAME OUT OF A RESPONSE BODY AND IS ABOUT TO BE INTERPOLATED INTO A URL
	// PATH, so it is checked against what an IAM role name can be before either.
	//
	// THAT IS NEEDED FOR THE REQUEST, NOT ONLY FOR THE LOG. Whatever answers on the
	// link-local address chooses these bytes, and billet was putting them straight
	// into the path of its next call — a `/`, a `?` or a `%` there addresses something
	// other than the profile billet thinks it is asking about. It is the same rule
	// this repository already applies to github.org: a string that leaves the process
	// is pinned at the boundary it crosses.
	//
	// AND IT IS WHY THE NAME MAY STILL APPEAR IN AN ERROR, which the status code may
	// not. The distinction is worth stating because the two look alike: the credential
	// DOCUMENT is a credential, so nothing derived from it is rendered; this listing
	// is not one, and billet has to use this value in a URL whether or not it prints
	// it — so the question is what the value may BE, and the answer is now 64
	// characters from AWS's documented set for a role name.
	if !isRoleName(name) {
		return awssig.Credentials{}, time.Time{}, errors.New(
			"awscreds: imds answered the instance-profile listing with something that is not a " +
				"role name; billet will not put it in a request path, and does not render it")
	}

	body, err := imdsGet(ctx, client, endpoint,
		"/latest/meta-data/iam/security-credentials/"+name, token)
	if err != nil {
		return awssig.Credentials{}, time.Time{},
			fmt.Errorf("awscreds: read credentials for instance profile %s: %w", name, err)
	}

	var payload struct {
		Code            string
		AccessKeyID     string `json:"AccessKeyId"`
		SecretAccessKey string
		Token           string
		Expiration      time.Time
	}

	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		// THE BODY IS NOT IN THE ERROR, AND THE DECODER'S MESSAGE IS PART OF THE BODY.
		// This used to wrap err, which reads as ordinary diagnostics and is not:
		// encoding/json quotes the offending input, so a document whose Expiration is
		// malformed puts those bytes in the error — and a document is a credential
		// whether or not it parsed. The failure is reported without it, which is the
		// same judgement billet makes about the App-key conversion response.
		return awssig.Credentials{}, time.Time{}, fmt.Errorf(
			"awscreds: imds returned something that is not a credential document for instance "+
				"profile %s", name)
	}

	if payload.Code != "" && payload.Code != "Success" {
		// THE CODE IS NOT RENDERED, AND VALIDATING ITS SHAPE WAS NOT GOOD ENOUGH.
		//
		// It is the one field here worth showing an operator — "AssumeRoleUnauthorized
		// Access" is the whole diagnosis — so the previous version echoed it when it
		// looked like a status: short and alphabetic. That is the filtering mistake
		// billet already wrote down for the App-key conversion response. A secret out
		// of its field is an opaque string and carries no marker to catch:
		// `"Code":"SECRETTHATMUSTNOTAPPEAR"` passes every shape test there is, and this
		// document came from whatever holds the link-local address rather than from
		// AWS by definition.
		//
		// So billet says what IT knows — the profile it asked for, and that the answer
		// was not a success — and nothing derived from the body. False positives cost
		// an operator AWS's explanation; false negatives cost the credential, and that
		// asymmetry decides it here exactly as it does there.
		return awssig.Credentials{}, time.Time{},
			fmt.Errorf("awscreds: imds did not return credentials for instance profile %s; "+
				"its status code is not rendered because the document it came in IS the "+
				"credential", name)
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
	// THE ACCESS KEY ID IS THE ONE CREDENTIAL FIELD BILLET DELIBERATELY RENDERS, so it
	// is the one that has to be shaped before it can be.
	//
	// Every redaction in this package shows it on purpose — it is an identifier, and
	// printing it is the difference between "billet used the wrong role" and an
	// unactionable "authentication failed". But it arrives in the credential document,
	// which is chosen by whatever answers on the link-local address: an unbounded
	// string there is response bytes appearing inside a rendering the whole file calls
	// redacted. Checking it closes the last way the document reaches a log, and it is
	// a correctness check besides — a malformed key id fails signing later, in an
	// error naming neither the key nor the document it came from.
	//
	// AWS's key ids are uppercase alphanumeric, 16 to 128 characters.
	//
	// THIS NARROWS THE CHANNEL AND DOES NOT CLOSE IT, which is worth saying because
	// the checks above it DO close theirs. A hostile responder can still choose 16 to
	// 128 uppercase alphanumeric characters and have billet render them, because that
	// is what an access key id looks like and printing it is the entire reason the
	// field is shown. No shape check can do better while the value stays visible, and
	// making it invisible costs the diagnostic that distinguishes "billet used the
	// wrong role" from "authentication failed". Recorded rather than claimed away.
	switch {
	case payload.AccessKeyID != "" && !isAccessKeyID(payload.AccessKeyID):
		return awssig.Credentials{}, time.Time{},
			fmt.Errorf("awscreds: imds returned a credential document whose access key id is "+
				"not one, for instance profile %s; it is not rendered because the document it "+
				"came in IS the credential", name)

	case payload.AccessKeyID == "" || payload.SecretAccessKey == "":
		return awssig.Credentials{}, time.Time{},
			fmt.Errorf("awscreds: imds returned an incomplete credential document "+
				"for instance profile %s: no access key", name)

	case payload.Token == "":
		return awssig.Credentials{}, time.Time{},
			fmt.Errorf("awscreds: imds returned a credential document with no "+
				"session token for instance profile %s, which cannot authenticate anything", name)

	case payload.Expiration.IsZero():
		return awssig.Credentials{}, time.Time{},
			fmt.Errorf("awscreds: imds returned a credential document with no "+
				"expiry for instance profile %s; billet would cache it for the life of the process "+
				"and start failing the moment aws rotates it", name)
	}

	return awssig.Credentials{
		AccessKeyID:     payload.AccessKeyID,
		SecretAccessKey: payload.SecretAccessKey,
		SessionToken:    payload.Token,
	}, payload.Expiration, nil
}

// isAccessKeyID reports whether a string is shaped like an AWS access key id.
//
// UPPERCASE ALPHANUMERIC, 16 TO 128, which is AWS's documented shape and is pinned to
// the documentation rather than to a measurement — billet cannot ask a real IMDS from
// a unit test. What it buys is not that the value is a key, which only AWS knows, but
// that a value billet is about to PRINT in every redacted rendering is bounded and
// carries nothing a peer chose beyond those characters.
func isAccessKeyID(s string) bool {
	const minKeyID, maxKeyID = 16, 128

	if len(s) < minKeyID || len(s) > maxKeyID {
		return false
	}

	for _, r := range s {
		if (r < 'A' || r > 'Z') && (r < '0' || r > '9') {
			return false
		}
	}

	return true
}

// isRoleName reports whether a string is shaped like an IAM role name.
//
// AWS DOCUMENTS THE SET as [\w+=,.@-] with a maximum of 64 characters, and this is
// pinned to the documentation rather than to a measurement, which is worth saying
// plainly: billet cannot ask a real IMDS from a unit test. What it buys is not that
// the value is a role — only AWS knows that — but that it is safe to place in a URL
// path and bounded if it is ever rendered.
func isRoleName(s string) bool {
	const maxRoleName = 64

	if s == "" || len(s) > maxRoleName {
		return false
	}

	// "." AND ".." ARE THE SPECIAL SEGMENTS, and only those two. They pass the
	// character set and re-address the URL this value is concatenated into, which is
	// the one thing the set alone does not settle. `...` is neither special nor
	// invalid, and refusing it would refuse a name AWS accepts — a guard that rejects
	// correct input being the failure ADR-005 names.
	if s == "." || s == ".." {
		return false
	}

	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '_' || r == '+' || r == '=' || r == ',' || r == '.' || r == '@' || r == '-':
		default:
			return false
		}
	}

	return true
}

// imdsToken performs the PUT that IMDSv2 requires before it will answer anything.
func imdsToken(ctx context.Context, client *http.Client, endpoint string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint+"/latest/api/token", http.NoBody)
	if err != nil {
		return "", fmt.Errorf("awscreds: build an imds token request: %w", err)
	}

	req.Header.Set("X-Aws-Ec2-Metadata-Token-Ttl-Seconds", "21600")

	body, err := readIMDS(client, req)
	if err != nil {
		return "", fmt.Errorf("awscreds: imds would not issue a session token (billet reads "+
			"instance credentials over IMDSv2 only): %w", err)
	}

	token := strings.TrimSpace(body)
	if token == "" {
		return "", errors.New("awscreds: imds issued an empty session token")
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
//
// WHAT IS DELIBERATELY NOT SANITISED HERE, AND WHY. A transport error from client.Do
// is returned as it is and reaches the caller's error through %w. Go builds that
// message, but a peer CAN influence one: a malformed status line puts bytes it chose
// into "malformed HTTP response ...". So this does not fully satisfy the rule one
// function up, which is that nothing derived from a metadata response reaches an
// error. The body READ error beside it IS replaced — see there for why the two are
// treated differently.
//
// It is left that way on purpose, and the reasoning is the trade rather than an
// oversight. Sanitising means replacing the text with billet's own — the shape the
// App-key onboarding uses — and the text is the diagnosis for the failure that
// actually happens: billet is NOT on EC2, the link-local address is not routed, and
// the request times out. Replacing "dial tcp 169.254.169.254:80: connect: host is
// down" with "the metadata request failed" costs every operator the answer, on the
// common path, to buy nothing against an attacker who by construction already
// controls the credential source — if they hold that address, they serve billet
// their own credentials rather than smuggling bytes into its log.
//
// The bytes that ARE the credential are covered separately: the document is never
// rendered, its status code is never rendered, the decoder's message is never
// rendered, and the profile name is validated before it can reach either a URL or an
// error. The access key id is the deliberate exception — it is SHOWN, because that is
// what makes a rendering worth reading — and it is bounded to AWS's shape rather than
// hidden, which narrows what a responder can put there without closing it. This residual is the transport layer beneath that, and
// it is recorded rather than claimed away — the same way argv is for the App key.
func readIMDS(client *http.Client, req *http.Request) (string, error) {
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}

	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		// SANITISED, UNLIKE THE DIAL ERROR ABOVE IT, and the difference is what each
		// one costs. A body-read failure's text can carry bytes a peer chose — a
		// malformed chunk trailer lands in Go's message — and it is NOT the diagnosis
		// for the common failure, which is the dial. So this one is replaced with
		// billet's text while its identity survives for errors.Is.
		return "", readError{inner: err}
	}

	// A RESPONSE THAT CAME FROM SOMEWHERE ELSE IS NOT AN ANSWER ABOUT THIS RESOURCE.
	//
	// http.Client.Do FOLLOWS REDIRECTS, and the status is then about wherever it ended
	// up. That matters because one status is load-bearing: a 404 on the
	// instance-profile listing means "no role attached", which is an ABSENCE and lets
	// the chain fall through to another AWS identity. A listing that redirected and
	// ultimately 404'd would be read the same way, so billet would report "no instance
	// profile attached" about a resource it never reached.
	//
	// The final URL is COMPARED rather than redirects refused outright, because the
	// caller supplies the client and may have its own policy; this holds whatever that
	// policy is.
	// THE WHOLE URL, NOT ITS PATH. Comparing paths alone let a redirect to a
	// different HOST — or a different scheme, port or query — with the same path
	// through, and the 404 that came back from it would still have been read as "no
	// role attached".
	if resp.Request != nil && resp.Request.URL != nil &&
		resp.Request.URL.String() != req.URL.String() {
		return "", fmt.Errorf("awscreds: the imds request for %s was answered from somewhere "+
			"else, so billet cannot say what the answer is about", req.URL.Path)
	}

	if resp.StatusCode != http.StatusOK {
		// THE BODY IS NOT RENDERED. On the credentials path it is the credential,
		// and one error format string shared by three call sites is how that
		// reaches a log.
		return "", statusError{path: req.URL.Path, code: resp.StatusCode}
	}

	return string(body), nil
}

// statusError is a non-200 from the metadata service, carrying the status so a
// caller can tell one refusal from another without parsing a message.
type statusError struct {
	path string
	code int
}

func (e statusError) Error() string { return fmt.Sprintf("imds %s: http %d", e.path, e.code) }

// readError is a failure reading a metadata response body, with billet's text.
type readError struct{ inner error }

func (readError) Error() string { return "awscreds: could not read the imds response" }

// Unwrap keeps cancellation and deadline identity reachable for a caller while the
// message stays billet's.
func (e readError) Unwrap() error { return e.inner }

// Chain tries each source in order and takes the first that has any.
//
// EXPLICIT BEFORE AMBIENT: the environment wins over the instance profile,
// because an operator who set AWS_ACCESS_KEY_ID on a machine that also has a role
// meant the key. The reverse order makes that setting silently do nothing.
type Chain []Source

// Chain renders as the TYPES of its sources. Printing the slice would print each
// element through its own methods, which is fine only while every source has
// them — and a Source can be implemented outside this package.
func (c Chain) String() string {
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

	return "awscreds.Chain[" + strings.Join(parts, " ") + "]"
}

// GoString covers %#v.
func (c Chain) GoString() string { return c.String() }

// Format catches every verb.
func (c Chain) Format(f fmt.State, _ rune) {
	_, _ = io.WriteString(f, c.String()) //nolint:errcheck // fmt.State swallows write errors by design
}

// MarshalJSON keeps a chain out of anything that serializes it structurally.
func (c Chain) MarshalJSON() ([]byte, error) { return json.Marshal(c.String()) }

// LogValue is what slog consults.
func (c Chain) LogValue() slog.Value { return slog.StringValue(c.String()) }

// Credentials satisfies Source.
func (c Chain) Credentials(ctx context.Context) (awssig.Credentials, error) {
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
		if !isAbsence(err) {
			// THE REASONS ARE CARRIED AS TEXT, NOT JOINED, and that distinction is
			// the whole fix rather than a stylistic one.
			//
			// errors.Join keeps every branch reachable by errors.Is — so a chain that
			// stopped at a terminal source still MATCHED errNoCredentials, because an
			// earlier source had reported an absence. Chain is itself a Source and
			// Default returns one, so an outer chain read that as "nothing here" and
			// carried on to another AWS identity: exactly the fallthrough this was
			// tightened to prevent, one level up.
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
				return awssig.Credentials{}, err
			}

			reasons := make([]string, 0, len(errs))
			for _, e := range errs {
				reasons = append(reasons, e.Error())
			}

			return awssig.Credentials{}, fmt.Errorf("%s; %w", strings.Join(reasons, "; "), err)
		}

		errs = append(errs, err)
	}

	if len(errs) == 0 {
		return awssig.Credentials{}, noCredentials(errNoCredentials)
	}

	// EVERY SOURCE'S REASON, not just the last one. "No credentials" with one
	// cause attached sends an operator to whichever source happened to be tried
	// last, which on a machine with no role is a link-local timeout that says
	// nothing about the environment variable they forgot.
	//
	// SEALED, so an OUTER chain recognises it without looking inside. That is what
	// lets isAbsence be a type assertion: Chain is the only thing that ever needs to
	// aggregate absences, so it is the only thing that needs to say "this aggregate is
	// one" — and it can, because it built it. errors.Is still reaches every branch for
	// a caller.
	return awssig.Credentials{}, noCredentials(errors.Join(errs...))
}

// Default is the chain an ordinary deployment wants.
func Default() Source {
	return Chain{Env{}, &IMDS{}}
}
