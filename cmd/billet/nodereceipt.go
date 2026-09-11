package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/junioryono/billet/internal/durablefile"
	"github.com/junioryono/billet/internal/endpoint"
	"github.com/junioryono/billet/internal/lifeops"
	"github.com/junioryono/billet/internal/regularfile"
)

// `billet node receipt` writes the durable endpoint receipt, in two modes.
// EVIDENCE MODE (`--evidence E --confirmation C --config PATH --run H`)
// follows a migration: the migration's evidence and the controller's
// confirmation are read strictly and must agree, the running node's record
// is re-verified under the bracket (a node restarted since the migration is
// refused, and the refresh is the way to a receipt), the configuration at
// the path the evidence names is re-read and must still be what the
// migration judged, and the receipt is written. REFRESH MODE (`--refresh
// --config PATH [--desired -] [--wait D] [--run H] [--dry-run]`) ends every
// ordinary converge: the running node's record is waited for (a node just
// started has none until its first registration), its effective endpoint,
// the installed configuration's and the rendering's are compared under the
// representation, and the receipt is rewritten for the current invocation,
// digest and incarnation, or left alone when a valid one already says so
// (its directory and parent flushed all the same). The configuration
// observation is carried through both modes and closed before the write or
// the shortcut. The receipt directory is created root 0700 with its parent
// flushed before anything is written into it, and the file is root 0600
// through the durable installer.

type receiptMode struct {
	evidence, confirmation, configPath, desired, run string
	refresh, dryRun                                  bool
	wait                                             time.Duration
}

// receiptAnswer is the successful answer of both modes.
type receiptAnswer struct {
	Schema  int              `json:"schema"`
	Outcome string           `json:"outcome"`
	Receipt *endpointReceipt `json:"receipt"`
	Why     string           `json:"why,omitempty"`
}

// The receipt writer's seams: the installer hook (a test observes or fails
// its steps), the directory flushes and the clock.
var (
	receiptInstaller = func() durablefile.Installer { return durablefile.Installer{} }
	receiptSyncDir   = func(dir string) error { return durablefile.Installer{}.SyncDirectory(dir) }
	receiptNow       = time.Now
	receiptMkdir     = os.Mkdir
)

func cmdNodeReceipt(ctx context.Context, args []string) error {
	flags := newFlagSet("billet node receipt")
	evidence := flags.String("evidence", "", "the migration's answer, as a file (evidence mode)")
	confirmation := flags.String("confirmation", "", "the controller's `rollout registration` answer, as a file (evidence mode)")
	configPath := flags.String("config", "", "the installed configuration (required in both modes)")
	desired := flags.String("desired", "", "with --refresh: the rendering the role installed, a path or - for stdin")
	run := flags.String("run", "", "the converge's holder, recorded in the receipt (required unless --dry-run)")
	refresh := flags.Bool("refresh", false, "keep the receipt current at the end of an ordinary converge")
	dryRun := flags.Bool("dry-run", false, "with --refresh: report what would be written; write nothing")
	wait := flags.Duration("wait", time.Minute, "with --refresh: how long to wait for a running node's current record")
	asJSON := flags.Bool("json", false, "print the answer as JSON (the only form)")

	if err := parse(flags, args); err != nil {
		return err
	}

	if !*asJSON {
		return errors.New("receipt answers as JSON; pass --json")
	}

	m := receiptMode{evidence: *evidence, confirmation: *confirmation, configPath: *configPath, desired: *desired,
		run: *run, refresh: *refresh, dryRun: *dryRun, wait: *wait}

	if r := checkReceiptCombination(m); r != nil {
		return answerEndpointRefusal(r)
	}

	if hostOS == "darwin" || receiptPath == "" {
		return answerEndpointRefusal(endpointRefuse(endpointReasonPlatform,
			"an endpoint receipt needs systemd and the node's runtime record, and this platform has neither", "", ""))
	}

	var (
		answer any
		r      *endpointRefusal
	)

	if m.refresh {
		answer, r = refreshReceipt(ctx, m)
	} else {
		answer, r = writeReceiptFromEvidence(ctx, m)
	}

	if r != nil {
		return answerEndpointRefusal(r)
	}

	return answerObject(answer)
}

