package scaleset

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"

	gh "github.com/actions/scaleset"
)

// DefaultRunnerGroup is where a scale set lands when a tier names no group.
const DefaultRunnerGroup = gh.DefaultRunnerGroup

// ScaleSet is billet's view of one provisioned scale set.
type ScaleSet struct {
	ID    int
	Name  string
	Group string
}

// EnsureScaleSet makes a tier's scale set exist and returns it.
//
// GitHub has no upsert here, so this is get-then-create — and the not-found
// answer is (nil, nil), which is why it is checked explicitly rather than by
// testing the error. A nil scale set with a nil error read as success would
// dereference on the next line.
//
// The create is retried through a second get on conflict. billet holds an
// exclusive lock on its state directory so two of its own processes cannot race
// here, but nothing stops an operator creating the scale set in the UI between
// the get and the create, and losing that race should be indistinguishable from
// winning it.
func (c *Client) EnsureScaleSet(ctx context.Context, name, group string, labels []string) (*ScaleSet, error) {
	if group == "" {
		group = DefaultRunnerGroup
	}

	rg, err := c.gh.GetRunnerGroupByName(ctx, group)
	if err != nil {
		return nil, fmt.Errorf("scaleset: find runner group %q: %w", group, err)
	}

	if rg == nil {
		return nil, fmt.Errorf("scaleset: runner group %q does not exist", group)
	}

	existing, err := c.gh.GetRunnerScaleSet(ctx, rg.ID, name)
	if err != nil {
		return nil, fmt.Errorf("scaleset: look up scale set %q: %w", name, err)
	}

	if existing != nil {
		// Adopted only if it is actually the scale set billet asked for.
		//
		// Name and group identify it, but LABELS decide which `runs-on` values
		// route here. A set someone created by hand with the same name and an
		// extra label silently pulls work into this tier that billet never
		// advertised for — and the tier's capacity accounting is sized for its own
		// labels, so the overflow lands as jobs that queue behind capacity that
		// was never meant for them.
		if err := checkLabels(name, existing, labels); err != nil {
			return nil, err
		}

		return &ScaleSet{ID: existing.ID, Name: existing.Name, Group: group}, nil
	}

	created, err := c.gh.CreateRunnerScaleSet(ctx, &gh.RunnerScaleSet{
		Name:          name,
		RunnerGroupID: rg.ID,
		Labels:        toLabels(labels),
		// Runner self-update is left ENABLED. GitHub stops queuing to runners more
		// than about a month old, so disabling updates converts a maintenance task
		// into a silent outage on someone else's schedule. billet rebuilds images
		// instead, and that is a decision the image pipeline owns rather than one
		// to smuggle in through a scale-set flag.
		RunnerSetting: gh.RunnerSetting{DisableUpdate: false},
	})
	if err != nil {
		// Lost the race, most likely. Ask again before reporting a failure: an
		// operator who created it in the UI a moment ago is not an error.
		if again, getErr := c.gh.GetRunnerScaleSet(ctx, rg.ID, name); getErr == nil && again != nil {
			// The SAME label check as the ordinary adoption path. Losing a create
			// race and inheriting whatever the winner configured is the exact
			// routing bug that check exists to prevent, reached by a different
			// door — and this door is the more likely one, because the thing
			// billet raced with is usually a human in the UI.
			if err := checkLabels(name, again, labels); err != nil {
				return nil, err
			}

			c.log.Info("scale set already existed; adopting it", "name", name, "group", group)

			return &ScaleSet{ID: again.ID, Name: again.Name, Group: group}, nil
		}

		return nil, fmt.Errorf("scaleset: create scale set %q: %w", name, err)
	}

	return &ScaleSet{ID: created.ID, Name: created.Name, Group: group}, nil
}

// checkLabels refuses a scale set whose labels are not the ones requested.
//
// It reports rather than patches. Rewriting somebody else's labels is a
// destructive act on an object billet did not create, and the operator who set
// them may have had a reason — so the answer is to say exactly what differs and
// let them decide.
func checkLabels(name string, existing *gh.RunnerScaleSet, want []string) error {
	have := make(map[string]bool, len(existing.Labels))
	for _, l := range existing.Labels {
		have[l.Name] = true
	}

	wanted := make(map[string]bool, len(want))
	for _, l := range want {
		wanted[l] = true
	}

	var missing, extra []string

	for l := range wanted {
		if !have[l] {
			missing = append(missing, l)
		}
	}

	for l := range have {
		if !wanted[l] {
			extra = append(extra, l)
		}
	}

	if len(missing) == 0 && len(extra) == 0 {
		return nil
	}

	sort.Strings(missing)
	sort.Strings(extra)

	return fmt.Errorf(
		"scaleset: a scale set named %q already exists with different labels "+
			"(missing %v, unexpected %v). billet will not rewrite labels it did not set — "+
			"fix them in GitHub, or point this tier at a different name",
		name, missing, extra)
}

// toLabels turns billet's plain label strings into GitHub's typed ones.
func toLabels(labels []string) []gh.Label {
	out := make([]gh.Label, 0, len(labels))
	for _, l := range labels {
		out = append(out, gh.Label{Name: l, Type: "User"})
	}

	return out
}

// JITRunner is a single-use runner registration.
//
// The encoded config is a CREDENTIAL until it is consumed. It is the thing that
// lets a process register as this runner and receive the job's token, so it gets
// the same treatment as the App private key: it cannot be rendered by fmt, by
// encoding/json, or by slog, and the raw value comes out only through Config()
// where the call site is visible in review.
//
// "No registration token ever enters the guest" is a claim billet makes, and it
// is true — but it must not be read as "no credential enters the guest". This
// one does. What is defensible is that it is single-use and scoped to one job.
//
//nolint:recvcheck // String/GoString/Format take VALUE receivers deliberately: a pointer-receiver String is not consulted when a value is formatted, which is how the App key leaked through %v before.
type JITRunner struct {
	// RunnerID is GitHub's id for the registration, needed to remove it.
	RunnerID int64
	// Name is the runner name billet asked for.
	Name string

	encodedConfig string
}

