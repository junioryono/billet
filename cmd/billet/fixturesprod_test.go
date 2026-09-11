package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/rollout"
	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/wirecert"
)

// THE STATUS AND INSPECT FIXTURES (fixtures-c4b.md, F1, F2): the two reports
// a role consumes are produced over planted ledgers and hosts, with every
// host-specific value (paths, identifiers, digests, times, certificates)
// normalised by key, and re-encoded with sorted members; the committed file
// is the normalised report and the comparison proves the command's report
// still normalises to it.

var (
	hex64      = regexp.MustCompile(`^[0-9a-f]{64}$`)
	hex16      = regexp.MustCompile(`^[0-9a-f]{16}$`)
	rfc3339ish = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}`)
	pemBlock   = regexp.MustCompile(`(?s)-{5}BEGIN [A-Z ]+-{5}.*?-{5}END [A-Z ]+-{5}\n?`)
)

// normaliseReport parses a report, replaces the host's spellings (longest
// first), and normalises by key: every digest, identifier, time and PEM.
func normaliseReport(t *testing.T, out string, spellings map[string]string) string {
	t.Helper()

	var doc any
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("the report is not JSON: %v\n%s", err, out)
	}

	keys := make([]string, 0, len(spellings))
	for k := range spellings {
		keys = append(keys, k)
	}

	sort.Slice(keys, func(i, j int) bool { return len(keys[i]) > len(keys[j]) })

	var walk func(key string, v any) any

	walk = func(key string, v any) any {
		switch x := v.(type) {
		case map[string]any:
			for k, val := range x {
				x[k] = walk(k, val)
			}

			return x
		case []any:
			for i, val := range x {
				x[i] = walk(key, val)
			}

			return x
		case string:
			spelled := false

			for _, k := range keys {
				if strings.Contains(x, k) {
					x = strings.ReplaceAll(x, k, spellings[k])
					spelled = true
				}
			}

			// A spelled value is final: the packaged spelling is never
			// normalised again by a key rule.
			if spelled {
				return x
			}

			switch {
			case hex64.MatchString(x):
				return strings.Repeat("ab", 32)
			case key == "pem" || strings.HasSuffix(key, "_pem"):
				return pemBlock.ReplaceAllString(x, "-----BEGIN CERTIFICATE-----\nMIIB...\n-----END CERTIFICATE-----\n")
			case rfc3339ish.MatchString(x):
				return "2026-09-10T12:00:00Z"
			case key == "id" && (hex32.MatchString(x) || hex16.MatchString(x)):
				return strings.Repeat("1", len(x))
			}

			return x
		}

		return v
	}

	body, err := json.MarshalIndent(walk("", doc), "", "  ")
	mustOK(t, err)

	return string(body) + "\n"
}

// F1: rollout status over planted ledgers.
func TestTheRolloutStatusFixturesAreTheCommandsOwn(t *testing.T) {
	shapes := map[string]func(t *testing.T, stateDir, deployment string){
		"no-rollout": func(t *testing.T, stateDir, deployment string) {
			t.Helper()
			statusPlane(t, stateDir, func(db *state.DB) {
				if _, err := db.ClaimController(t.Context(), "billet-control-01", deployment); err != nil {
					t.Fatal(err)
				}

				registerStatusNode(t, db, "node-a", "v0.9.3", statusDigestA, regIncarnationNew)
			})
		},
		"open-rollout": func(t *testing.T, stateDir, deployment string) {
			t.Helper()
			statusPlane(t, stateDir, func(db *state.DB) {
				if _, err := db.ClaimController(t.Context(), "billet-control-01", deployment); err != nil {
					t.Fatal(err)
				}

				epoch := registerStatusNode(t, db, "node-a", "v0.9.3", statusDigestA, regIncarnationNew)
				registerStatusNode(t, db, "node-b", "v0.9.3", statusDigestA, regIncarnationOld)

				store := rollout.New(db)
				open := startStatusRollout(t, store, "v0.9.4", statusDigestB, "node-a", "node-b")
				advanceStatus(t, store, rollout.AdvanceRequest{RolloutID: open.ID, To: rollout.PhaseDraining})
				advanceStatus(t, store, rollout.AdvanceRequest{RolloutID: open.ID, Node: "node-a", To: rollout.PhaseDraining,
					DispatchEpoch: epoch, PriorRelease: "v0.9.3"})
				advanceStatus(t, store, rollout.AdvanceRequest{RolloutID: open.ID, Node: "node-b", To: rollout.PhasePending,
					Backoff: time.Hour, Refusal: "the host holds a converge guard for holder ci-42"})
			})
		},
		"finished-rollout": func(t *testing.T, stateDir, deployment string) {
			t.Helper()
			statusPlane(t, stateDir, func(db *state.DB) {
				if _, err := db.ClaimController(t.Context(), "billet-control-01", deployment); err != nil {
					t.Fatal(err)
				}

				registerStatusNode(t, db, "node-a", "v0.9.4", statusDigestB, regIncarnationNew)

				store := rollout.New(db)
				done := startStatusRollout(t, store, "v0.9.4", statusDigestB, "node-a")

				for _, phase := range []rollout.Phase{rollout.PhaseDraining, rollout.PhaseReadyToInstall,
					rollout.PhaseInstalling, rollout.PhaseVerifying, rollout.PhaseCommitted} {
					advanceStatus(t, store, rollout.AdvanceRequest{RolloutID: done.ID, To: phase})
					advanceStatus(t, store, rollout.AdvanceRequest{RolloutID: done.ID, Node: "node-a", To: phase})
				}

				if err := store.Finish(t.Context(), done.ID, rollout.StateCompleted, "every host converged"); err != nil {
					t.Fatal(err)
				}
			})
		},
		"unbound": func(t *testing.T, stateDir, _ string) {
			t.Helper()
			statusPlane(t, stateDir, func(*state.DB) {})
		},
	}

	for name, plant := range shapes {
		t.Run(name, func(t *testing.T) {
			stateDir := t.TempDir()
			cfgPath := writeCAConfig(t, stateDir)

			deployment, err := state.DeploymentID(stateDir)
			mustOK(t, err)

			plant(t, stateDir, deployment)

			_, _, out := statusJSON(t, cfgPath)
			compareFixture(t, "rollout-status", name, normaliseReport(t, out, map[string]string{
				deployment: strings.Repeat("d", 32), stateDir: "/var/lib/billet/server",
			}))
		})
	}

	fixtureSetIs(t, "rollout-status", fixtureNames(shapes))
}

// F2: release inspect over the inspector's own fixtures.
func TestTheReleaseInspectFixturesAreTheCommandsOwn(t *testing.T) {
	type planted struct {
		configPath string
		spellings  map[string]string
	}

	shapes := map[string]func(t *testing.T) planted{
		"sqlite-controller": func(t *testing.T) planted {
			t.Helper()
			f := newInspectFixture(t)

			id, err := state.DeploymentID(f.stateDir)
			mustOK(t, err)

			_, err = wirecert.LoadOrCreateCA(f.stateDir, id)
			mustOK(t, err)

			return planted{f.configPath, map[string]string{id: strings.Repeat("d", 32), f.configPath: "/etc/billet/billet.yaml",
				f.stateDir: "/var/lib/billet/server", f.binPath: "/usr/bin/billet", f.dir: "/var/lib/billet-fixture"}}
		},
		"postgres-controller-guarded": func(t *testing.T) planted {
			t.Helper()
			f := newInspectFixture(t)
			f.writeConfig(t, f.postgresConfig())
			f.touchBeforeStart(t, f.configPath)

			id, err := state.DeploymentID(f.stateDir)
			mustOK(t, err)

			_, err = wirecert.LoadOrCreateCA(f.stateDir, id)
			mustOK(t, err)

			envFile := filepath.Join(f.dir, "server.env")
			writeFile(t, envFile, "BILLET_PG_DSN=postgres://billet@127.0.0.1/billet\n", 0o640)
			f.touchBeforeStart(t, envFile)
			f.unitRunning(t, "billet-server.service", "server", f.configPath, []string{envFile})
			f.process(t, []string{f.binPath, "server", "--config", f.configPath},
				[]string{"BILLET_PG_DSN=postgres://billet@127.0.0.1/billet"})

			// A guard held by a converge, its root the inspector's.
			g := newGuardFixture(t)
			mustHold(t, "ci-1")
			installedBinary = f.binPath

			return planted{f.configPath, map[string]string{id: strings.Repeat("d", 32), f.configPath: "/etc/billet/billet.yaml",
				f.stateDir: "/var/lib/billet/server", f.binPath: "/usr/bin/billet", g.root: "/var/lib/billet/upgrades",
				g.binary: "/usr/bin/billet", f.dir: "/var/lib/billet-fixture"}}
		},
		"node-with-bundle": func(t *testing.T) planted {
			t.Helper()
			f := nodeTLSFixture(t, true)
			f.writeRecord(t, f.record(nil))
			r := newReceiptFixture(t)
			r.writeReceipt(t, validReceipt())

			return planted{f.configPath, map[string]string{f.configPath: "/etc/billet/billet.yaml",
				f.stateDir: "/var/lib/billet/server", f.binPath: "/usr/bin/billet", r.path: "/var/lib/billet/node/endpoint-migration.json",
				f.dir: "/var/lib/billet-fixture"}}
		},
		"node-stopped": func(t *testing.T) planted {
			t.Helper()
			f := newRegistrationFixture(t, "")
			f.writeConfig(t, f.nodeOnlyConfig())
			f.unitAbsent(t, "billet-node.service")
			_ = os.Remove(f.recordPath)
			r := newReceiptFixture(t)
			mustOK(t, os.RemoveAll(r.dir))

			return planted{f.configPath, map[string]string{f.configPath: "/etc/billet/billet.yaml",
				f.binPath: "/usr/bin/billet", f.dir: "/var/lib/billet-fixture"}}
		},
		"darwin": func(t *testing.T) planted {
			t.Helper()
			f := newInspectFixture(t)
			hostOS = "darwin"

			return planted{f.configPath, map[string]string{f.configPath: "/usr/local/etc/billet/billet.yaml",
				f.stateDir: "/usr/local/var/lib/billet/server", f.binPath: "/usr/local/bin/billet", f.dir: "/var/lib/billet-fixture"}}
		},
	}

	for name, plant := range shapes {
		t.Run(name, func(t *testing.T) {
			p := plant(t)

			var runErr error

			out := capture(t, func() { runErr = cmdReleaseInspect(t.Context(), []string{"--json", "--config", p.configPath}) })
			mustOK(t, runErr)

			compareFixture(t, "release-inspect", name, normaliseReport(t, out, p.spellings))
		})
	}

	fixtureSetIs(t, "release-inspect", fixtureNames(shapes))
}