func checkReceiptCombination(m receiptMode) *endpointRefusal {
	bad := func(why string) *endpointRefusal {
		return endpointRefuse(endpointReasonCombination, why,
			"receipt --evidence E --confirmation C --config PATH --run H --json, or receipt --refresh --config PATH "+
				"[--desired -] [--wait D] [--run H] [--dry-run] --json", "")
	}

	switch {
	case m.configPath == "":
		return bad("--config names the installed configuration, and it is empty")
	case m.refresh && (m.evidence != "" || m.confirmation != ""):
		return bad("--refresh takes no evidence or confirmation")
	case !m.refresh && (m.evidence == "" || m.confirmation == ""):
		return bad("evidence mode needs --evidence and --confirmation; an ordinary converge uses --refresh")
	case !m.refresh && (m.dryRun || m.desired != ""):
		return bad("--dry-run and --desired belong to --refresh")
	case m.run == "" && !m.dryRun:
		return bad("--run names the converge's holder, and it is empty")
	case m.wait <= 0:
		return bad("--wait must be positive")
	}

	if m.run != "" {
		if err := checkHolder(m.run); err != nil {
			return bad("--run: " + err.Error())
		}
	}

	return nil
}

// receiptConfirmation is the controller's `rollout registration` answer as
// evidence mode reads it.
type receiptConfirmation struct {
	Schema      int    `json:"schema"`
	Outcome     string `json:"outcome"`
	Node        string `json:"node"`
	Incarnation string `json:"incarnation"`
	Epoch       int64  `json:"epoch"`
	Live        bool   `json:"live"`
	Deployment  struct {
		Bound bool   `json:"bound"`
		ID    string `json:"id"`
	} `json:"deployment"`
}

// writeReceiptFromEvidence is evidence mode.
func writeReceiptFromEvidence(ctx context.Context, m receiptMode) (any, *endpointRefusal) {
	ev, r := readMigrationEvidence(m.evidence)
	if r != nil {
		return nil, r
	}

	conf, r := readConfirmation(m.confirmation)
	if r != nil {
		return nil, r
	}

	switch {
	case ev.Node != conf.Node:
		return nil, endpointRefuse(endpointReasonAgreement, fmt.Sprintf("the evidence names the node %q and the confirmation %q",
			ev.Node, conf.Node), "", "")
	case ev.Incarnation != conf.Incarnation:
		return nil, endpointRefuse(endpointReasonAgreement, fmt.Sprintf("the evidence names the incarnation %s and the "+
			"confirmation %s", ev.Incarnation, conf.Incarnation), "", "")
	case ev.Deployment != conf.Deployment.ID:
		return nil, endpointRefuse(endpointReasonAgreement, fmt.Sprintf("the evidence names the deployment %s and the "+
			"confirmation %s", ev.Deployment, conf.Deployment.ID), "", "")
	}

	abs, err := filepath.Abs(m.configPath)
	if err != nil {
		return nil, endpointUnknown(endpointReasonConfig, err.Error(), "", "")
	}

	if abs != ev.ConfigPath {
		return nil, endpointRefuse(endpointReasonConfig, fmt.Sprintf("--config names %s and the evidence was taken over %s",
			abs, ev.ConfigPath), "", "")
	}

	installed, r := observeInstalledConfig(m.configPath)
	if r != nil {
		return nil, r
	}

	if !installed.present {
		return nil, endpointRefuse(endpointReasonConfig, "no configuration is installed at "+installed.path, "", "")
	}

	if installed.sha256 != ev.InstalledSHA256 {
		return nil, endpointRefuse(endpointReasonConfig, "the installed configuration is not the one the migration judged "+
			"(its digest differs)", "converge again", "")
	}

	installedEP, hasNode, err := nodeEndpointOf(installed.cfg)
	if err != nil || !hasNode {
		return nil, endpointRefuse(endpointReasonConfig, "the installed configuration names no node endpoint", "", "")
	}

	if installedEP.String() != ev.To {
		return nil, endpointRefuse(endpointReasonConfig, fmt.Sprintf("the installed configuration names %s and the migration "+
			"moved the node to %s", installedEP.String(), ev.To), "converge again", "")
	}

	// THE RECORD RE-VERIFIED NOW, under the bracket.
	insp := endpointInspector()
	identity := expectedRegistrationIdentity(installed.cfg)

	br, problem := readRecordUnderBracket(ctx, insp, nodeUnit, identity)
	if problem != "" {
		return nil, endpointUnknown(endpointReasonRecord, problem, "", "")
	}

	switch {
	case !br.running:
		return nil, endpointRefuse(endpointReasonRecord, "the node is not running now; the migration's receipt cannot be "+
			"bound to a process", "start it and let the refresh write the receipt", "")
	case br.class != recordUsable:
		return nil, endpointRefuse(endpointReasonRecord, "the running node's record is not current: "+br.why,
			"a node restarted since the migration gets its receipt from the refresh", "")
	case br.record.InvocationID != ev.InvocationID || br.record.Incarnation != ev.Incarnation:
		return nil, endpointRefuse(endpointReasonRecord, fmt.Sprintf("the node runs as invocation %s incarnation %s and the "+
			"migration proved invocation %s incarnation %s; it was restarted since", br.record.InvocationID,
			br.record.Incarnation, ev.InvocationID, ev.Incarnation), "the refresh writes the receipt for the running node", "")
	case br.record.Endpoint != ev.To:
		return nil, endpointRefuse(endpointReasonRecord, fmt.Sprintf("the node dials %s and the migration moved it to %s",
			br.record.Endpoint, ev.To), "", "")
	case br.record.Node != ev.Node || br.record.Deployment != ev.Deployment:
		return nil, endpointRefuse(endpointReasonRecord, "the record names another node or deployment than the evidence", "", "")
	}

	rec := endpointReceipt{
		Schema: receiptSchema, Run: m.run, Node: ev.Node, Deployment: ev.Deployment, InstalledSHA256: installed.sha256,
		InstalledEndpoint: ev.To, EffectiveEndpoint: ev.To, InvocationID: ev.InvocationID, Incarnation: ev.Incarnation,
	}

	return publishReceipt(ctx, insp, installed, br, rec, false)
}

