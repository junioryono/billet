package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The second review round's cases: the stop authorised by a fresh
// observation, the configuration closed before the stop and before the
// start, the post-start wait bound to the started invocation, the
// judgement's retries, a name that cannot be resolved, an incomplete pre-R
// probe, the receipt's directory and file identities, the answers' exact
// member sets and the controller's identity at every poll.

// The action's shows before the stop: the first observation and the
// bracket's two; the fourth is the observation immediately before the stop.
const showsBeforeTheStop = 3

// A fresh observation immediately before the stop authorises it: the same
// process the record was read from, not stopping, loaded, under a KillMode
// whose stop proves something.
func TestMigrateObservesTheUnitAfreshBeforeTheStop(t *testing.T) {
	for _, c := range []struct {
		name    string
		body    string
		outcome string
		reason  string
	}{
		{"the unit stopping", nodeUnitBody("deactivating", "stop-sigterm", inspectPID, nodeInvocation, "mixed", "success"),
			outcomeRefused, endpointReasonStopping},
		{"the process moved", nodeUnitBody("active", "running", 9999, newInvocation, "mixed", "success"),
			outcomeUnknown, endpointReasonProcess},
		{"the policy changed", nodeUnitBody("active", "running", inspectPID, nodeInvocation, "process", "success"),
			outcomeRefused, endpointReasonPolicy},
		{"the unit masked", strings.Replace(nodeUnitBody("active", "running", inspectPID, nodeInvocation, "mixed", "success"),
			"LoadState=loaded", "LoadState=masked", 1), outcomeRefused, endpointReasonUnit},
		{"the unit unobservable", "LoadState=loaded\n", outcomeUnknown, endpointReasonUnit},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newEndpointFixture(t)
			f.installB(t)
			f.afterStop(t, "inactive", "dead", "success")
			f.afterStart(t)
			f.recordAfterStart(t, canonicalB)
			f.afterShows(t, showsBeforeTheStop, c.body)

			o := f.migrate(t, f.rendering(endpointB))
			mustEndpointRefusal(t, o, c.outcome, c.reason)

			if o.str("state") != stateNothing || f.calls(t, "stop") != 0 {
				t.Errorf("state %q stops %d", o.str("state"), f.calls(t, "stop"))
			}
		})
	}
}

// The configuration is closed before the stop (a replacement under the
// record wait stops nothing) and before the start (a replacement under the
// stop leaves the node stopped rather than starting it on a file nobody
// judged).
func TestMigrateClosesTheConfigurationBeforeTheStopAndBeforeTheStart(t *testing.T) {
	t.Run("replaced under the record wait", func(t *testing.T) {
		f := newEndpointFixture(t)
		f.installB(t)
		f.afterStop(t, "inactive", "dead", "success")
		f.afterStart(t)
		f.recordAfterStart(t, canonicalB)
		f.onRecordRead(t, 1, func() {
			tmp := f.configPath + ".new"
			writeFile(t, tmp, f.rendering(endpointB), 0o644)
			mustOK(t, os.Rename(tmp, f.configPath))
		})

		o := f.migrate(t, f.rendering(endpointB))
		mustEndpointRefusal(t, o, outcomeUnknown, endpointReasonConfig)

		if o.str("state") != stateNothing || f.calls(t, "stop") != 0 {
			t.Errorf("state %q stops %d", o.str("state"), f.calls(t, "stop"))
		}
	})

	t.Run("replaced under the stop", func(t *testing.T) {
		f := newEndpointFixture(t)
		f.installB(t)
		f.afterStop(t, "inactive", "dead", "success")
		f.afterStart(t)
		f.recordAfterStart(t, canonicalB)
		f.stopMode(t, "replace-config")

		o := f.migrate(t, f.rendering(endpointB))
		mustEndpointRefusal(t, o, outcomeUnknown, endpointReasonConfig)

		if o.str("state") != stateStopped || f.calls(t, "stop") != 1 || f.calls(t, "start") != 0 {
			t.Errorf("state %q calls %v", o.str("state"), f.systemctlCalls(t))
		}
	})
}

