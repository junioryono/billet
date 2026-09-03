package codebuild

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/provider"
)

// THE CONTROL PLANE'S SWEEP OF STAGED REGISTRATIONS A DEAD NODE NEVER REAPED.
//
// A build cannot be handed a secret, so a runner's JIT configuration is staged in
// Parameter Store before StartBuild and outlives the build. Three places remove it
// and each holds a proof: a conclusive launch refusal, a confirmed teardown, and
// custody settlement. A node process that dies between staging one and reaching any
// of them leaks exactly one — measured during the backend's acceptance, where a
// killed node left three behind.
//
// NOTHING ON THE NODE CAN AUTHORISE CLEANING THAT UP, which is why this runs on the
// control plane. From the provider alone, "no build for this lease" and "the build
// has not appeared yet" are the same observation, and deleting on the second
// strands a live build: it starts, resolves nothing, registers nothing, and every
// signal says the launch merely failed. The one sound authorisation is the
// LEDGER's — the lease is terminal, and has been for longer than any build could
// still be running — so the ledger's answer is INJECTED here (ClosureLookup) and
// this package decides nothing about a lease on its own.
//
// WHAT THE CONTROLLER MAY DO IS LIST NAMES AND DELETE THEM. It never reads a
// registration: the listing asks for no decryption, the response is decoded into a
// type with no Value field, and the IAM grant beside this (SweepIAMActions) carries
// no GetParameter and no KMS action. THE FIRST TWO ARE THE BOUNDARY, NOT THE THIRD:
// measured on 2026-09-02, a role holding exactly that grant received plaintext the
// moment it asked for decryption, because the account's aws/ssm key authorises any
// principal reaching it through Parameter Store. Under a customer-managed key the
// grant is decisive too (measured: the same role was refused kms:Decrypt and its
// delete still succeeded); under the default key only this code is.

// LeaseClosure is what the ledger knows about whether one lease is over.
//
// THREE ANSWERS, NOT TWO, and the caller supplies them. Known=false is a lease the
// ledger has never heard of; Terminal=false is one still open; Terminal=true is one
// that released its capacity, closed at FinishedAt. Only the third, aged past
// ServiceInventoryWindow, authorises a delete. The first is reported and kept: it
// is what a ledger restored from an older backup looks like, and a build may be
// running under it.
type LeaseClosure struct {
	Known    bool
	Terminal bool
	// FinishedAt is when the ledger closed the lease. Zero means it cannot say,
	// which is never old enough.
	FinishedAt time.Time
}

// ClosureLookup answers LeaseClosure for one lease id, from the ledger.
//
// AN ERROR STOPS THE PASS. A ledger that could not answer is evidence about
// nothing, so the sweep deletes nothing further and reports the failure rather
// than reading silence as permission — the could-not-tell/no rule this repository
// applies to every deletion.
type ClosureLookup func(ctx context.Context, leaseID string) (LeaseClosure, error)

// ServiceInventoryWindow is how long after a lease closed its staged registration
// is still kept.
//
// THE SERVICE MAXIMUM, NOT A NODE'S DECLARED CEILING. CodeBuild ends a build once
// its queued and build ceilings elapse, so a registration whose lease closed longer
// ago than the largest possible pair (plus the slack config already adds) cannot be
// read by anything again — whatever the node that staged it had declared, because
// every declared window is at most this one. Deriving it from the service rather
// than from a registration is what lets the sweep need nothing about a node but
// its path.
var ServiceInventoryWindow = time.Duration(
	(*config.CodeBuildConfig)(nil).InventoryWindowMinutes()) * time.Minute

// sweepPageSize is what GetParametersByPath caps a page at.
const sweepPageSize = 10

// SweepReport is what one pass over one path found and did.
type SweepReport struct {
	Region string
	Path   string
	// Removed is what the pass deleted.
	Removed int
	// Kept is waiting on a lease that is open or closed too recently.
	Kept int
	// Unaccounted names lease ids the ledger has never heard of. Kept, and worth
	// a person's look.
	Unaccounted int
	// Foreign is entries under the path that are not billet's at all. Kept.
	Foreign int
}

