// Package awsssm reads and writes AWS Systems Manager Parameter Store values.
//
// WHAT IT IS FOR is the deployment identity material two controllers have to
// share: the node-wire certificate authority and the GitHub App private key. The
// codebuild backend already wrote single-use runner registrations here, so the
// service, the endpoint derivation, the signing vectors and the SecureString
// rules are all measured behaviour billet already has — what is new is the READ.
//
// THE RESPONSE CARRIES THE SECRET, WHICH THE WRITE PATH NEVER DID. Every rule the
// codebuild JIT channel states is about keeping a request out of a log; here the
// value comes BACK, so nothing derived from a GetParameter response is ever
// rendered, and the error paths say what failed without saying what was in it.
//
// IT IS NOT REACHABLE FROM THE LEDGER PACKAGES, and that is enforced rather than
// intended: depguard's `ledgerwriters` rule bans net/http from internal/state,
// internal/alloc and internal/rollout, because DB.Tx holds the single writer slot
// from BEGIN and a remote call inside one stalls every scheduling write in the
// process. Nothing here may be called from inside a transaction.
package awsssm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/junioryono/billet/internal/awscreds"
	"github.com/junioryono/billet/internal/awsjson"
)

// The service's own names. targetPrefix is the API version; a request whose
// X-Amz-Target does not match one AWS knows is refused with nothing useful.
const (
	service      = "ssm"
	targetPrefix = "AmazonSSM."
)

// ErrNotFound means Parameter Store has no value under that name.
//
// ITS OWN ERROR BECAUSE ABSENCE IS AN ORDINARY STATE FOR SOME CALLERS AND A FAULT
// FOR OTHERS. A deployment that has never published its authority has none here,
// which is day one; a controller that expects one and finds nothing has lost it.
// Only the caller can tell those apart, so the client refuses to.
var ErrNotFound = errors.New("awsssm: no parameter of that name")

// ErrAlreadyExists means a no-overwrite write found the name taken.
//
// A REFUSAL RATHER THAN A REPLACEMENT is the whole point of the flag that
// produces it: the GitHub App private key is issued once and can never be
// re-fetched, so publishing it must never be able to overwrite anything.
var ErrAlreadyExists = errors.New("awsssm: a parameter already exists under that name")

// Client reads and writes Parameter Store values for one region.
//
//nolint:recvcheck // The redaction methods take a value receiver for the reason awsjson.Client's do; every other method needs the pointer.
type Client struct {
	api      *awsjson.Client
	endpoint string
}

// New builds a client for one region.
//
// THE ENDPOINT IS DERIVED AND NOT CONFIGURABLE, which is the rule the codebuild
// backend already states for this service and which matters more here: an
// operator override would be a way to send a deployment's private key to a host
// of somebody's choosing. A partition billet has not been taught about needs a
// code change rather than a config field.
func New(region string, creds awscreds.Source) *Client {
	return &Client{
		api:      awsjson.New("awsssm", region, creds),
		endpoint: awsjson.EndpointFor(service, region),
	}
}

// REDACTED, BECAUSE IT HOLDS A CREDENTIAL SOURCE IN AN UNEXPORTED FIELD.
//
// awsjson.Client redacts itself, and that is not enough: reflect refuses to call
// methods through an unexported field, so `%+v` on this struct would walk past
// that redaction into the source's own fields. The same trap awscreds.IMDS
// records, two packages along.
func (c Client) String() string { return "awsssm.Client{endpoint=" + c.endpoint + "}" }

// GoString covers %#v.
func (c Client) GoString() string { return c.String() }

// Format catches every verb, so no fallback prints the struct.
func (c Client) Format(f fmt.State, _ rune) { fmt.Fprint(f, c.String()) }

// MarshalJSON keeps a client out of anything that serializes it structurally.
func (c Client) MarshalJSON() ([]byte, error) { return json.Marshal(c.String()) }

// LogValue is what slog consults; its JSON handler ignores fmt entirely.
//
// IT MUST RETURN slog.Value, NOT any. slog.LogValuer is a named interface, and a
// method with a different result type does not implement it — the handler then
// falls back to reflecting over the struct, which is the whole failure this is
// here to prevent.
func (c Client) LogValue() slog.Value { return slog.StringValue(c.String()) }

// Parameter is one stored value and the version it was read at.
//
// THE VERSION IS THE PART A CALLER REASONS ABOUT. Parameter Store reads are
// eventually consistent, so "I fetched something" is not "I fetched the newest
// thing"; a caller that has recorded a floor elsewhere compares against this.
//
// NO String, GoString OR MarshalJSON, DELIBERATELY. This type IS the secret, and
// giving it a redacted rendering would make it look safe to log — which is the
// opposite of what a caller should conclude. Nothing in billet prints one.
type Parameter struct {
	Name    string
	Value   string
	Version int64
}

