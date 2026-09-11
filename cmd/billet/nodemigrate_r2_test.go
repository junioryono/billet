package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
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
		{"a state this does not judge", nodeUnitBody("maintenance", "running", inspectPID, nodeInvocation, "mixed", "success"),
			outcomeUnknown, endpointReasonProcess},
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

	// The certificate's read fails through the public-read seam (a chmod
	// proves nothing under root).
	prev := readPublicFile
	readPublicFile = func(path string) ([]byte, error) {
		if path == cert[1] {
			return nil, &os.PathError{Op: "open", Path: path, Err: syscall.EACCES}
		}

		return prev(path)
	}

	t.Cleanup(func() { readPublicFile = prev })

	o := f.migrate(t, string(body))
	mustEndpointRefusal(t, o, outcomeUnknown, endpointReasonConfig)

	// The identity comparison reads the certificate first and answers the
	// same could-not-tell; either wording names the failed read.
	if why := o.str("why"); !strings.Contains(why, "effective name") && !strings.Contains(why, "node identity") {
		t.Errorf("why %q", why)
	}
}

// A pre-R node without a registration directory at all (a release before
// the record has none) is judged pre-R by its image and the managed binary,
// and the record wait is bounded so nothing observed after it counts.
func TestMigrateJudgesPreRWithoutARegistrationDirectory(t *testing.T) {
	f := newEndpointFixture(t)
	f.preRBinary(t)
	mustOK(t, os.RemoveAll(f.recordDir))

	o := f.migrate(t, f.rendering(endpointB), "--dry-run")
	mustEndpointOutcome(t, o, outcomeReported)

	if o.str("record") != recordAbsentPreR {
		t.Errorf("record %q", o.str("record"))
	}

	t.Run("a record appearing during the sleep that ends at the deadline is not judged", func(t *testing.T) {
		f := newEndpointFixture(t)
		f.rBinary(t)
		mustOK(t, os.Remove(f.recordPath))

		// The sleep between brackets is longer than the wait, so the first
		// absent bracket is followed by one sleep that ends at the deadline;
		// the record published during it must not be read by a bracket
		// started at the deadline.
		prev := endpointPoll
		endpointPoll = time.Second

		t.Cleanup(func() { endpointPoll = prev })

		f.onRecordRead(t, 1, func() {
			f.writeRecord(t, f.record(map[string]any{"deployment": f.deployment, "endpoint": canonicalA}))
		})

		o := f.migrate(t, f.rendering(endpointB), "--dry-run", "--wait", "100ms")
		mustEndpointRefusal(t, o, outcomeUnknown, endpointReasonRecord)
	})

	t.Run("a record naming a malformed endpoint under an old invocation never reaches the probe", func(t *testing.T) {
		f := newEndpointFixture(t)
		f.preRBinary(t)
		f.writeRecord(t, f.record(map[string]any{"deployment": f.deployment, "endpoint": "not-an-endpoint",
			"invocation_id": newInvocation}))

		o := f.migrate(t, f.rendering(endpointB), "--dry-run")
		mustEndpointRefusal(t, o, outcomeUnknown, endpointReasonRecord)

		if !strings.Contains(o.str("why"), "canonical") {
			t.Errorf("why %q", o.str("why"))
		}
	})

	t.Run("two configured names that disagree on a host without an identity", func(t *testing.T) {
		f := newEndpointFixture(t)
		mustOK(t, os.Remove(filepath.Join(f.nodeState, "deployment-id")))
		f.setNode(t, "inactive", "dead", 0, "", "mixed")

		o := f.migrate(t, strings.Replace(f.rendering(endpointA), "name: node-a", "name: node-b", 1))
		mustEndpointRefusal(t, o, outcomeRefused, endpointReasonDesired)
	})
}

