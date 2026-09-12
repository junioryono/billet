package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/junioryono/billet/internal/retirement"
)

// The request's input is ONE JSON document on stdin, ingested whole before
// anything is examined: the round the role minted, this host's own report,
// the survivor's, every node's, and the serverless rendering. Every report
// is an ENVELOPE the role wrote around what it collected over its own
// authenticated transport, and the command binds each to the host the role
// names, to this run's round and to the reservation's clock.

// The bounds: the whole input, and each collected document inside it.
const (
	maxRetireInputBytes    = 16 << 20
	maxRetireEnvelopeBytes = 4 << 20
)

// retireInput is the request's document.
type retireInput struct {
	Schema int         `json:"schema"`
	Round  retireRound `json:"round"`
	// Self is this host's own report, Survivor the designated survivor's,
	// Nodes every node-bearing host's by inventory name.
	Self     *retireEnvelope            `json:"self"`
	Survivor *retireEnvelope            `json:"survivor"`
	Nodes    map[string]*retireEnvelope `json:"nodes"`
	// Desired is the serverless rendering's text, null on a server-only
	// request; DesiredNodes each node's full desired configuration's digest
	// and node endpoint, by inventory name.
	Desired      *string                      `json:"desired"`
	DesiredNodes map[string]retireDesiredNode `json:"desired_nodes"`
}

// retireRound is the collection round: its id carries the holder, its start
// is the retiring host's own clock read before the first delegation.
type retireRound struct {
	ID        string `json:"id"`
	StartedAt string `json:"started_at"`
}

// retireEnvelope is one collected report: the inventory host it came from,
// when it was collected (the same clock as the round's), the inspect document
// and the status document (null for a host with no ledger).
type retireEnvelope struct {
	Host        string          `json:"host"`
	CollectedAt string          `json:"collected_at"`
	Inspect     json.RawMessage `json:"inspect"`
	Status      json.RawMessage `json:"status"`

	// inspect and status are the decoded documents, filled by the binding.
	inspect, status report
	collected       time.Time
}

// retireDesiredNode is a node's desired configuration as the role rendered
// it: the digest of the whole file and its node endpoint.
type retireDesiredNode struct {
	SHA256   string `json:"sha256"`
	Endpoint string `json:"endpoint"`
}

// retireInputSchema numbers the input's shape.
const retireInputSchema = 1

// decodeRetireInput decodes the input strictly and holds its shape: the
// schema, a round with both members, a self envelope, every envelope's host
// equal to its key, every collected document under its bound and decodable.
func decodeRetireInput(raw []byte) (*retireInput, *retireRefusal) {
	var in retireInput
	if err := retirement.DecodeDocument(raw, &in); err != nil {
		return nil, retireRefuse(retireReasonInput, "the input does not decode: "+err.Error(), "")
	}

	refuse := func(why string) (*retireInput, *retireRefusal) {
		return nil, retireRefuse(retireReasonInput, why, "")
	}

	switch {
	case in.Schema != retireInputSchema:
		return refuse(fmt.Sprintf("the input is schema %d, not %d", in.Schema, retireInputSchema))
	case in.Round.ID == "" || in.Round.StartedAt == "":
		return refuse("the input's round names no id or no started_at")
	case in.Self == nil:
		return refuse("the input carries no self report")
	}

	for name, env := range in.Nodes {
		if env == nil {
			return refuse("the input's node report for " + name + " is null")
		}

		if env.Host != name {
			return refuse(fmt.Sprintf("the input's node report under %q says it is %q's", name, env.Host))
		}
	}

	for _, env := range in.envelopes() {
		if err := env.decode(); err != nil {
			return refuse(fmt.Sprintf("the report of %s: %v", env.Host, err))
		}
	}

	return &in, nil
}

// envelopes is every envelope present, self first, the survivor second, the
// nodes in name order.
func (in *retireInput) envelopes() []*retireEnvelope {
	out := []*retireEnvelope{in.Self}

	if in.Survivor != nil {
		out = append(out, in.Survivor)
	}

	names := make([]string, 0, len(in.Nodes))
	for name := range in.Nodes {
		names = append(names, name)
	}

	sort.Strings(names)

	for _, name := range names {
		out = append(out, in.Nodes[name])
	}

	return out
}

// decode decodes an envelope's two documents under the per-document bound;
// a status of null is a host with no ledger.
func (e *retireEnvelope) decode() error {
	if err := checkHolder(e.Host); err != nil {
		return fmt.Errorf("host: %w", err)
	}

	if len(e.Inspect) == 0 {
		return fmt.Errorf("carries no inspect document")
	}

	if int64(len(e.Inspect)) > maxRetireEnvelopeBytes || int64(len(e.Status)) > maxRetireEnvelopeBytes {
		return fmt.Errorf("carries a document longer than %d bytes", maxRetireEnvelopeBytes)
	}

	var err error

	e.inspect, err = decodeReport(e.Inspect)
	if err != nil {
		return fmt.Errorf("inspect document: %w", err)
	}

	if len(e.Status) != 0 && string(e.Status) != "null" {
		e.status, err = decodeReport(e.Status)
		if err != nil {
			return fmt.Errorf("status document: %w", err)
		}
	}

	e.collected, err = parseRetireTime(e.CollectedAt)
	if err != nil {
		return fmt.Errorf("collected_at: %w", err)
	}

	return nil
}