// The post-start wait is bound to the invocation the start produced: a
// restart under the wait publishes a record under a later invocation, which
// is not the process this migration started.
func TestMigrateBindsTheRecordWaitToTheStartedInvocation(t *testing.T) {
	f := newEndpointFixture(t)
	f.installB(t)
	f.afterStop(t, "inactive", "dead", "success")
	f.afterStart(t)

	later := "2222222222222222222222222222cccc"

	// The started node (invocation newInvocation) has no record; on the
	// first post-start read the unit is already another invocation with a
	// record of its own.
	f.onRecordRead(t, 2, func() {
		f.setNode(t, "active", "running", inspectPID+2, later, "mixed")
		f.writeRecord(t, f.record(map[string]any{"deployment": f.deployment, "endpoint": canonicalB,
			"invocation_id": later, "incarnation": newIncarnation}))
	})

	o := f.migrate(t, f.rendering(endpointB), "--wait", "3s")
	mustEndpointRefusal(t, o, outcomeUnknown, endpointReasonProcess)

	if o.str("state") != stateStarted || !strings.Contains(o.str("why"), "moved under the wait") {
		t.Errorf("why %q state %q", o.str("why"), o.str("state"))
	}

	if f.calls(t, "stop") != 1 {
		t.Errorf("stops %d", f.calls(t, "stop"))
	}
}

// The judgement is retried when its closing observation finds the process
// moved: one movement followed by stability answers; a process that keeps
// moving exhausts the attempts.
func TestMigrateRetriesAJudgementWhoseProcessMoved(t *testing.T) {
	moved := nodeUnitBody("active", "running", 9999, newInvocation, "mixed", "success")

	t.Run("one movement then stability", func(t *testing.T) {
		f := newEndpointFixture(t)
		// The first attempt's closing show (the fourth) sees another
		// process; the second attempt's shows see the original.
		f.showsBetween(t, 3, 4, moved)

		o := f.migrate(t, f.rendering(endpointB), "--dry-run")
		mustEndpointOutcome(t, o, outcomeReported)

		if n := f.calls(t, "show"); n != 8 {
			t.Errorf("%d shows, want the first attempt's four and the retry's four", n)
		}
	})

	t.Run("exhaustion", func(t *testing.T) {
		f := newEndpointFixture(t)
		f.everyShow(t, 4, moved)

		o := f.migrate(t, f.rendering(endpointA))
		mustEndpointRefusal(t, o, outcomeUnknown, endpointReasonProcess)

		if n := f.calls(t, "show"); n < 12 {
			t.Errorf("%d shows, want three attempts", n)
		}
	})

	t.Run("a unit found stopping is not retried", func(t *testing.T) {
		f := newEndpointFixture(t)
		f.afterShows(t, 3, nodeUnitBody("deactivating", "stop-sigterm", inspectPID, nodeInvocation, "mixed", "success"))

		o := f.migrate(t, f.rendering(endpointA))
		mustEndpointRefusal(t, o, outcomeRefused, endpointReasonStopping)

		if n := f.calls(t, "show"); n != 4 {
			t.Errorf("%d shows, want one attempt", n)
		}
	})
}

// A node section whose effective name cannot be resolved (a TLS node named
// by a certificate that cannot be read) is could-not-tell, never a null
// name beside a successful answer.
func TestMigrateRefusesAnUnchangedAnswerWhoseNodeNameCannotBeResolved(t *testing.T) {
	tls := nodeTLSFixture(t, false)

	body, err := os.ReadFile(tls.configPath)
	mustOK(t, err)

	cert := regexp.MustCompile(`cert: (\S+)`).FindStringSubmatch(string(body))
	if cert == nil {
		t.Fatalf("no cert path in\n%s", body)
	}

	f := newEndpointFixture(t)
	f.writeConfig(t, string(body))
	f.setNode(t, "inactive", "dead", 0, "", "mixed")
	mustOK(t, os.Chmod(cert[1], 0))

	o := f.migrate(t, string(body))
	mustEndpointRefusal(t, o, outcomeUnknown, endpointReasonConfig)

	if !strings.Contains(o.str("why"), "effective name") {
		t.Errorf("why %q", o.str("why"))
	}
}

// A pre-R probe the bound ended is no probe: the diagnostic's words in a
// prefix do not make the process pre-R.
func TestMigrateDoesNotReadAnIncompleteProbeAsPreR(t *testing.T) {
	prevTimeout, prevDelay := guardCommandTimeout, guardWaitDelay
	guardCommandTimeout, guardWaitDelay = 300*time.Millisecond, 200*time.Millisecond

	t.Cleanup(func() { guardCommandTimeout, guardWaitDelay = prevTimeout, prevDelay })

	f := newEndpointFixture(t)
	mustOK(t, os.Remove(f.recordPath))

	script := "#!/bin/sh\necho 'billet: unknown converge-guard command \"prepare\"' >&2\nexec sleep 30\n"
	writeFile(t, installedBinary, script, 0o755)
	writeFile(t, filepath.Join(f.procDir, strconv.Itoa(inspectPID), "image"), script, 0o755)

	o := f.migrate(t, f.rendering(endpointB), "--dry-run")
	mustEndpointRefusal(t, o, outcomeUnknown, endpointReasonRecord)

	if !strings.Contains(o.str("why"), "within its bound") {
		t.Errorf("why %q", o.str("why"))
	}
}