// Get reads one parameter, decrypting a SecureString.
//
// WithDecryption IS ALWAYS TRUE, because every value billet stores here is one.
// Making it a parameter would be offering a caller a way to receive ciphertext it
// cannot use and would then have to detect.
func (c *Client) Get(ctx context.Context, name string) (Parameter, error) {
	var out struct {
		Parameter struct {
			Name    string `json:"Name"`
			Value   string `json:"Value"`
			Version int64  `json:"Version"`
		} `json:"Parameter"`
	}

	in := map[string]any{"Name": name, "WithDecryption": true}

	if err := c.call(ctx, "GetParameter", in, &out); err != nil {
		if code, ok := awsjson.CodeOf(err); ok && code == "ParameterNotFound" {
			return Parameter{}, fmt.Errorf("%w: %s", ErrNotFound, name)
		}

		// THE ERROR IS NOT WRAPPED WITH THE NAME'S VALUE, only its NAME. A name is
		// a path an operator chose; the value is the deployment's private key.
		return Parameter{}, fmt.Errorf("awsssm: read %s: %w", name, err)
	}

	if out.Parameter.Value == "" {
		// AN EMPTY VALUE IS NOT AN ABSENT ONE, and collapsing them would let a
		// caller install an empty authority as though nothing were there. Parameter
		// Store refuses to store an empty string, so this is a response billet
		// cannot explain rather than a state it should act on.
		return Parameter{}, fmt.Errorf(
			"awsssm: %s answered with an empty value, which Parameter Store does not store; "+
				"something other than AWS answered, or the parameter was written by something "+
				"that is not billet", name)
	}

	return Parameter{
		Name:    out.Parameter.Name,
		Value:   out.Parameter.Value,
		Version: out.Parameter.Version,
	}, nil
}

// PutOptions are the choices a write makes that are not the value.
type PutOptions struct {
	// Overwrite REPLACES an existing value. False is a refusal, and it is what
	// publishing an unrepeatable credential uses.
	Overwrite bool

	// KMSKeyID names the key that encrypts the SecureString. Empty uses the
	// account's default SSM key, which is what a deployment that has not chosen
	// one gets.
	KMSKeyID string

	// Description is what an operator finding this parameter reads before deciding
	// whether removing it is safe.
	Description string
}

// Put writes one SecureString and returns the version it became.
//
// INTELLIGENT-TIERING, AND THE REASON IS MEASURED. A standard parameter caps its
// value at 4096 characters, and that limit is what stopped the codebuild backend
// running a single job until it was found: a real GitHub JIT registration exceeds
// it. An authority document carrying two certificates and two private keys
// exceeds it comfortably. Intelligent-Tiering keeps a parameter standard while
// the value fits and promotes it only when it does not.
func (c *Client) Put(ctx context.Context, name, value string, opts PutOptions) (int64, error) {
	in := map[string]any{
		"Name":      name,
		"Value":     value,
		"Type":      "SecureString",
		"Overwrite": opts.Overwrite,
		"Tier":      "Intelligent-Tiering",
	}

	if opts.KMSKeyID != "" {
		in["KeyId"] = opts.KMSKeyID
	}

	if opts.Description != "" {
		in["Description"] = opts.Description
	}

	var out struct {
		Version int64 `json:"Version"`
	}

	if err := c.call(ctx, "PutParameter", in, &out); err != nil {
		if code, ok := awsjson.CodeOf(err); ok && code == "ParameterAlreadyExists" {
			return 0, fmt.Errorf("%w: %s", ErrAlreadyExists, name)
		}

		// NOTHING FROM THIS CALL IS RENDERED BEYOND ITS CODE, because the request
		// body IS the credential and one shared error format string across the
		// write paths is how it reaches a log. The rule the codebuild JIT channel
		// states, for the same reason.
		return 0, fmt.Errorf("awsssm: write %s: %w", name, redactedCause(err))
	}

	return out.Version, nil
}

// Delete removes one parameter.
//
// ABSENCE IS SUCCESS, because every caller of this is a cleanup that has already
// decided the value should not exist — and a delete that fails because somebody
// else got there first has produced the outcome it wanted.
func (c *Client) Delete(ctx context.Context, name string) error {
	if err := c.call(ctx, "DeleteParameter", map[string]any{"Name": name}, nil); err != nil {
		if code, ok := awsjson.CodeOf(err); ok && code == "ParameterNotFound" {
			return nil
		}

		return fmt.Errorf("awsssm: delete %s: %w", name, err)
	}

	return nil
}

// call issues one action, retrying only what is transient.
func (c *Client) call(ctx context.Context, action string, in, out any) error {
	return c.api.Invoke(ctx, c.endpoint, service, targetPrefix+action, action, in, out,
		awsjson.Retryable)
}

// redactedCause reduces an error from a call whose REQUEST carried a secret to
// the parts that cannot have come from it.
//
// A SERVICE THAT ECHOES A REJECTED REQUEST BACK is not a thing to find out about
// from a log, and neither is a proxy that renders a body. The code is from a
// fixed enumeration and is what an operator acts on; everything else is dropped.
func redactedCause(err error) error {
	code, ok := awsjson.CodeOf(err)
	if !ok {
		// NOT AN API REFUSAL AT ALL — a transport failure, a refused redirect, a
		// signature this client built. None of those is composed from the request
		// body, so the error stands as it is.
		return err
	}

	status, _ := awsjson.StatusOf(err)

	return &awsjson.APIError{Service: "awsssm", Code: code, Status: status}
}

// PathFor joins a prefix and a leaf into a Parameter Store name.
//
// PARAMETER STORE NAMES ARE PATHS and a doubled or missing separator is a
// different parameter, not a formatting nicety — so this is one function rather
// than a concatenation at each call site.
func PathFor(prefix, leaf string) string {
	return strings.TrimSuffix(prefix, "/") + "/" + strings.TrimPrefix(leaf, "/")
}