// parseRetireTime parses one of the input's times: RFC 3339, fractional
// seconds allowed.
func parseRetireTime(s string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("%q is not an RFC 3339 time", s)
	}

	return t, nil
}

// bindRetireInput holds the input to this request: the round's id carries the
// holder and its start, the self report is this host's, the survivor's is the
// designated survivor's, and every envelope is FRESH against the reservation:
// reserved_at ≤ started_at ≤ collected_at ≤ now, and now − started_at within
// the age bound. The retiring host's clock is the only one consulted.
func bindRetireInput(in *retireInput, m retireMode, reservedAt string, now time.Time, maxAge time.Duration) *retireRefusal {
	refuse := func(reason, why string) *retireRefusal { return retireRefuse(reason, why, "") }

	if in.Round.ID != m.run+"-"+in.Round.StartedAt {
		return refuse(retireReasonInput, fmt.Sprintf("the round's id %q is not this run's (%s) followed by its start", in.Round.ID, m.run))
	}

	started, err := parseRetireTime(in.Round.StartedAt)
	if err != nil {
		return refuse(retireReasonInput, "the round's started_at: "+err.Error())
	}

	reserved, err := parseRetireTime(reservedAt)
	if err != nil {
		return retireUnknown(retireReasonLedger, "the row's reserved_at: "+err.Error(), "")
	}

	switch {
	case started.Before(reserved):
		return refuse(retireReasonReportStale, fmt.Sprintf("the round started at %s, before the reservation at %s; a report "+
			"a request acts on never predates its own reservation", in.Round.StartedAt, reservedAt))
	case now.Before(started):
		return refuse(retireReasonReportStale, fmt.Sprintf("the round started at %s, after now (%s)", in.Round.StartedAt,
			now.UTC().Format(time.RFC3339Nano)))
	case now.Sub(started) > maxAge:
		return refuse(retireReasonReportStale, fmt.Sprintf("the round started at %s, longer than %s ago", in.Round.StartedAt, maxAge))
	}

	if in.Self.Host != m.retiringHost {
		return refuse(retireReasonReportHost, fmt.Sprintf("the self report is %s's, and this host is %s", in.Self.Host, m.retiringHost))
	}

	switch {
	case in.Survivor == nil:
		return refuse(retireReasonSurvivor, "the input carries no survivor report")
	case in.Survivor.Host != m.survivorHost:
		return refuse(retireReasonReportHost, fmt.Sprintf("the survivor report is %s's, and --survivor-host names %s",
			in.Survivor.Host, m.survivorHost))
	}

	for _, env := range in.envelopes() {
		switch {
		case env.collected.Before(started):
			return refuse(retireReasonReportStale, fmt.Sprintf("the report of %s was collected at %s, before the round started (%s)",
				env.Host, env.CollectedAt, in.Round.StartedAt))
		case now.Before(env.collected):
			return refuse(retireReasonReportStale, fmt.Sprintf("the report of %s was collected at %s, after now", env.Host, env.CollectedAt))
		}
	}

	return nil
}

// report is a collected JSON document, read through paths. A member the
// producer could not observe is `{"unknown": why}` and answers fieldUnknown
// wherever it is met on a path.
type report struct{ m map[string]any }

type fieldState int

const (
	fieldAbsent fieldState = iota
	fieldNull
	fieldUnknown
	fieldPresent
)

// decodeReport decodes a document strictly (no repeated member, nothing after
// it) into a report.
func decodeReport(raw []byte) (report, error) {
	var m map[string]any
	if err := retirement.DecodeDocument(raw, &m); err != nil {
		return report{}, err
	}

	if m == nil {
		return report{}, fmt.Errorf("the document is null")
	}

	return report{m: m}, nil
}

// get walks the path and answers the value with its state; the why of an
// unknown member is the producer's.
func (r report) get(path ...string) (any, fieldState, string) {
	if r.m == nil {
		return nil, fieldAbsent, ""
	}

	var cur any = r.m

	for _, key := range path {
		obj, ok := cur.(map[string]any)
		if !ok {
			return nil, fieldAbsent, ""
		}

		if why, unknown := unknownMember(obj); unknown {
			return nil, fieldUnknown, why
		}

		next, present := obj[key]
		if !present {
			return nil, fieldAbsent, ""
		}

		cur = next
	}

	switch v := cur.(type) {
	case nil:
		return nil, fieldNull, ""
	case map[string]any:
		if why, unknown := unknownMember(v); unknown {
			return nil, fieldUnknown, why
		}
	}

	return cur, fieldPresent, ""
}

