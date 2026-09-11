package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The migration's remaining fixtures (fixtures-c4b.md, section A): the
// pre-R judgement, the server-only and absent-configuration branches, the
// record wait before and after the start, the closing check of a migrated
// answer, the inputs, the source, and the committed fixtures.

// installedSHA is the digest of the installed configuration's bytes.
func (f *endpointFixture) installedSHA(t *testing.T) string {
	t.Helper()

	body, err := os.ReadFile(f.configPath)
	mustOK(t, err)

	sum := sha256.Sum256(body)

	return hex.EncodeToString(sum[:])
}

// preRBinary makes the managed binary and the running process's image one
// script that answers "unknown command" to the preparation's dry run: a
// release before the guard.
func (f *endpointFixture) preRBinary(t *testing.T) {
	t.Helper()

	script := "#!/bin/sh\necho 'billet: unknown converge-guard command \"prepare\"' >&2\nexit 2\n"
	writeFile(t, installedBinary, script, 0o755)
	writeFile(t, filepath.Join(f.procDir, strconv.Itoa(inspectPID), "image"), script, 0o755)
}

// serverOnly is a valid server-only configuration.
func (f *endpointFixture) serverOnly() string {
	return "server:\n  listen: 127.0.0.1:7717\n  state_dir: " + f.stateDir + "\n  max_vcpu: 8\n  max_memory: 32GiB\n" +
		"github:\n  org: acme\n  app_id: 1\n  installation_id: 2\n  private_key_path: " + filepath.Join(f.dir, "app.pem") +
		"\ntiers:\n  - label: billet-2vcpu\n    provider: docker\n    vcpu: 2\n    memory: 8GiB\n    image: ubuntu:24.04\n"
}

// notFound makes the node unit one systemd does not know.
func (f *endpointFixture) notFound(t *testing.T) {
	t.Helper()
	writeFile(t, f.unitFile(nodeUnit), strings.Replace(nodeUnitBody("inactive", "dead", 0, "", "mixed", "success"),
		"LoadState=loaded", "LoadState=not-found", 1), 0o644)
}

// onRecordRead runs fn once, after the nth record read of a command and
// before the bracket's closing observation.
func (f *endpointFixture) onRecordRead(t *testing.T, nth int, fn func()) {
	t.Helper()

	prev := inspectAfterRecordRead
	n := 0
	inspectAfterRecordRead = func() {
		n++
		if n == nth {
			fn()
		}
	}

	t.Cleanup(func() { inspectAfterRecordRead = prev })
}