// A configuration that is a symlink is closed through its target: unchanged
// when the target is the file opened, could-not-tell when retargeted.
func TestMigrateClosesASymlinkedConfigurationThroughItsTarget(t *testing.T) {
	f := newEndpointFixture(t)
	target := f.configPath + ".real"
	mustOK(t, os.Rename(f.configPath, target))
	mustOK(t, os.Symlink(target, f.configPath))

	o := f.migrate(t, f.rendering(endpointA))
	mustEndpointOutcome(t, o, outcomeUnchanged)

	t.Run("retargeted under the judgement", func(t *testing.T) {
		other := f.configPath + ".other"
		writeFile(t, other, f.nodeOnlyConfig(), 0o644)
		f.onRecordRead(t, 1, func() {
			mustOK(t, os.Remove(f.configPath))
			mustOK(t, os.Symlink(other, f.configPath))
		})

		o := f.migrate(t, f.rendering(endpointA))
		mustEndpointRefusal(t, o, outcomeUnknown, endpointReasonConfig)
	})
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

// A retry of a bracket the unit moved under is a bracket too, and none
// starts after the wait: a record published while the first bracket ran
// past the deadline is not read by a second.
func TestMigrateRetriesNoBracketAfterTheWait(t *testing.T) {
	t.Helper()
	f := newEndpointFixture(t)
	f.rBinary(t)
	mustOK(t, os.Remove(f.recordPath))

	f.onRecordRead(t, 1, func() {
		n := f.calls(t, "show")
		f.showsBetween(t, n, n+1, nodeUnitBody("active", "running", 9999, newInvocation, "mixed", "success"))
		time.Sleep(120 * time.Millisecond)
		f.writeRecord(t, f.record(map[string]any{"deployment": f.deployment, "endpoint": canonicalA}))
	})

	o := f.migrate(t, f.rendering(endpointB), "--dry-run", "--wait", "50ms")
	mustEndpointRefusal(t, o, outcomeUnknown, endpointReasonUnit)

	if !strings.Contains(o.str("why"), "the wait ended while the unit moved") {
		t.Errorf("why %q", o.str("why"))
	}
}

// A rendering whose identity cannot be read (no name, and a certificate
// that does not read) establishes nothing, and the answer is could-not-tell
// rather than a plan over an unexamined identity.
func TestMigrateAnUnreadableRenderingIdentityIsUnknown(t *testing.T) {
	t.Helper()
	f := newEndpointFixture(t)

	dir := t.TempDir()
	tls := "  tls:\n    cert: " + dir + "\n    key: " + dir + "/node.key\n    ca: " + dir + "/ca.crt\n"
	base := f.rendering("10.9.0.2:7719")

	if strings.Count(base, "  name: node-a\n") != 1 {
		t.Fatalf("the rendering does not name the node once:\n%s", base)
	}

	o := f.migrate(t, strings.Replace(base, "  name: node-a\n", tls, 1), "--dry-run")
	mustEndpointRefusal(t, o, outcomeUnknown, endpointReasonConfig)

	if !strings.Contains(o.str("why"), "rendering configuration's node identity") {
		t.Errorf("why %q", o.str("why"))
	}
}

// A rendering that names another node is refused even when its deployment
// is not minted yet: the resolved names are compared before the absence
// shortcut, and only the deployment comparison is skipped.
func TestMigrateRefusesARenameWhoseDeploymentIsUnminted(t *testing.T) {
	tls := nodeTLSFixture(t, false)

	body, err := os.ReadFile(tls.configPath)
	mustOK(t, err)

	f := newEndpointFixture(t)
	f.writeConfig(t, string(body))
	f.setNode(t, "inactive", "dead", 0, "", "mixed")

	rendering := strings.Replace(f.rendering(endpointB), "  name: node-a\n", "  name: node-b\n", 1)
	rendering = regexp.MustCompile(`state_dir: \S+`).ReplaceAllString(rendering, "state_dir: "+t.TempDir())

	o := f.migrate(t, rendering, "--dry-run")
	mustEndpointRefusal(t, o, outcomeRefused, endpointReasonDesired)

	if !strings.Contains(o.str("why"), "names the node") {
		t.Errorf("why %q", o.str("why"))
	}
}

// The migration's close requires a running process: a started unit found
// inactive with the sampled pid still on it is could-not-tell, never
// migrated.
func TestMigrateCloseRequiresARunningProcess(t *testing.T) {
	f := newEndpointFixture(t)
	f.installB(t)
	f.afterStop(t, "inactive", "dead", "success")
	f.afterStart(t)
	f.recordAfterStart(t, canonicalB)

	// The second record read is the first after the start; from the
	// bracket's closing observation on every show answers inactive with the
	// started pid, so the bracket agrees on the process and the migration's
	// close finds it stopped.
	f.onRecordRead(t, 2, func() {
		f.afterShows(t, f.calls(t, "show")+1, nodeUnitBody("inactive", "dead", inspectPID+1, newInvocation, "mixed", "success"))
	})

	o := f.migrate(t, f.rendering(endpointB), "--wait", "3s")
	mustEndpointRefusal(t, o, outcomeUnknown, endpointReasonProcess)

	if o.str("state") != stateStarted || !strings.Contains(o.str("why"), "moved or stopped") {
		t.Errorf("state %q why %q", o.str("state"), o.str("why"))
	}
}

// A rendering whose explicit name the certificate contradicts is refused:
// the node's own startup refuses such a configuration, so a migration that
// installed it would stop a working node for one that never starts.
func TestMigrateRefusesANameTheCertificateContradicts(t *testing.T) {
	tls := nodeTLSFixture(t, true)

	body, err := os.ReadFile(tls.configPath)
	mustOK(t, err)

	// The installed configuration names node-b too, so the configured names
	// agree and only the certificate disagrees.
	f := newEndpointFixture(t)
	f.writeConfig(t, strings.Replace(f.nodeOnlyConfig(), "  name: node-a\n", "  name: node-b\n", 1))

	rendering := strings.Replace(string(body), "  name: node-a\n", "  name: node-b\n", 1)
	if rendering == string(body) {
		t.Fatalf("the TLS configuration does not name node-a:\n%s", body)
	}

	o := f.migrate(t, rendering, "--dry-run")
	mustEndpointRefusal(t, o, outcomeRefused, endpointReasonDesired)

	if !strings.Contains(o.str("why"), "was issued for") {
		t.Errorf("why %q", o.str("why"))
	}
}

// A pre-stop observation that lacks a property is could-not-tell, never a
// refusal: an unanswered KillMode or LoadState is not a disallowed value.
func TestMigratePreStopObservationMustBeComplete(t *testing.T) {
	for name, body := range map[string]string{
		"no KillMode":  nodeUnitBody("active", "running", inspectPID, nodeInvocation, "", "success"),
		"no LoadState": strings.Replace(nodeUnitBody("active", "running", inspectPID, nodeInvocation, "mixed", "success"), "LoadState=loaded\n", "LoadState=\n", 1),
		"no Result":    strings.Replace(nodeUnitBody("active", "running", inspectPID, nodeInvocation, "mixed", "success"), "Result=success\n", "Result=\n", 1),
		"no SubState":  strings.Replace(nodeUnitBody("active", "running", inspectPID, nodeInvocation, "mixed", "success"), "SubState=running\n", "SubState=\n", 1),
	} {
		t.Run(name, func(t *testing.T) {
			f := newEndpointFixture(t)
			f.installB(t)
			f.afterStop(t, "inactive", "dead", "success")
			f.afterStart(t)
			f.recordAfterStart(t, canonicalB)
			f.afterShows(t, showsBeforeTheStop, body)

			o := f.migrate(t, f.rendering(endpointB))
			mustEndpointRefusal(t, o, outcomeUnknown, endpointReasonUnit)

			if !strings.Contains(o.str("why"), "answered no") || f.calls(t, "stop") != 0 {
				t.Errorf("why %q stops %d", o.str("why"), f.calls(t, "stop"))
			}
		})
	}
}

// A contradicted rendering is refused on a first start too, where no
// installed node exists to compare it with.
func TestMigrateRefusesAContradictedRenderingOnAFirstStart(t *testing.T) {
	tls := nodeTLSFixture(t, true)

	body, err := os.ReadFile(tls.configPath)
	mustOK(t, err)

	f := newEndpointFixture(t)
	f.writeConfig(t, f.serverOnly())
	f.setNode(t, "inactive", "dead", 0, "", "mixed")

	o := f.migrate(t, strings.Replace(string(body), "  name: node-a\n", "  name: node-b\n", 1), "--dry-run")
	mustEndpointRefusal(t, o, outcomeRefused, endpointReasonDesired)

	if !strings.Contains(o.str("why"), "was issued for") {
		t.Errorf("why %q", o.str("why"))
	}
}

// A proven contradiction on the rendering's side is refused even when the
// installed identity could not be read: both contradictions are judged
// before either could-not-tell.
func TestMigrateRefusesAContradictionBesideAnUnreadableInstalledIdentity(t *testing.T) {
	installedTLS, renderedTLS := nodeTLSFixture(t, true), nodeTLSFixture(t, true)

	installedBody, err := os.ReadFile(installedTLS.configPath)
	mustOK(t, err)

	renderedBody, err := os.ReadFile(renderedTLS.configPath)
	mustOK(t, err)

	cert := regexp.MustCompile(`cert: (\S+)`).FindStringSubmatch(string(installedBody))
	if cert == nil {
		t.Fatalf("no cert path in\n%s", installedBody)
	}

	// Both configurations name node-b; the installed certificate cannot be
	// read, the rendering's was issued for node-a.
	f := newEndpointFixture(t)
	f.writeConfig(t, strings.Replace(string(installedBody), "  name: node-a\n", "  name: node-b\n", 1))
	f.setNode(t, "inactive", "dead", 0, "", "mixed")

	prev := readPublicFile
	readPublicFile = func(path string) ([]byte, error) {
		if path == cert[1] {
			return nil, &os.PathError{Op: "open", Path: path, Err: syscall.EACCES}
		}

		return prev(path)
	}

	t.Cleanup(func() { readPublicFile = prev })

	o := f.migrate(t, strings.Replace(string(renderedBody), "  name: node-a\n", "  name: node-b\n", 1), "--dry-run")
	mustEndpointRefusal(t, o, outcomeRefused, endpointReasonDesired)

	if !strings.Contains(o.str("why"), "rendering configuration") || !strings.Contains(o.str("why"), "was issued for") {
		t.Errorf("why %q", o.str("why"))
	}
}

// A first start over a rendering whose identity cannot be read (its
// certificate does not read, with or without an explicit name) is
// could-not-tell, never a reported plan.
func TestMigrateFirstStartOverAnUnreadIdentityIsUnknown(t *testing.T) {
	for _, withName := range []bool{true, false} {
		t.Run(map[bool]string{true: "an explicit name", false: "no name"}[withName], func(t *testing.T) {
			tls := nodeTLSFixture(t, withName)

			body, err := os.ReadFile(tls.configPath)
			mustOK(t, err)

			cert := regexp.MustCompile(`cert: (\S+)`).FindStringSubmatch(string(body))
			if cert == nil {
				t.Fatalf("no cert path in\n%s", body)
			}

			f := newEndpointFixture(t)
			f.writeConfig(t, f.serverOnly())
			f.setNode(t, "inactive", "dead", 0, "", "mixed")

			prev := readPublicFile
			readPublicFile = func(path string) ([]byte, error) {
				if path == cert[1] {
					return nil, &os.PathError{Op: "open", Path: path, Err: syscall.EACCES}
				}

				return prev(path)
			}

			t.Cleanup(func() { readPublicFile = prev })

			o := f.migrate(t, string(body), "--dry-run")
			mustEndpointRefusal(t, o, outcomeUnknown, endpointReasonConfig)

			if !strings.Contains(o.str("why"), "node identity") {
				t.Errorf("why %q", o.str("why"))
			}
		})
	}
}

// The migration's close judges only the states it knows: a started unit in
// a state this does not judge is could-not-tell, never migrated.
func TestMigrateCloseRefusesAStateItDoesNotJudge(t *testing.T) {
	f := newEndpointFixture(t)
	f.installB(t)
	f.afterStop(t, "inactive", "dead", "success")
	f.afterStart(t)
	f.recordAfterStart(t, canonicalB)

	f.onRecordRead(t, 2, func() {
		f.afterShows(t, f.calls(t, "show")+1, nodeUnitBody("maintenance", "running", inspectPID+1, newInvocation, "mixed", "success"))
	})

	o := f.migrate(t, f.rendering(endpointB), "--wait", "3s")
	mustEndpointRefusal(t, o, outcomeUnknown, endpointReasonProcess)

	if o.str("state") != stateStarted || !strings.Contains(o.str("why"), "does not judge") {
		t.Errorf("state %q why %q", o.str("state"), o.str("why"))
	}
}

// Two configured names that differ are refused only once both identities
// read: an installed certificate that cannot be read is could-not-tell
// first, so a refusal never speaks over an unexamined identity.
func TestMigrateReadsBothIdentitiesBeforeComparingConfiguredNames(t *testing.T) {
	tls := nodeTLSFixture(t, true)

	body, err := os.ReadFile(tls.configPath)
	mustOK(t, err)

	cert := regexp.MustCompile(`cert: (\S+)`).FindStringSubmatch(string(body))
	if cert == nil {
		t.Fatalf("no cert path in\n%s", body)
	}

	f := newEndpointFixture(t)
	f.writeConfig(t, string(body))
	f.setNode(t, "inactive", "dead", 0, "", "mixed")

	prev := readPublicFile
	readPublicFile = func(path string) ([]byte, error) {
		if path == cert[1] {
			return nil, &os.PathError{Op: "open", Path: path, Err: syscall.EACCES}
		}

		return prev(path)
	}

	t.Cleanup(func() { readPublicFile = prev })

	o := f.migrate(t, strings.Replace(f.rendering(endpointB), "  name: node-a\n", "  name: node-b\n", 1), "--dry-run")
	mustEndpointRefusal(t, o, outcomeUnknown, endpointReasonConfig)

	if !strings.Contains(o.str("why"), "installed configuration's node identity") {
		t.Errorf("why %q", o.str("why"))
	}
}

// emptyDeploymentCertPEM is a certificate for node-a whose one Organization
// value is empty: a shape the node's startup refuses as no deployment.
func emptyDeploymentCertPEM(t *testing.T) string {
	t.Helper()

	return certPEM(t, "node-a", "")
}

// certPEM is a self-signed certificate with the given CommonName and one
// Organization value.
func certPEM(t *testing.T, cn, org string) string {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	mustOK(t, err)

	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: cn, Organization: []string{org}},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	mustOK(t, err)

	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

// A first start over a certificate whose deployment is empty is could-not-
// tell: the node's startup refuses that identity, and a plan over it would
// stop nothing but start nothing either.
func TestMigrateFirstStartOverAnEmptyDeploymentIsUnknown(t *testing.T) {
	tls := nodeTLSFixture(t, false)

	body, err := os.ReadFile(tls.configPath)
	mustOK(t, err)

	cert := regexp.MustCompile(`cert: (\S+)`).FindStringSubmatch(string(body))
	if cert == nil {
		t.Fatalf("no cert path in\n%s", body)
	}

	writeFile(t, cert[1], emptyDeploymentCertPEM(t), 0o644)

	f := newEndpointFixture(t)
	f.writeConfig(t, f.serverOnly())
	f.setNode(t, "inactive", "dead", 0, "", "mixed")

	o := f.migrate(t, string(body), "--dry-run")
	mustEndpointRefusal(t, o, outcomeUnknown, endpointReasonConfig)

	if !strings.Contains(o.str("why"), "Organization") {
		t.Errorf("why %q", o.str("why"))
	}
}

// A certificate with no CommonName beside an omitted node.name names no
// node: the startup refuses that, and a first start over it is
// could-not-tell.
func TestMigrateFirstStartOverANamelessCertificateIsUnknown(t *testing.T) {
	tls := nodeTLSFixture(t, false)

	body, err := os.ReadFile(tls.configPath)
	mustOK(t, err)

	cert := regexp.MustCompile(`cert: (\S+)`).FindStringSubmatch(string(body))
	if cert == nil {
		t.Fatalf("no cert path in\n%s", body)
	}

	writeFile(t, cert[1], certPEM(t, "", "dep-1234"), 0o644)

	f := newEndpointFixture(t)
	f.writeConfig(t, f.serverOnly())
	f.setNode(t, "inactive", "dead", 0, "", "mixed")

	o := f.migrate(t, string(body), "--dry-run")
	mustEndpointRefusal(t, o, outcomeUnknown, endpointReasonConfig)

	if !strings.Contains(o.str("why"), "CommonName") {
		t.Errorf("why %q", o.str("why"))
	}
}

// The bracket's closing observation is read as its opening one: a state
// this does not judge at the close is could-not-tell, never a usable record
// that a migration verdict is drawn from.
func TestBracketCloseRefusesAStateItDoesNotJudge(t *testing.T) {
	f := newEndpointFixture(t)
	// Only the bracket's closing show (the third, after the judgement's own
	// opening observation and the bracket's) answers the unknown state;
	// every later observation is ordinary, so nothing but the bracket's own
	// reading of its close can refuse.
	f.showsBetween(t, 2, 3, nodeUnitBody("maintenance", "running", inspectPID, nodeInvocation, "mixed", "success"))

	o := f.migrate(t, f.rendering(endpointB), "--dry-run")
	mustEndpointRefusal(t, o, outcomeUnknown, endpointReasonUnit)

	if !strings.Contains(o.str("why"), "at the close of the read") {
		t.Errorf("why %q", o.str("why"))
	}
}