// unknownMember says whether an object is the inspector's `{"unknown": why}`.
func unknownMember(obj map[string]any) (string, bool) {
	if len(obj) != 1 {
		return "", false
	}

	why, ok := obj["unknown"].(string)

	return why, ok
}

// str answers a string member; anything else is absent for the caller's
// purpose, and the state says why.
func (r report) str(path ...string) (string, fieldState, string) {
	v, st, why := r.get(path...)
	if st != fieldPresent {
		return "", st, why
	}

	s, ok := v.(string)
	if !ok {
		return "", fieldAbsent, ""
	}

	return s, fieldPresent, ""
}

// boolean answers a boolean member.
func (r report) boolean(path ...string) (bool, fieldState, string) {
	v, st, why := r.get(path...)
	if st != fieldPresent {
		return false, st, why
	}

	b, ok := v.(bool)
	if !ok {
		return false, fieldAbsent, ""
	}

	return b, fieldPresent, ""
}

// requireStr is a string member required present and equal to want; the
// refusal names the member and both values, and an unknown member is
// could-not-tell.
func (r report) requireStr(reason, who string, want string, path ...string) *retireRefusal {
	got, st, why := r.str(path...)

	member := strings.Join(path, ".")

	switch st {
	case fieldUnknown:
		return retireUnknown(reason, fmt.Sprintf("the report of %s could not observe %s: %s", who, member, why), "")
	case fieldPresent:
		if got == want {
			return nil
		}

		return retireRefuse(reason, fmt.Sprintf("the report of %s says %s is %q, and %q is required", who, member, got, want), "")
	default:
		return retireRefuse(reason, fmt.Sprintf("the report of %s carries no %s", who, member), "")
	}
}

// requireBool is a boolean member required present and equal to want.
func (r report) requireBool(reason, who string, want bool, path ...string) *retireRefusal {
	got, st, why := r.boolean(path...)

	member := strings.Join(path, ".")

	switch st {
	case fieldUnknown:
		return retireUnknown(reason, fmt.Sprintf("the report of %s could not observe %s: %s", who, member, why), "")
	case fieldPresent:
		if got == want {
			return nil
		}

		return retireRefuse(reason, fmt.Sprintf("the report of %s says %s is %t, and %t is required", who, member, got, want), "")
	default:
		return retireRefuse(reason, fmt.Sprintf("the report of %s carries no %s", who, member), "")
	}
}

// requireNull is a member required present and null.
func (r report) requireNull(reason, who string, path ...string) *retireRefusal {
	_, st, why := r.get(path...)

	member := strings.Join(path, ".")

	switch st {
	case fieldNull:
		return nil
	case fieldUnknown:
		return retireUnknown(reason, fmt.Sprintf("the report of %s could not observe %s: %s", who, member, why), "")
	case fieldAbsent:
		return retireRefuse(reason, fmt.Sprintf("the report of %s carries no %s", who, member), "")
	default:
		return retireRefuse(reason, fmt.Sprintf("the report of %s carries a %s, and none is required", who, member), "")
	}
}

// The reasons the request's binding and eligibility refuse with, beside the
// record-only modes'.
const (
	retireReasonReportHost       = "report-host"
	retireReasonReportStale      = "report-stale"
	retireReasonSurvivor         = "survivor"
	retireReasonSurvivorBinding  = "survivor-binding"
	retireReasonSurvivorFlagged  = "survivor-flagged"
	retireReasonSurvivorRetiring = "survivor-retiring"
	retireReasonAuthority        = "authority"
	retireReasonVariant          = "variant"
	retireReasonDesired          = "desired"
	retireReasonStateDir         = "state-dir"
	retireReasonUnbound          = "unbound"
	retireReasonTrust            = "trust"
	retireReasonNodeMissing      = "node-missing"
	retireReasonNodeDuplicate    = "node-duplicate"
	retireReasonNodeUnknown      = "node-unknown"
	retireReasonNode             = "node"
	retireReasonReceipt          = "receipt"
	retireReasonEndpoint         = "endpoint"
	retireReasonNodeChanged      = "node-changed"
	retireReasonPolicy           = "policy"
	retireReasonUnit             = "unit"
	retireReasonMount            = "mount"
	retireReasonNested           = "nested"
	retireReasonNodePath         = "node-path"
	retireReasonBackup           = "backup"
	retireReasonPhase            = "phase"
	retireReasonLifecycle        = "lifecycle"
	retireReasonStop             = "stop"
	retireReasonArchive          = "archive"
	retireReasonRewrite          = "rewrite"
	retireReasonRestart          = "restart"
)