// A3, continued: the dry run's branches.
func TestMigrateDryRunJudgesEveryBranch(t *testing.T) {
	t.Run("a unit systemd does not know", func(t *testing.T) {
		t.Helper()
		f := newEndpointFixture(t)
		f.notFound(t)

		o := f.migrate(t, f.rendering(endpointB), "--dry-run")
		mustEndpointOutcome(t, o, outcomeReported)

		if !o.boolean("planned") || o.str("record") != recordNone || asMap(o.doc["unit"])["load_state"] != "not-found" {
			t.Errorf("answer %v", o.doc)
		}
	})

	// A3h: the running pre-R node, in both forms.
	for _, c := range []struct {
		name     string
		rendered string
		planned  bool
	}{{"planned", endpointB, true}, {"unplanned", endpointA, false}} {
		t.Run("a pre-R node, "+c.name, func(t *testing.T) {
			t.Helper()
			f := newEndpointFixture(t)
			f.preRBinary(t)
			mustOK(t, os.Remove(f.recordPath))

			o := f.migrate(t, f.rendering(c.rendered), "--dry-run")
			mustEndpointOutcome(t, o, outcomeReported)

			if o.str("record") != recordAbsentPreR || o.doc["effective"] != nil || o.boolean("planned") != c.planned ||
				o.str("from") != canonicalA {
				t.Errorf("answer %v", o.doc)
			}

			if f.calls(t, "stop") != 0 {
				t.Error("a dry run over a pre-R node stopped it")
			}
		})
	}

	t.Run("a pre-R node in the action", func(t *testing.T) {
		t.Helper()
		f := newEndpointFixture(t)
		f.preRBinary(t)
		f.installB(t)
		mustOK(t, os.Remove(f.recordPath))

		o := f.migrate(t, f.rendering(endpointB))
		mustEndpointRefusal(t, o, outcomeRefused, endpointReasonPreR)

		if f.calls(t, "stop") != 0 {
			t.Error("a refused pre-R node was stopped")
		}
	})

	// A3o, A3p: a pre-R process whose configured endpoint cannot be read.
	for _, c := range []struct {
		name  string
		plant func(t *testing.T, f *endpointFixture)
	}{
		{"beside an absent configuration", func(t *testing.T, f *endpointFixture) {
			t.Helper()
			mustOK(t, os.Remove(f.configPath))
		}},
		{"beside a server-only configuration", func(t *testing.T, f *endpointFixture) {
			t.Helper()
			f.writeConfig(t, f.serverOnly())
		}},
	} {
		t.Run("a pre-R node "+c.name, func(t *testing.T) {
			t.Helper()
			f := newEndpointFixture(t)
			f.preRBinary(t)
			mustOK(t, os.Remove(f.recordPath))
			rendering := f.rendering(endpointB)
			c.plant(t, f)

			for _, extra := range [][]string{{"--dry-run"}, nil} {
				o := f.migrate(t, rendering, extra...)
				mustEndpointRefusal(t, o, outcomeUnknown, endpointReasonRecord)

				if !strings.Contains(o.str("why"), "restore") || f.calls(t, "stop") != 0 {
					t.Errorf("why %q calls %v", o.str("why"), f.systemctlCalls(t))
				}
			}
		})
	}

	// A3k, A3l, A3m: a leftover node beside a server-only installation; a
	// removal; a removal over a node still stopping.
	t.Run("a leftover node beside a server-only installation", func(t *testing.T) {
		t.Helper()
		f := newEndpointFixture(t)
		f.writeConfig(t, f.serverOnly())

		o := f.migrate(t, f.rendering(endpointB), "--dry-run")
		mustEndpointOutcome(t, o, outcomeReported)

		if !o.boolean("planned") || o.boolean("first_start") || o.str("effective") != canonicalA || o.str("record") != recordCurrent {
			t.Errorf("answer %v", o.doc)
		}
	})

	t.Run("a removal over a running node", func(t *testing.T) {
		t.Helper()
		f := newEndpointFixture(t)

		o := f.migrate(t, f.serverOnly(), "--dry-run")
		mustEndpointOutcome(t, o, outcomeReported)

		if !o.boolean("node_removed") || o.boolean("planned") || o.str("record") != recordCurrent ||
			o.str("effective") != canonicalA || o.doc["to"] != nil {
			t.Errorf("answer %v", o.doc)
		}
	})

	t.Run("a removal over a node still stopping", func(t *testing.T) {
		t.Helper()
		f := newEndpointFixture(t)
		f.setNode(t, "deactivating", "stop-sigterm", inspectPID, nodeInvocation, "mixed")

		o := f.migrate(t, f.serverOnly(), "--dry-run")
		mustEndpointRefusal(t, o, outcomeRefused, endpointReasonStopping)
	})

	// A3n: both configurations server-only.
	t.Run("two server-only configurations", func(t *testing.T) {
		t.Helper()
		for _, c := range []struct {
			name   string
			plant  func(t *testing.T, f *endpointFixture)
			record string
			load   string
		}{
			{"a loaded inactive unit", func(t *testing.T, f *endpointFixture) {
				t.Helper()
				f.setNode(t, "inactive", "dead", 0, "", "mixed")
			}, recordNone, "loaded"},
			{"a unit systemd does not know", func(t *testing.T, f *endpointFixture) {
				t.Helper()
				f.notFound(t)
			}, recordNone, "not-found"},
			{"an active unit with a process", func(*testing.T, *endpointFixture) {}, recordUnread, "loaded"},
		} {
			t.Run(c.name, func(t *testing.T) {
				t.Helper()
				f := newEndpointFixture(t)
				f.writeConfig(t, f.serverOnly())
				c.plant(t, f)

				o := f.migrate(t, f.serverOnly(), "--dry-run")
				mustEndpointOutcome(t, o, outcomeReported)

				if o.boolean("planned") || o.str("record") != c.record || asMap(o.doc["unit"])["load_state"] != c.load ||
					o.doc["from"] != nil || o.doc["to"] != nil || o.doc["node"] != nil {
					t.Errorf("answer %v", o.doc)
				}

				if f.ops["read"] != 0 {
					t.Errorf("the record was read %d times with no endpoint to judge", f.ops["read"])
				}
			})
		}
	})
}