// refreshReceipt is refresh mode.
func refreshReceipt(ctx context.Context, m receiptMode) (any, *endpointRefusal) {
	installed, r := observeInstalledConfig(m.configPath)
	if r != nil {
		return nil, r
	}

	if !installed.present {
		return nil, endpointRefuse(endpointReasonConfig, "no configuration is installed at "+installed.path, "", "")
	}

	installedEP, hasNode, err := nodeEndpointOf(installed.cfg)
	if err != nil {
		return nil, endpointRefuse(endpointReasonConfig, "the installed configuration's node endpoint: "+err.Error(), "", "")
	}

	if !hasNode {
		if m.dryRun {
			return receiptAnswer{Schema: endpointSchema, Outcome: outcomeReported,
				Why: "no node is installed; the receipt follows the first converge that installs one"}, nil
		}

		return nil, endpointRefuse(endpointReasonConfig, "the installed configuration has no node section; nothing to receipt",
			"", "")
	}

	var (
		renderedEP      endpoint.Endpoint
		renderedHasNode bool
		hasRendering    bool
	)

	if m.desired != "" {
		_, cfg, r := readRendering(m.desired)
		if r != nil {
			return nil, r
		}

		renderedEP, renderedHasNode, err = nodeEndpointOf(cfg)
		if err != nil {
			return nil, endpointRefuse(endpointReasonDesired, "the rendering's node endpoint: "+err.Error(), "", "")
		}

		hasRendering = true
	}

	insp := endpointInspector()
	identity := expectedRegistrationIdentity(installed.cfg)

	first, problem := observeUnit(ctx, insp, nodeUnit)
	if problem != "" {
		return nil, endpointUnknown(endpointReasonUnit, problem, "", "")
	}

	if _, running, problem := runningPID(first); problem != "" {
		return nil, endpointUnknown(endpointReasonUnit, problem, "", "")
	} else if !running {
		if m.dryRun {
			return receiptAnswer{Schema: endpointSchema, Outcome: outcomeReported,
				Why: "a dry run writes no receipt: the node is not running (" + first.ActiveState + ")"}, nil
		}

		return nil, endpointRefuse(endpointReasonNotRunning, "the node is not running ("+first.ActiveState+
			"), so no receipt can be bound to a process", "", "")
	}

	br, elapsed, problem := waitForRecord(ctx, insp, nodeUnit, identity, m.wait)
	if problem != "" {
		return nil, endpointUnknown(endpointReasonUnit, problem, "", "")
	}

	switch {
	case !br.running:
		if m.dryRun {
			return receiptAnswer{Schema: endpointSchema, Outcome: outcomeReported,
				Why: "a dry run writes no receipt: the node stopped running under the wait"}, nil
		}

		return nil, endpointRefuse(endpointReasonNotRunning, "the node stopped running under the wait", "", "")
	case br.class == recordForeign:
		return nil, endpointRefuse(endpointReasonRecord, "the running node's record cannot be judged: "+br.why, "", "")
	case elapsed || br.class != recordUsable:
		if m.dryRun {
			return receiptAnswer{Schema: endpointSchema, Outcome: outcomeReported,
				Why: "a dry run writes no receipt: the running node published no current record within " + m.wait.String() +
					" (" + br.why + ")"}, nil
		}

		return nil, endpointRefuse(endpointReasonRecord, "the running node published no current record within "+
			m.wait.String()+" ("+br.why+")", "wait for it to register, then converge again", "")
	}

	effective, err := endpoint.ParseCanonical(br.record.Endpoint)
	if err != nil {
		return nil, endpointRefuse(endpointReasonRecord, "the record's endpoint: "+err.Error(), "", "")
	}

	if !effective.Equal(installedEP) {
		return nil, endpointRefuse(endpointReasonMigration, fmt.Sprintf("the running node dials %s and the installed "+
			"configuration names %s; a migration is owed", effective.String(), installedEP.String()),
			"billet node migrate-endpoint, which the converge runs before the node's restart", "")
	}

	if hasRendering && !m.dryRun && (!renderedHasNode || !renderedEP.Equal(installedEP)) {
		return nil, endpointRefuse(endpointReasonDesired, "the rendering was not installed: the installed configuration's "+
			"node endpoint is not the rendering's", "", "")
	}

	rec := endpointReceipt{
		Schema: receiptSchema, Run: m.run, Node: br.record.Node, Deployment: br.record.Deployment,
		InstalledSHA256: installed.sha256, InstalledEndpoint: installedEP.String(), EffectiveEndpoint: effective.String(),
		InvocationID: br.record.InvocationID, Incarnation: br.record.Incarnation,
	}

	if m.dryRun {
		if problem := closeInstalledConfig(installed); problem != "" {
			return nil, endpointUnknown(endpointReasonConfig, problem, "", "")
		}

		rec.WrittenAt = receiptNow().UTC().Format(time.RFC3339Nano)

		return receiptAnswer{Schema: endpointSchema, Outcome: outcomeReported, Receipt: &rec,
			Why: "a dry run writes no receipt; this is what a converge would write"}, nil
	}

	return publishReceipt(ctx, insp, installed, br, rec, true)
}