// RegistrationSweeper lists one Parameter Store path and removes the registrations
// the ledger has proved dead.
//
//nolint:recvcheck // The redaction methods take a value receiver for the reason the client's do: a pointer-receiver String is not consulted when a VALUE is formatted. Sweep needs the pointer.
type RegistrationSweeper struct {
	api    *client
	region string
	path   string
	window time.Duration
	now    func() time.Time
	log    *slog.Logger
}

// REDACTED, because it holds a credential source through an unexported field of
// an unexported struct, and fmt cannot invoke methods through either. Same five
// methods as the client, same value receiver.
func (s RegistrationSweeper) String() string {
	return "codebuild.RegistrationSweeper{path=" + s.path + "}"
}

// GoString covers %#v.
func (s RegistrationSweeper) GoString() string { return s.String() }

// Format catches every verb.
func (s RegistrationSweeper) Format(f fmt.State, _ rune) {
	_, _ = io.WriteString(f, s.String()) //nolint:errcheck // fmt.State swallows write errors by design
}

// MarshalJSON keeps a sweeper out of anything that serializes it structurally.
func (s RegistrationSweeper) MarshalJSON() ([]byte, error) { return json.Marshal(s.String()) }

// LogValue is what slog consults; its JSON handler ignores fmt entirely.
func (s RegistrationSweeper) LogValue() slog.Value { return slog.StringValue(s.String()) }

// SweepOption configures a RegistrationSweeper.
type SweepOption func(*RegistrationSweeper)

// SweepWithLogger sets the logger. The default is slog.Default().
func SweepWithLogger(log *slog.Logger) SweepOption {
	return func(s *RegistrationSweeper) { s.log = log }
}

// SweepWithHTTPClient sets the client used for API calls, for a test or a proxy.
func SweepWithHTTPClient(c *http.Client) SweepOption {
	return func(s *RegistrationSweeper) { s.api.setHTTPClient(c) }
}

// SweepWithClock replaces the clock the window is measured against.
func SweepWithClock(now func() time.Time) SweepOption {
	return func(s *RegistrationSweeper) { s.now = now }
}

// NewRegistrationSweeper builds a sweeper for one path in one region.
//
// THE PATH IS RE-VALIDATED HERE because it arrived over the wire rather than
// through config.Load — the alloc.New rule — and because it is a prefix under
// which this process will DELETE. Parameter Store's endpoint is derived from the
// region and never configurable, for the reason the provider's is: it is where
// registrations live.
func NewRegistrationSweeper(
	region, path string, creds CredentialSource, opts ...SweepOption,
) (*RegistrationSweeper, error) {
	region, path = strings.TrimSpace(region), strings.TrimSpace(path)

	if err := config.CheckCodeBuildRegion(region); err != nil {
		return nil, fmt.Errorf("codebuild: registration sweep: %w", err)
	}

	if err := config.CheckSSMParameterPath(path); err != nil {
		return nil, fmt.Errorf("codebuild: registration sweep: the path %w", err)
	}

	if creds == nil || isNilValue(creds) {
		return nil, errors.New("codebuild: registration sweep: a credential source is required; " +
			"nothing may sign with credentials a caller did not choose")
	}

	s := &RegistrationSweeper{
		api:    newClient(region, "", creds),
		region: region,
		path:   path,
		window: ServiceInventoryWindow,
		now:    time.Now,
		log:    slog.Default(),
	}

	for _, opt := range opts {
		opt(s)
	}

	switch {
	case s.api.httpClient() == nil:
		return nil, errors.New("codebuild: registration sweep: SweepWithHTTPClient was given no client")
	case s.log == nil:
		return nil, errors.New("codebuild: registration sweep: SweepWithLogger was given no logger")
	case s.now == nil:
		return nil, errors.New("codebuild: registration sweep: SweepWithClock was given no clock")
	}

	return s, nil
}

// parametersByPathPage is what GetParametersByPath answers with, reduced to what
// the sweep acts on.
//
// THERE IS NO Value FIELD, AND THAT IS THE SECURITY PROPERTY OF THIS TYPE. The
// request asks for no decryption, so a SecureString's Value arrives as ciphertext
// (MEASURED 2026-09-02: ~250 bytes beginning with the KMS header `AQICAHi`, and
// not the plaintext that was staged) — but a type that decoded it would hold
// registration-shaped bytes in a struct that reports, logs and tests can reach.
// Decoding only the name and the write time means the response buffer is the only
// place those bytes ever are, and TestTheSweepNeverDecodesAValue pins the field set.
//
// LastModifiedDate IS THE SECOND AGE PROOF, from AWS's clock rather than the
// ledger's; see stagedParameter.
type parametersByPathPage struct {
	Parameters []stagedParameter `json:"Parameters"`
	NextToken  string            `json:"NextToken"`
}