// A8b, A9e, A9f, A10b, A10c: the record waits.
func TestMigrateRecordWaitsBeforeAndAfterTheStart(t *testing.T) {
	t.Run("no InvocationID after the start", func(t *testing.T) {
		t.Helper()
		f := newEndpointFixture(t)
		f.installB(t)
		f.afterStop(t, "inactive", "dead", "success")
		writeFile(t, f.unitFile(nodeUnit+".after-start"), strings.Replace(nodeUnitBody("active", "running", inspectPID+1,
			newInvocation, "mixed", "success"), "InvocationID="+newInvocation+"\n", "", 1), 0o644)

		o := f.migrate(t, f.rendering(endpointB))
		mustEndpointRefusal(t, o, outcomeUnknown, endpointReasonUnproved)

		if o.str("state") != stateStartedNoRecord || !strings.Contains(o.str("why"), "InvocationID") {
			t.Errorf("why %q state %q", o.str("why"), o.str("state"))
		}
	})

	t.Run("another deployment after the start", func(t *testing.T) {
		t.Helper()
		f := newEndpointFixture(t)
		f.installB(t)
		f.afterStop(t, "inactive", "dead", "success")
		f.afterStart(t)
		f.recordAfterStart(t, canonicalB)
		// The staged record names another deployment.
		body, err := os.ReadFile(f.unitFile("record.after-start"))
		mustOK(t, err)
		writeFile(t, f.unitFile("record.after-start"), strings.ReplaceAll(string(body), f.deployment, regDeployment2), 0o600)

		o := f.migrate(t, f.rendering(endpointB))
		mustEndpointRefusal(t, o, outcomeUnknown, endpointReasonUnproved)

		if o.str("state") != stateStartedNoRecord || !strings.Contains(o.str("why"), "cannot be judged") {
			t.Errorf("why %q state %q", o.str("why"), o.str("state"))
		}
	})

	t.Run("the record appearing after the start", func(t *testing.T) {
		t.Helper()
		f := newEndpointFixture(t)
		f.installB(t)
		f.afterStop(t, "inactive", "dead", "success")
		f.afterStart(t)

		// The fake's start publishes nothing; the record arrives a little
		// after the start was recorded.
		f.afterCall(t, "start", 40*time.Millisecond, func() {
			f.writeRecord(t, f.record(map[string]any{"deployment": f.deployment, "endpoint": canonicalB,
				"invocation_id": newInvocation, "incarnation": newIncarnation}))
		})

		o := f.migrate(t, f.rendering(endpointB), "--wait", "3s")
		mustEndpointOutcome(t, o, outcomeMigrated)
	})

	t.Run("the record appearing before the decision", func(t *testing.T) {
		t.Helper()
		f := newEndpointFixture(t)
		f.rBinary(t)
		mustOK(t, os.Remove(f.recordPath))

		// The record is published after the first read found none, so the
		// wait is exercised and the next bracket decides from it.
		f.onRecordRead(t, 1, func() {
			f.writeRecord(t, f.record(map[string]any{"deployment": f.deployment, "endpoint": canonicalA}))
		})

		o := f.migrate(t, f.rendering(endpointB), "--dry-run", "--wait", "3s")
		mustEndpointOutcome(t, o, outcomeReported)

		if !o.boolean("planned") || o.str("effective") != canonicalA {
			t.Errorf("answer %v", o.doc)
		}
	})

	t.Run("a record naming another node before the decision", func(t *testing.T) {
		t.Helper()
		f := newEndpointFixture(t)
		f.writeRecord(t, f.record(map[string]any{"deployment": f.deployment, "endpoint": canonicalA, "node": "node-b"}))

		started := time.Now()

		o := f.migrate(t, f.rendering(endpointB), "--dry-run", "--wait", "3s")
		mustEndpointRefusal(t, o, outcomeUnknown, endpointReasonRecord)

		if time.Since(started) > 2*time.Second {
			t.Error("a foreign record was waited on")
		}
	})
}