// publishReceipt is the write both modes end in: the directory established
// and its parent flushed before anything is written into it, the existing
// file validated whole and compared on its evidential members (a valid
// equal one is left alone, both flushes done all the same), the
// configuration observation and the process re-checked immediately before
// the write or the shortcut, then the durable write and the two closing
// flushes.
func publishReceipt(ctx context.Context, insp *lifeops.Inspector, installed *installedConfigObservation,
	br bracketedRecord, rec endpointReceipt, allowCurrent bool,
) (any, *endpointRefusal) {
	dir := filepath.Dir(receiptPath)
	parent := filepath.Dir(dir)

	// THE PARENT must be a root-owned directory and not a link; the receipt
	// directory is created when absent, root 0700, and its parent flushed
	// before anything is written into it.
	parentInfo, err := receiptLstat(parent)
	if err != nil {
		return nil, endpointUnknown(endpointReasonTrust, fmt.Sprintf("examine %s: %v", parent, err), "", "")
	}

	if why := receiptParentProblem(parent, parentInfo); why != "" {
		return nil, endpointRefuse(endpointReasonTrust, why, "", "")
	}

	dirInfo, err := receiptLstat(dir)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if err := receiptMkdir(dir, 0o700); err != nil {
			return nil, endpointUnknown(endpointReasonTrust, fmt.Sprintf("create the receipt directory %s: %v", dir, err), "", "")
		}

		if err := receiptSyncDir(parent); err != nil {
			return nil, endpointUnknown(endpointReasonTrust, fmt.Sprintf("flush %s after creating the receipt directory: %v",
				parent, err), "converge again; the flush is retried", "")
		}

		if dirInfo, err = receiptLstat(dir); err != nil {
			return nil, endpointUnknown(endpointReasonTrust, fmt.Sprintf("examine the receipt directory %s just created: %v",
				dir, err), "", "")
		}

		if why := receiptDirectoryProblem(dir, dirInfo); why != "" {
			return nil, endpointUnknown(endpointReasonTrust, "the receipt directory just created: "+why, "", "")
		}
	case err != nil:
		return nil, endpointUnknown(endpointReasonTrust, fmt.Sprintf("examine the receipt directory %s: %v", dir, err), "", "")
	default:
		if why := receiptDirectoryProblem(dir, dirInfo); why != "" {
			return nil, endpointRefuse(endpointReasonTrust, why, "", "")
		}
	}

	// sameDirectory says the name still holds the examined directory: the
	// write and the flushes resolve the pathname again, so a directory
	// swapped for another after the examination is caught here, before the
	// answer and never as `written`.
	sameDirectory := func(when string) *endpointRefusal {
		nowParent, err := receiptLstat(parent)
		if err != nil || !os.SameFile(nowParent, parentInfo) {
			return endpointUnknown(endpointReasonTrust, fmt.Sprintf("%s is not the directory examined (%s; %v)", parent, when, err),
				"", "")
		}

		if why := receiptParentProblem(parent, nowParent); why != "" {
			return endpointUnknown(endpointReasonTrust, why+" ("+when+")", "", "")
		}

		now, err := receiptLstat(dir)
		if err != nil || !os.SameFile(now, dirInfo) {
			return endpointUnknown(endpointReasonTrust, fmt.Sprintf("the receipt directory %s is not the one examined (%s; %v)",
				dir, when, err), "", "")
		}

		if why := receiptDirectoryProblem(dir, now); why != "" {
			return endpointUnknown(endpointReasonTrust, why+" ("+when+")", "", "")
		}

		return nil
	}

	// sameReceipt says the name still holds the inode judged, with the same
	// size, modification time, owner and mode.
	sameReceipt := func(judged os.FileInfo, when string) *endpointRefusal {
		now, err := receiptLstat(receiptPath)
		if err != nil || !os.SameFile(now, judged) || now.Size() != judged.Size() || !now.ModTime().Equal(judged.ModTime()) {
			return endpointUnknown(endpointReasonTrust, fmt.Sprintf("the receipt %s moved %s (%v)", receiptPath, when, err),
				"converge again", "")
		}

		if why := receiptFileProblem(receiptPath, now); why != "" {
			return endpointUnknown(endpointReasonTrust, why+" ("+when+")", "converge again", "")
		}

		return nil
	}

	// THE EXISTING FILE, validated whole before any shortcut. A regular
	// file that is not a receipt is REPLACED BY THE DURABLE RENAME after the
	// closing checks, never removed first: a removal would open a window in
	// which a receipt another publisher installed is the one removed. A name
	// that holds anything but a regular file refuses.
	existing := readEndpointReceipt(receiptPath)

	switch existing.presence {
	case receiptUnreadable:
		return nil, endpointUnknown(endpointReasonTrust, existing.why, "", "")
	case receiptInvalid:
		if existing.info == nil || !existing.info.Mode().IsRegular() {
			return nil, endpointRefuse(endpointReasonTrust, "the receipt path "+receiptPath+" holds something billet did not "+
				"write and cannot replace: "+existing.why, "remove it by hand", "")
		}
	}

	// THE CLOSING CHECKS, immediately before the write or the shortcut: the
	// configuration still the one observed, the process still the one the
	// record named.
	if problem := closeInstalledConfig(installed); problem != "" {
		return nil, endpointUnknown(endpointReasonConfig, problem, "", "")
	}

	closing, problem := observeUnit(ctx, insp, nodeUnit)
	if problem != "" {
		return nil, endpointUnknown(endpointReasonProcess, "at the close: "+problem, "", "")
	}

	if _, running, problem := runningPID(closing); problem != "" || !running || processMoved(br.obs, closing) ||
		closing.ActiveState == "deactivating" {
		return nil, endpointUnknown(endpointReasonProcess, fmt.Sprintf("the node moved before the receipt could be "+
			"written (pid %s, invocation %s, %s at the close; pid %s, invocation %s when its record was read)",
			closing.MainPID, closing.InvocationID, closing.ActiveState, br.obs.MainPID, br.obs.InvocationID),
			"converge again", "")
	}

	// THE SHORTCUT: a valid receipt already saying this is left alone, its
	// directory and parent flushed all the same (a flush an earlier
	// interruption owed is completed here).
	if allowCurrent && existing.presence == receiptPresent && existing.receipt.evidentialEqual(rec) {
		// THE FILE JUDGED IS STILL THE FILE AT THE NAME: the read is evidence
		// about an inode, and `current` is a claim about the name.
		if r := sameDirectory("before the shortcut"); r != nil {
			return nil, r
		}

		if r := sameReceipt(existing.info, "after it was read"); r != nil {
			return nil, r
		}

		for _, d := range []string{dir, parent} {
			if err := receiptSyncDir(d); err != nil {
				return nil, endpointUnknown(endpointReasonTrust, fmt.Sprintf("flush %s: %v", d, err), "converge again; the flush is retried", "")
			}
		}

		return receiptAnswer{Schema: endpointSchema, Outcome: outcomeCurrent, Receipt: existing.receipt}, nil
	}

	if r := sameDirectory("before the write"); r != nil {
		return nil, r
	}

	rec.WrittenAt = receiptNow().UTC().Format(time.RFC3339Nano)

	body, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return nil, endpointUnknown(endpointReasonTrust, "encode the receipt: "+err.Error(), "", "")
	}

	body = append(body, '\n')

	if _, err := receiptInstaller().Install(dir, filepath.Base(receiptPath), 0o600, func(w io.Writer) error {
		_, err := w.Write(body)

		return err
	}); err != nil {
		return nil, endpointUnknown(endpointReasonTrust, "write the receipt: "+err.Error(), "converge again", "")
	}

	if err := receiptSyncDir(parent); err != nil {
		return nil, endpointUnknown(endpointReasonTrust, fmt.Sprintf("flush %s after writing the receipt: %v", parent, err),
			"converge again; the flush is retried", "")
	}

	// THE PUBLISHED FILE READ BACK through the reader, at the examined
	// directory: what the name holds now is what this answer claims.
	if r := sameDirectory("after the write"); r != nil {
		return nil, r
	}

	back := readEndpointReceipt(receiptPath)
	if back.presence != receiptPresent || !back.receipt.evidentialEqual(rec) || back.receipt.Run != rec.Run ||
		back.receipt.WrittenAt != rec.WrittenAt {
		return nil, endpointUnknown(endpointReasonTrust, "the receipt read back after the write is not the one written ("+
			string(back.presence)+": "+back.why+")", "converge again", "")
	}

	// THE READ-BACK IS EVIDENCE ABOUT AN INODE; the name is checked once more
	// against it, in the examined directory.
	if r := sameDirectory("after the read-back"); r != nil {
		return nil, r
	}

	if r := sameReceipt(back.info, "after it was written"); r != nil {
		return nil, r
	}

	return receiptAnswer{Schema: endpointSchema, Outcome: outcomeWritten, Receipt: &rec}, nil
}