// stagedParameter is one listed name and when Parameter Store last wrote it.
//
// THE WRITE TIME IS A SECOND, INDEPENDENT PROOF THAT NOTHING CAN STILL READ THE
// PARAMETER, and it is required beside the ledger's. The ledger's finished_at is
// written by whichever process terminalized the lease — a control plane, or an
// operator command on another machine — with THAT process's clock; a clock more
// than a window slow would close a lease with a finished_at already old enough,
// and the ledger proof alone would then release a registration whose build was
// still queued. A registration is written before StartBuild and CodeBuild ends the
// build within its ceilings, so a parameter older than the window by AWS's own
// timestamp cannot be read by anything again, whatever any billet clock says.
// Both proofs must hold; a parameter that reports no write time is kept.
type stagedParameter struct {
	Name string `json:"Name"`
	// LastModifiedDate is unix seconds, possibly fractional, the way every AWS
	// JSON 1.1 timestamp arrives.
	LastModifiedDate float64 `json:"LastModifiedDate"`
}

// writtenAt is the parameter's write time, or zero when the service gave none.
func (p stagedParameter) writtenAt() time.Time {
	if p.LastModifiedDate <= 0 {
		return time.Time{}
	}

	sec := int64(p.LastModifiedDate)
	nsec := int64((p.LastModifiedDate - float64(sec)) * 1e9)

	return time.Unix(sec, nsec).UTC()
}

// Sweep lists the path once and removes every registration whose lease the ledger
// has proved closed for longer than the service window.
//
// THE REPORT IS RETURNED BESIDE AN ERROR, deliberately: a pass that stopped
// part-way still removed and kept what it counted, and a caller recording the pass
// should record that alongside why it stopped.
func (s *RegistrationSweeper) Sweep(ctx context.Context, closed ClosureLookup) (SweepReport, error) {
	report := SweepReport{Region: s.region, Path: s.path}

	if closed == nil {
		return report, errors.New("codebuild: registration sweep: no ledger lookup was supplied, " +
			"and nothing else may authorise removing a staged registration")
	}

	// THE WHOLE LISTING FIRST, AND ONLY THEN ANY DELETE. A pagination token is a
	// cursor into a listing this pass is about to change, and MEASURED against real
	// Parameter Store in us-west-2 on 2026-09-02 the cursor is POSITIONAL: three
	// parameters at MaxResults 1, delete the one page one returned, and page two
	// fetched with the old token returns the THIRD — the second, which still
	// existed, is never listed. A sweep that deleted as it paged would leave every
	// name behind each delete unswept and report a clean pass. Collecting the names
	// costs one small slice; acting on a complete listing removes the question.
	listed, err := s.listParameters(ctx)
	if err != nil {
		return report, err
	}

	now := s.now()
	prefix := s.path + "/"

	for _, p := range listed {
		name := p.Name

		rel, ours := strings.CutPrefix(name, prefix)
		if !ours || rel == "" || strings.Contains(rel, "/") {
			report.Foreign++

			continue
		}

		leaseID, ok := provider.LeaseOf(rel)
		if !ok {
			report.Foreign++

			continue
		}

		closure, err := closed(ctx, leaseID)
		if err != nil {
			// THE LEDGER COULD NOT ANSWER, SO NOTHING FURTHER IS TOUCHED. Every name
			// before this one was decided on an answer; every name after it would be
			// decided on silence.
			return report, fmt.Errorf("codebuild: the ledger could not say whether lease %s "+
				"is closed, so the sweep of %s stopped with nothing further removed: %w",
				leaseID, s.path, err)
		}

		switch {
		case !closure.Known:
			// A LEASE THIS LEDGER HAS NEVER SEEN IS A PERSON'S QUESTION. A ledger
			// restored from an older backup, or another writer on the path — and in
			// the first case a build may be running under it right now.
			report.Unaccounted++

			s.log.Warn("a staged runner registration names a lease this ledger has never "+
				"heard of; it is kept, and somebody should look at how it got there",
				"parameter", name, "lease", leaseID)

		case !closure.Terminal:
			report.Kept++

		// KEPT AT THE BOUNDARY: the rule is "closed LONGER AGO than the window", so
		// a closure exactly one window old is one the window still covers.
		case !olderThan(closure.FinishedAt, s.window, now):
			report.Kept++

		// AND THE PARAMETER ITSELF MUST BE OLDER THAN THE WINDOW BY AWS'S CLOCK. See
		// stagedParameter: the ledger's time came from whichever billet closed the
		// lease, and a clock a window slow would otherwise release a registration
		// its build is still waiting to read.
		case !olderThan(p.writtenAt(), s.window, now):
			report.Kept++

		default:
			if err := s.api.deleteParameter(ctx, name); err != nil {
				return report, fmt.Errorf("codebuild: remove the staged registration for %s: %w",
					rel, err)
			}

			report.Removed++

			s.log.Info("removed a staged runner registration whose lease closed longer ago "+
				"than any build could still be running",
				"instance", rel, "closed", closure.FinishedAt.UTC().Format(time.RFC3339))
		}
	}

	return report, nil
}