// A14i: a migrated answer is never retried; a process that moved or a unit
// stopping at the close is could-not-tell with the state started.
func TestMigrateNeverRetriesAMigratedAnswer(t *testing.T) {
	for _, c := range []struct{ name, body string }{
		{"a process that moved", nodeUnitBody("active", "running", 9999, newInvocation, "mixed", "success")},
		{"a unit stopping", nodeUnitBody("deactivating", "stop-sigterm", inspectPID+1, newInvocation, "mixed", "success")},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Helper()
			f := newEndpointFixture(t)
			f.installB(t)
			f.afterStop(t, "inactive", "dead", "success")
			f.afterStart(t)
			f.recordAfterStart(t, canonicalB)

			// After the post-start record read: the bracket's closing show
			// still agrees, the answer's closing observation does not.
			f.onRecordRead(t, 2, func() { f.afterShows(t, f.calls(t, "show")+1, c.body) })

			o := f.migrate(t, f.rendering(endpointB))
			mustEndpointRefusal(t, o, outcomeUnknown, endpointReasonProcess)

			if o.str("state") != stateStarted || f.calls(t, "stop") != 1 {
				t.Errorf("state %q stops %d", o.str("state"), f.calls(t, "stop"))
			}
		})
	}
}

// A16, in the action: an absent configuration refuses, whatever the node
// says (the render precedes the action).
func TestMigrateActionRefusesAnAbsentConfiguration(t *testing.T) {
	f := newEndpointFixture(t)
	rendering := f.rendering(endpointB)
	mustOK(t, os.Remove(f.configPath))

	o := f.migrate(t, rendering)
	mustEndpointRefusal(t, o, outcomeRefused, endpointReasonConfig)

	if f.calls(t, "stop") != 0 {
		t.Error("an absent configuration stopped the node")
	}
}

// A16c: the closing examination failing after an initial absence.
func TestMigrateClosesAnAbsenceToo(t *testing.T) {
	f := newEndpointFixture(t)
	rendering := f.rendering(endpointB)
	mustOK(t, os.Remove(f.configPath))

	prev := closingStat
	closingStat = func(string) (os.FileInfo, error) { return nil, syscall.EACCES }

	t.Cleanup(func() { closingStat = prev })

	o := f.migrate(t, rendering, "--dry-run")
	mustEndpointRefusal(t, o, outcomeUnknown, endpointReasonConfig)
}

// A1e: an identity is never minted by the command; a node without one
// answers with a null deployment.
func TestMigrateNeverMintsAnIdentity(t *testing.T) {
	f := newEndpointFixture(t)
	mustOK(t, os.Remove(filepath.Join(f.nodeState, "deployment-id")))
	f.setNode(t, "inactive", "dead", 0, "", "mixed")

	o := f.migrate(t, f.rendering(endpointA))
	mustEndpointOutcome(t, o, outcomeUnchanged)

	if o.doc["deployment"] != nil || o.str("node") != "node-a" || o.str("endpoint") != canonicalA || o.str("record") != recordNone {
		t.Errorf("answer %v", o.doc)
	}

	if _, err := os.Lstat(filepath.Join(f.nodeState, "deployment-id")); err == nil {
		t.Error("the command minted an identity")
	}
}