// readMigrationEvidence reads a migration's answer strictly: one object,
// one value per member, the outcome `migrated`, every member typed as the
// producer writes it, and the stopped triple exactly inactive/dead/success.
func readMigrationEvidence(path string) (*migrateEvidence, *endpointRefusal) {
	body, err := regularfile.ReadFile(path, maxEvidenceBytes, regularfile.Options{NoFollow: true})
	if err != nil {
		return nil, endpointRefuse(endpointReasonEvidence, fmt.Sprintf("read the evidence %s: %v", path, err), "", "")
	}

	if problem := strictObject(body); problem != "" {
		return nil, endpointRefuse(endpointReasonEvidence, "the evidence: "+problem, "", "")
	}

	// THE MEMBER SETS BY THEIR EXACT SPELLING, at both depths, before the
	// struct decode: Go matches a member name case-insensitively, so an
	// alias such as "Node" beside "node" would be admitted by the decoder
	// and the last one would win.
	if problem := exactAnswerMembers(body, evidenceMembers, map[string][]string{"stopped": stoppedMembers}); problem != "" {
		return nil, endpointRefuse(endpointReasonEvidence, "the evidence: "+problem, "", "")
	}

	var ev migrateEvidence

	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()

	if err := dec.Decode(&ev); err != nil {
		return nil, endpointRefuse(endpointReasonEvidence, "the evidence is not a migration's answer: "+err.Error(), "", "")
	}

	switch {
	case ev.Schema != endpointSchema:
		return nil, endpointRefuse(endpointReasonEvidence, fmt.Sprintf("the evidence's schema is %d, want %d", ev.Schema, endpointSchema), "", "")
	case ev.Outcome != outcomeMigrated:
		return nil, endpointRefuse(endpointReasonEvidence, fmt.Sprintf("the evidence's outcome is %q, not %q", ev.Outcome, outcomeMigrated), "", "")
	case ev.Node == "" || ev.Deployment == "" || ev.From == "" || ev.To == "" || ev.ConfigPath == "" || ev.RegisteredAt == "":
		return nil, endpointRefuse(endpointReasonEvidence, "the evidence has an empty member", "", "")
	case !filepath.IsAbs(ev.ConfigPath):
		return nil, endpointRefuse(endpointReasonEvidence, "the evidence's config_path is not absolute", "", "")
	case !sha256Hex.MatchString(ev.InstalledSHA256):
		return nil, endpointRefuse(endpointReasonEvidence, "the evidence's installed_sha256 is not 64 lowercase hex digits", "", "")
	case !hex32.MatchString(ev.InvocationID) || !hex32.MatchString(ev.Incarnation):
		return nil, endpointRefuse(endpointReasonEvidence, "the evidence's identifiers are not 32 hex characters", "", "")
	case ev.Stopped.ActiveState != "inactive" || ev.Stopped.SubState != "dead" || ev.Stopped.Result != "success":
		return nil, endpointRefuse(endpointReasonEvidence, fmt.Sprintf("the evidence's stopped triple is %s/%s/%s, and only "+
			"inactive/dead/success proves the stop", ev.Stopped.ActiveState, ev.Stopped.SubState, ev.Stopped.Result), "", "")
	}

	for _, text := range []string{ev.From, ev.To} {
		if _, err := endpoint.ParseCanonical(text); err != nil {
			return nil, endpointRefuse(endpointReasonEvidence, "the evidence's endpoint is not canonical: "+err.Error(), "", "")
		}
	}

	if _, err := time.Parse(time.RFC3339Nano, ev.RegisteredAt); err != nil {
		return nil, endpointRefuse(endpointReasonEvidence, "the evidence's registered_at is not RFC 3339", "", "")
	}

	return &ev, nil
}