// olderThan reports whether t is STRICTLY more than window before now, and false
// for a zero t, which is a time nothing could vouch for.
func olderThan(t time.Time, window time.Duration, now time.Time) bool {
	if t.IsZero() {
		return false
	}

	return now.After(t.Add(window))
}

// listParameters walks every page of the path and returns what it holds, each
// name once.
//
// DEDUPLICATED, because the cursor is positional (measured; see Sweep) and a name
// staged BETWEEN two page requests, sorting before the cursor, shifts the listing
// so the previous page's last name comes back again. Counted twice, a terminal
// lease's registration would be "removed" twice — the second delete answers
// ParameterNotFound, which is success — and the inflated count is accumulated
// into removed_total for good.
func (s *RegistrationSweeper) listParameters(ctx context.Context) ([]stagedParameter, error) {
	var (
		listed []stagedParameter
		names  = map[string]struct{}{}
	)

	token := ""
	// EVERY TOKEN, NOT ONLY THE LAST ONE — the cycle guard the inventory walk
	// carries, for the same reason: a listing that never ends is a sweep that never
	// reports and a tick that never returns.
	seen := map[string]struct{}{}

	for {
		in := map[string]any{
			"Path": s.path,
			// ONE LEVEL, so a nested hierarchy somebody else keeps under a shared
			// prefix is not walked, and NO DECRYPTION, stated rather than defaulted:
			// this process lists names and must never hold a registration.
			"Recursive":      false,
			"WithDecryption": false,
			"MaxResults":     sweepPageSize,
		}
		if token != "" {
			in["NextToken"] = token
		}

		var page parametersByPathPage
		if err := s.api.callSSM(ctx, "GetParametersByPath", in, &page); err != nil {
			return nil, fmt.Errorf("codebuild: list the staged registrations under %s: %s",
				s.path, ssmFailure(err))
		}

		for _, p := range page.Parameters {
			if _, dup := names[p.Name]; dup {
				continue
			}

			names[p.Name] = struct{}{}
			listed = append(listed, p)
		}

		if page.NextToken == "" {
			return listed, nil
		}

		if _, repeated := seen[page.NextToken]; repeated {
			return nil, fmt.Errorf("codebuild: listing the staged registrations under %s "+
				"revisited a pagination token, so the sweep would not end; stopping", s.path)
		}

		seen[page.NextToken] = struct{}{}
		token = page.NextToken
	}
}

// ssmFailure renders a Parameter Store failure by its code alone.
//
// THE CODE AND NOTHING ELSE, the rule every SSM call in this package follows: the
// message is the service's prose about a request that names registrations, and a
// fixed enumeration is what an operator acts on anyway.
func ssmFailure(err error) string {
	if errors.Is(err, errRedirected) {
		return err.Error()
	}

	if code, ok := codeOf(err); ok {
		return code
	}

	return "the api could not be reached"
}