// A11, continued: a FIFO at the configuration is could-not-tell without a
// wait; a negative wait and a missing --json refuse.
func TestMigrateInputsContinued(t *testing.T) {
	t.Run("a FIFO at the configuration", func(t *testing.T) {
		t.Helper()
		f := newEndpointFixture(t)
		rendering := f.rendering(endpointB)
		mustOK(t, os.Remove(f.configPath))
		mustOK(t, syscall.Mkfifo(f.configPath, 0o600))

		done := make(chan endpointOut, 1)
		go func() { done <- f.migrate(t, rendering, "--dry-run") }()

		select {
		case o := <-done:
			mustEndpointRefusal(t, o, outcomeUnknown, endpointReasonConfig)
		case <-time.After(5 * time.Second):
			t.Fatal("the configuration's open waited on the FIFO")
		}
	})

	t.Run("a negative wait", func(t *testing.T) {
		t.Helper()
		f := newEndpointFixture(t)

		o := f.migrate(t, f.rendering(endpointB), "--wait", "-1s")
		mustEndpointRefusal(t, o, outcomeRefused, endpointReasonCombination)
	})

	t.Run("without --json", func(t *testing.T) {
		t.Helper()
		f := newEndpointFixture(t)

		err := cmdNodeMigrate(t.Context(), []string{"--config", f.configPath, "--dry-run"})
		if err == nil || !strings.Contains(err.Error(), "--json") {
			t.Errorf("err %v", err)
		}
	})
}

// A13: the source. Every systemctl runs through lifeops; the stop is
// StopAndProve and nothing else; nothing execs.
func TestMigrateSourceRunsSystemctlThroughLifeopsOnly(t *testing.T) {
	for _, file := range []string{"nodemigrate.go", "nodeendpoint.go", "nodereceipt.go", "rolloutregistration.go"} {
		src, err := os.ReadFile(file)
		mustOK(t, err)

		if strings.Contains(string(src), `"os/exec"`) {
			t.Errorf("%s imports os/exec", file)
		}

		if strings.Contains(string(src), `"systemctl"`) || strings.Contains(string(src), "exec.Command") {
			t.Errorf("%s runs systemctl directly", file)
		}
	}

	src, err := os.ReadFile("nodemigrate.go")
	mustOK(t, err)

	if n := strings.Count(string(src), "StopAndProve("); n != 1 {
		t.Errorf("nodemigrate.go calls StopAndProve %d times, want 1", n)
	}

	if n := strings.Count(string(src), "StartAndProve("); n != 1 {
		t.Errorf("nodemigrate.go calls StartAndProve %d times, want 1", n)
	}
}