// readConfirmation reads the controller's answer strictly: the outcome
// `confirmed`, `live` and `deployment.bound` JSON booleans true, the
// identifiers typed.
func readConfirmation(path string) (*receiptConfirmation, *endpointRefusal) {
	body, err := regularfile.ReadFile(path, maxEvidenceBytes, regularfile.Options{NoFollow: true})
	if err != nil {
		return nil, endpointRefuse(endpointReasonConfirm, fmt.Sprintf("read the confirmation %s: %v", path, err), "", "")
	}

	if problem := strictObject(body); problem != "" {
		return nil, endpointRefuse(endpointReasonConfirm, "the confirmation: "+problem, "", "")
	}

	if problem := exactAnswerMembers(body, confirmationMembers, map[string][]string{"deployment": confirmationDepMembers}); problem != "" {
		return nil, endpointRefuse(endpointReasonConfirm, "the confirmation: "+problem, "", "")
	}

	// THE BOOLEANS BY THEIR BYTES: a decoder would read "true" as a string
	// into nothing and a fixture writer could not tell.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, endpointRefuse(endpointReasonConfirm, "the confirmation is not JSON", "", "")
	}

	if !isJSONBool(raw["live"]) {
		return nil, endpointRefuse(endpointReasonConfirm, "the confirmation's live is not a boolean", "", "")
	}

	var dep map[string]json.RawMessage
	if err := json.Unmarshal(raw["deployment"], &dep); err != nil || !isJSONBool(dep["bound"]) {
		return nil, endpointRefuse(endpointReasonConfirm, "the confirmation's deployment.bound is not a boolean", "", "")
	}

	if !isJSONInteger(raw["epoch"]) {
		return nil, endpointRefuse(endpointReasonConfirm, "the confirmation's epoch is not an integer", "", "")
	}

	var conf receiptConfirmation

	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()

	if err := dec.Decode(&conf); err != nil {
		return nil, endpointRefuse(endpointReasonConfirm, "the confirmation is not a registration's answer: "+err.Error(), "", "")
	}

	switch {
	case conf.Schema != endpointSchema:
		return nil, endpointRefuse(endpointReasonConfirm, fmt.Sprintf("the confirmation's schema is %d, want %d", conf.Schema, endpointSchema), "", "")
	case conf.Outcome != outcomeConfirmed:
		return nil, endpointRefuse(endpointReasonConfirm, fmt.Sprintf("the confirmation's outcome is %q, not %q", conf.Outcome, outcomeConfirmed), "", "")
	case !conf.Live:
		return nil, endpointRefuse(endpointReasonConfirm, "the confirmation says the node is not live", "", "")
	case !conf.Deployment.Bound || conf.Deployment.ID == "":
		return nil, endpointRefuse(endpointReasonConfirm, "the confirmation's ledger is unbound", "", "")
	case conf.Node == "" || !hex32.MatchString(conf.Incarnation):
		return nil, endpointRefuse(endpointReasonConfirm, "the confirmation's node or incarnation is not typed", "", "")
	}

	return &conf, nil
}