// Config returns the encoded JIT configuration.
//
// Named rather than exported as a field so that every use is a call a reader can
// find, and so no reflection-based encoder reaches it by accident.
func (j *JITRunner) Config() string { return j.encodedConfig }

// String redacts. See the type comment.
func (j JITRunner) String() string {
	return fmt.Sprintf("scaleset.JITRunner{RunnerID:%d Name:%q config:[redacted]}", j.RunnerID, j.Name)
}

// GoString covers %#v, which does not consult String.
func (j JITRunner) GoString() string { return j.String() }

// Format covers every verb, not just the ones fmt.Stringer is asked for. A verb
// fmt does not recognise for a struct falls back to printing the fields — which
// is how %d printed the App private key after String had already been added.
//
// The verb is deliberately ignored: there is no verb for which rendering this
// value differently is worth the chance of rendering it at all.
func (j JITRunner) Format(f fmt.State, _ rune) {
	// A write failure here is the caller's broken writer, and returning it is not
	// possible through this interface. What matters is that nothing else is
	// written in its place.
	if _, err := f.Write([]byte(j.String())); err != nil {
		return
	}
}

// MarshalJSON keeps the config out of anything that serializes to JSON,
// including slog's JSON handler, which ignores fmt entirely.
func (j JITRunner) MarshalJSON() ([]byte, error) {
	out, err := json.Marshal(struct {
		RunnerID int64  `json:"runner_id"`
		Name     string `json:"name"`
		Config   string `json:"config"`
	}{RunnerID: j.RunnerID, Name: j.Name, Config: "[redacted]"})
	if err != nil {
		return nil, fmt.Errorf("scaleset: marshal redacted jit runner: %w", err)
	}

	return out, nil
}

// LogValue is what slog asks for before falling back to reflection.
func (j JITRunner) LogValue() slog.Value {
	return slog.GroupValue(
		slog.Int64("runner_id", j.RunnerID),
		slog.String("name", j.Name),
		slog.String("config", "[redacted]"),
	)
}

// redactBody replaces an error whose text may contain a response body.
//
// It DROPS the message rather than scrubbing it. The config is base64 with no
// fixed marker to match on, so a filter would be guessing — and the App flow
// already established that filtering a body for secrets cannot work, because a
// secret out of its field is an opaque string. The chain survives, so errors.Is
// and errors.As still classify it.
func redactBody(err error) error { return &redactedError{err: err} }

// redactedError refuses to render an error, and refuses to hand it over.
type redactedError struct{ err error }

func (e *redactedError) Error() string {
	return "[redacted: this endpoint's response body carries the JIT credential]"
}

// Unwrap deliberately returns NOTHING.
//
// This is the same bug as the App manifest conversion, remade three weeks after
// it was fixed there: sanitising Error() while Unwrap hands back the original
// leaves the credential one errors.As away. Any reporter that walks causes and
// serialises them — which is what a structured logger does — renders the vendor
// error and its response body.
//
// There the fix was to rebuild the chain, because the code was a known string
// that could be found and removed. Here it cannot be: the JIT config is base64
// with no marker, so there is nothing to scrub and the only safe chain is no
// chain. What is lost is errors.Is/errors.As against the vendor's error, and
// nothing in billet classifies on it — the status is already in the message, and
// this endpoint's failures are all handled the same way.
func (e *redactedError) Unwrap() error { return nil }

// JITConfig generates a single-use runner registration for one job.
//
// One config, one runner, one job: the runner is registered ephemeral, takes the
// job it was created for, and is destroyed. That is what makes the credential in
// it acceptable to hand to a guest.
func (c *Client) JITConfig(ctx context.Context, scaleSetID int, runnerName, workFolder string) (*JITRunner, error) {
	if workFolder == "" {
		workFolder = "_work"
	}

	cfg, err := c.gh.GenerateJitRunnerConfig(ctx, &gh.RunnerScaleSetJitRunnerSetting{
		Name:       runnerName,
		WorkFolder: workFolder,
	}, scaleSetID)
	if err != nil {
		// The vendor's error carries the whole HTTP response body, and THIS
		// endpoint's success body contains the encoded JIT config. A proxy that
		// forwards a 200 body under a non-200 status therefore puts a live
		// credential inside an error that would otherwise be logged verbatim.
		//
		// Same shape as the App manifest conversion, and the same answer: this
		// endpoint's body is never rendered. The status and the runner name are
		// what an operator can act on anyway.
		return nil, fmt.Errorf("scaleset: generate JIT config for %q: %w",
			runnerName, redactBody(err))
	}

	if cfg == nil || cfg.EncodedJITConfig == "" {
		return nil, fmt.Errorf("scaleset: GitHub returned no JIT config for %q", runnerName)
	}

	out := &JITRunner{Name: runnerName, encodedConfig: cfg.EncodedJITConfig}
	if cfg.Runner != nil {
		// int64 because that is what RemoveRunner takes, and removing an orphaned
		// registration is the operation that has to work when something has gone
		// wrong. Upstream is inconsistent with itself here — RunnerReference.ID and
		// GetRunner use int while RemoveRunner uses int64 — so billet picks the
		// width its cleanup path needs and converts once.
		out.RunnerID = int64(cfg.Runner.ID)
	}

	return out, nil
}