// A12: the fixtures the role's parser consumes are the command's own.
func TestTheMigrationFixturesAreTheCommandsOwn(t *testing.T) {
	prev := guardWaitDelay
	guardWaitDelay = 300 * time.Millisecond

	t.Cleanup(func() { guardWaitDelay = prev })

	happy := func(t *testing.T, f *endpointFixture) {
		t.Helper()
		f.installB(t)
		f.afterStop(t, "inactive", "dead", "success")
		f.afterStart(t)
		f.recordAfterStart(t, canonicalB)
	}

	shapes := map[string]func(t *testing.T, f *endpointFixture) endpointOut{
		"migrated": func(t *testing.T, f *endpointFixture) endpointOut {
			t.Helper()
			happy(t, f)

			return f.migrate(t, f.rendering(endpointB))
		},
		"unchanged": func(t *testing.T, f *endpointFixture) endpointOut {
			t.Helper()

			return f.migrate(t, f.rendering(endpointA))
		},
		"unchanged-stopped": func(t *testing.T, f *endpointFixture) endpointOut {
			t.Helper()
			f.setNode(t, "inactive", "dead", 0, "", "mixed")
			f.installB(t)

			return f.migrate(t, f.rendering(endpointB))
		},
		"unchanged-none": func(t *testing.T, f *endpointFixture) endpointOut {
			t.Helper()
			f.setNode(t, "inactive", "dead", 0, "", "mixed")

			return f.migrate(t, f.rendering(endpointA))
		},
		"unchanged-none-no-identity": func(t *testing.T, f *endpointFixture) endpointOut {
			t.Helper()
			mustOK(t, os.Remove(filepath.Join(f.nodeState, "deployment-id")))
			f.setNode(t, "inactive", "dead", 0, "", "mixed")

			return f.migrate(t, f.rendering(endpointA))
		},
		"unchanged-server-only-inactive": func(t *testing.T, f *endpointFixture) endpointOut {
			t.Helper()
			f.writeConfig(t, f.serverOnly())
			f.setNode(t, "inactive", "dead", 0, "", "mixed")

			return f.migrate(t, f.serverOnly())
		},
		"unchanged-server-only-not-found": func(t *testing.T, f *endpointFixture) endpointOut {
			t.Helper()
			f.writeConfig(t, f.serverOnly())
			f.notFound(t)

			return f.migrate(t, f.serverOnly())
		},
		"unchanged-node-removed": func(t *testing.T, f *endpointFixture) endpointOut {
			t.Helper()
			f.writeConfig(t, f.serverOnly())

			return f.migrate(t, f.serverOnly())
		},
		"unchanged-unread": func(t *testing.T, f *endpointFixture) endpointOut {
			t.Helper()
			f.writeConfig(t, f.serverOnly())

			return f.migrate(t, f.serverOnly())
		},
		"reported-planned": func(t *testing.T, f *endpointFixture) endpointOut {
			t.Helper()

			return f.migrate(t, f.rendering(endpointB), "--dry-run")
		},
		"reported-unplanned": func(t *testing.T, f *endpointFixture) endpointOut {
			t.Helper()

			return f.migrate(t, f.rendering(endpointA), "--dry-run")
		},
		"reported-planned-pre-r": func(t *testing.T, f *endpointFixture) endpointOut {
			t.Helper()
			f.preRBinary(t)
			mustOK(t, os.Remove(f.recordPath))

			return f.migrate(t, f.rendering(endpointB), "--dry-run")
		},
		"reported-unplanned-pre-r": func(t *testing.T, f *endpointFixture) endpointOut {
			t.Helper()
			f.preRBinary(t)
			mustOK(t, os.Remove(f.recordPath))

			return f.migrate(t, f.rendering(endpointA), "--dry-run")
		},
		"reported-first-start": func(t *testing.T, f *endpointFixture) endpointOut {
			t.Helper()
			f.writeConfig(t, f.serverOnly())
			f.setNode(t, "inactive", "dead", 0, "", "mixed")

			return f.migrate(t, f.rendering(endpointB), "--dry-run")
		},
		"reported-node-removed": func(t *testing.T, f *endpointFixture) endpointOut {
			t.Helper()

			return f.migrate(t, f.serverOnly(), "--dry-run")
		},
		"reported-server-only-inactive": func(t *testing.T, f *endpointFixture) endpointOut {
			t.Helper()
			f.writeConfig(t, f.serverOnly())
			f.setNode(t, "inactive", "dead", 0, "", "mixed")

			return f.migrate(t, f.serverOnly(), "--dry-run")
		},
		"reported-server-only-not-found": func(t *testing.T, f *endpointFixture) endpointOut {
			t.Helper()
			f.writeConfig(t, f.serverOnly())
			f.notFound(t)

			return f.migrate(t, f.serverOnly(), "--dry-run")
		},
		"reported-server-only-active": func(t *testing.T, f *endpointFixture) endpointOut {
			t.Helper()
			f.writeConfig(t, f.serverOnly())

			return f.migrate(t, f.serverOnly(), "--dry-run")
		},
		"reported-fresh": func(t *testing.T, f *endpointFixture) endpointOut {
			t.Helper()
			rendering := f.rendering(endpointB)
			mustOK(t, os.Remove(f.configPath))
			f.setNode(t, "inactive", "dead", 0, "", "mixed")

			return f.migrate(t, rendering, "--dry-run")
		},
		"refused-stopping": func(t *testing.T, f *endpointFixture) endpointOut {
			t.Helper()
			f.setNode(t, "deactivating", "stop-sigterm", inspectPID, nodeInvocation, "mixed")

			return f.migrate(t, f.rendering(endpointB), "--dry-run")
		},
		"refused-policy": func(t *testing.T, f *endpointFixture) endpointOut {
			t.Helper()
			f.setNode(t, "active", "running", inspectPID, nodeInvocation, "process")
			f.installB(t)

			return f.migrate(t, f.rendering(endpointB))
		},
		"refused-pre-r": func(t *testing.T, f *endpointFixture) endpointOut {
			t.Helper()
			f.preRBinary(t)
			f.installB(t)
			mustOK(t, os.Remove(f.recordPath))

			return f.migrate(t, f.rendering(endpointB))
		},
		"unknown-unproved-deadline": func(t *testing.T, f *endpointFixture) endpointOut {
			t.Helper()
			f.installB(t)
			f.stopMode(t, "hang")

			return f.migrate(t, f.rendering(endpointB), "--stop-timeout", "200ms")
		},
		"unknown-unproved-result": func(t *testing.T, f *endpointFixture) endpointOut {
			t.Helper()
			f.installB(t)
			f.afterStop(t, "failed", "failed", "timeout")

			return f.migrate(t, f.rendering(endpointB))
		},
		"unknown-started-no-record": func(t *testing.T, f *endpointFixture) endpointOut {
			t.Helper()
			f.installB(t)
			f.afterStop(t, "inactive", "dead", "success")
			f.afterStart(t)
			f.recordRemovedAtStart(t)

			return f.migrate(t, f.rendering(endpointB))
		},
		"unknown-record": func(t *testing.T, f *endpointFixture) endpointOut {
			t.Helper()
			f.rBinary(t)
			mustOK(t, os.Remove(f.recordPath))

			return f.migrate(t, f.rendering(endpointB), "--dry-run")
		},
		"unknown-record-pre-r": func(t *testing.T, f *endpointFixture) endpointOut {
			t.Helper()
			f.preRBinary(t)
			f.writeConfig(t, f.serverOnly())
			mustOK(t, os.Remove(f.recordPath))

			return f.migrate(t, f.rendering(endpointB), "--dry-run")
		},
		"unknown-config-changed": func(t *testing.T, f *endpointFixture) endpointOut {
			t.Helper()
			f.onRecordRead(t, 1, func() {
				tmp := f.configPath + ".new"
				writeFile(t, tmp, f.nodeOnlyConfig(), 0o644)
				mustOK(t, os.Rename(tmp, f.configPath))
			})

			return f.migrate(t, f.rendering(endpointB), "--dry-run")
		},
		"unknown-process": func(t *testing.T, f *endpointFixture) endpointOut {
			t.Helper()
			f.everyShow(t, 4, nodeUnitBody("active", "running", 9999, newInvocation, "mixed", "success"))

			return f.migrate(t, f.rendering(endpointA))
		},
	}

	for name, produce := range shapes {
		t.Run(name, func(t *testing.T) {
			t.Helper()
			f := newEndpointFixture(t)

			o := produce(t, f)

			out := strings.ReplaceAll(o.raw, f.deployment, strings.Repeat("d", 32))
			out = strings.ReplaceAll(out, f.configPath, "/etc/billet/billet.yaml")
			out = regexp.MustCompile(`"installed_sha256": "[0-9a-f]{64}"`).
				ReplaceAllString(out, `"installed_sha256": "`+strings.Repeat("ab", 32)+`"`)
			out = strings.ReplaceAll(out, f.recordPath, "/run/billet/registration/current")
			out = strings.ReplaceAll(out, f.dir, "/var/lib/billet-fixture")
			compareFixture(t, "node-migrate-endpoint", name, out)
		})
	}

	fixtureSetIs(t, "node-migrate-endpoint", fixtureNames(shapes))
}