// isJSONInteger says the raw member is a JSON integer literal (never null,
// never a string, never a fraction).
func isJSONInteger(raw json.RawMessage) bool {
	return jsonIntegerPattern.Match(bytes.TrimSpace(raw))
}

var jsonIntegerPattern = regexp.MustCompile(`^-?(0|[1-9]\d*)$`)

// The member sets the two answers carry, exactly.
var (
	evidenceMembers = []string{"schema", "outcome", "node", "deployment", "from", "to", "config_path", "installed_sha256",
		"invocation_id", "incarnation", "registered_at", "stopped"}
	stoppedMembers         = []string{"active_state", "sub_state", "result"}
	confirmationMembers    = []string{"schema", "outcome", "node", "incarnation", "epoch", "live", "deployment"}
	confirmationDepMembers = []string{"bound", "id"}
)

// exactAnswerMembers requires the object's member names to be exactly the given
// set, case-sensitively, and each named nested object's likewise.
func exactAnswerMembers(body []byte, want []string, nested map[string][]string) string {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return "not one JSON object"
	}

	if problem := exactAnswerMemberSet(raw, want); problem != "" {
		return problem
	}

	for name, members := range nested {
		var inner map[string]json.RawMessage
		if err := json.Unmarshal(raw[name], &inner); err != nil || inner == nil {
			return name + " is not an object"
		}

		if problem := exactAnswerMemberSet(inner, members); problem != "" {
			return name + ": " + problem
		}
	}

	return ""
}

func exactAnswerMemberSet(raw map[string]json.RawMessage, want []string) string {
	for _, name := range want {
		if _, ok := raw[name]; !ok {
			return fmt.Sprintf("the member %q is missing", name)
		}
	}

	for name := range raw {
		known := false

		for _, w := range want {
			if name == w {
				known = true
			}
		}

		if !known {
			return fmt.Sprintf("the member %q is not one the producer writes", name)
		}
	}

	return ""
}

// strictObject requires one JSON object with one value per member at every
// depth and nothing after it.
func strictObject(body []byte) string {
	dec := json.NewDecoder(bytes.NewReader(body))

	var probe map[string]json.RawMessage
	if err := dec.Decode(&probe); err != nil || probe == nil {
		return "not one JSON object"
	}

	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return "bytes follow the object"
	}

	switch key, repeated, err := jsonRepeatedMember(body); {
	case err != nil:
		return "could not be walked member by member: " + err.Error()
	case repeated:
		return fmt.Sprintf("repeats the member %q", key)
	}

	return ""
}
