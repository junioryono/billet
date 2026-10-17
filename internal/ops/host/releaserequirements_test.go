package host

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/junioryono/billet/deploy"
	"github.com/junioryono/billet/internal/lifeops"
)

// requirementsHost is a staged candidate that answers `release requirements`
// and the guard's dry run as the test says, and refuses any other argv; and a
// systemctl that answers the node unit's properties from a file, each only when
// asked for, and nothing about any other unit.
type requirementsHost struct {
	dir, candidate, unit string
	insp                 *lifeops.Inspector
}

func newRequirementsHost(t *testing.T, requirements, guard, unit string) *requirementsHost {
	t.Helper()

	h := &requirementsHost{dir: t.TempDir()}
	h.candidate = filepath.Join(h.dir, "billet-candidate")
	h.unit = filepath.Join(h.dir, "node.properties")
	writeFile(t, h.candidate, `#!/bin/sh
case "$*" in
  "release requirements --config /etc/billet/billet.yaml --json")
`+requirements+`
    ;;
  "converge-guard prepare --dry-run --json")
`+guard+`
    ;;
  *) echo "unexpected argv: $*" >&2; exit 99 ;;
esac
`, 0o755)
	writeFile(t, h.unit, unit, 0o644)

	systemctl := filepath.Join(h.dir, "systemctl")
	writeFile(t, systemctl, `#!/bin/sh
case " $* " in *" billet-node.service "*) ;; *) exit 0 ;; esac
for a in "$@"; do case "$a" in --property=*) grep "^${a#--property=}=" "`+h.unit+`";; esac; done
exit 0
`, 0o755)

	h.insp = lifeops.NewInspector(lifeops.WithSystemctl(systemctl))

	return h
}

func (h *requirementsHost) check(t *testing.T) (string, error) {
	t.Helper()

	return checkUnitRequirements(t.Context(), h.candidate, "/etc/billet/billet.yaml", h.insp)
}

const (
	requiresRegistration = `printf '{"schema":1,"runtime_directories":["billet/registration"]}\n'`
	requiresNothing      = `printf '{"schema":1,"runtime_directories":[]}\n'`
	unknownRequirements  = `echo 'unknown release command "requirements"; try record or inspect' >&2; exit 1`
	guardAnswers         = `printf '{"outcome":"reported"}\n'`
	guardUnknown         = `echo 'unknown command "converge-guard"' >&2; exit 2`
	guardNeverAsked      = `echo 'the guard was asked' >&2; exit 98`
	legacyNodeUnit       = "LoadState=loaded\nRuntimeDirectory=billet/locks\n"
	currentNodeUnit      = "LoadState=loaded\nRuntimeDirectory=billet/locks billet/registration\n"
)

// A CANDIDATE IS HELD TO WHAT IT NEEDS OF THE NODE UNIT (#168): a loaded unit
// that lacks a required runtime directory refuses naming the preparation; one
// that has it passes; a candidate that requires nothing passes and says so; a
// candidate from before the question is asked whether it carries the guard,
// which every release that publishes its record does, and is held to the
// record's directory when it does; a host whose node unit is absent or masked
// passes; and anything not answered (a candidate that fails another way, an
// answer this build cannot read, a property systemd did not give, a candidate
// whose vintage cannot be told) is an error, never a pass.
func TestTheCandidateIsHeldToWhatItRequiresOfTheNodeUnit(t *testing.T) {
	for _, tc := range []struct {
		name, requirements, guard, unit string
		refused                         string // a clause of the refusal, or "" for a pass
		found                           string // a clause of the pass's report
	}{
		{"the unit from before", requiresRegistration, guardNeverAsked, legacyNodeUnit,
			"RuntimeDirectory=billet/registration, and the loaded unit does not", ""},
		{"the current unit", requiresRegistration, guardNeverAsked, currentNodeUnit, "", "provides everything"},
		{"nothing required", requiresNothing, guardNeverAsked, legacyNodeUnit, "", "requires nothing"},
		{"a publishing candidate from before the question, the unit from before", unknownRequirements, guardAnswers,
			legacyNodeUnit, "RuntimeDirectory=billet/registration, and the loaded unit does not", ""},
		{"a publishing candidate from before the question, the current unit", unknownRequirements, guardAnswers,
			currentNodeUnit, "", "provides everything"},
		{"a candidate from before the guard", unknownRequirements, guardUnknown, legacyNodeUnit, "",
			"release before the converge guard"},
		{"a candidate from before the question whose guard does not answer", unknownRequirements,
			`exit 1`, legacyNodeUnit, "", ""},
		{"no node unit", requiresRegistration, guardNeverAsked, "LoadState=not-found\nRuntimeDirectory=\n", "",
			"is not-found"},
		{"a masked node unit", requiresRegistration, guardNeverAsked, "LoadState=masked\nRuntimeDirectory=\n", "",
			"is masked"},
		{"a candidate that fails", `echo 'open /etc/billet/billet.yaml: permission denied' >&2; exit 1`,
			guardNeverAsked, currentNodeUnit, "refused to say", ""},
		{"an answer with a member this build does not read",
			`printf '{"schema":1,"runtime_directories":[],"later":true}\n'`, guardNeverAsked, currentNodeUnit,
			"not one this build reads", ""},
		{"an answer of another schema", `printf '{"schema":2,"runtime_directories":[]}\n'`, guardNeverAsked,
			currentNodeUnit, "not one this build reads", ""},
		{"an answer without the list", `printf '{"schema":1}\n'`, guardNeverAsked, currentNodeUnit,
			"not one this build reads", ""},
		{"RuntimeDirectory unanswered", requiresRegistration, guardNeverAsked, "LoadState=loaded\n",
			"answered no RuntimeDirectory", ""},
		{"a unit in another state", requiresRegistration, guardNeverAsked, "LoadState=error\nRuntimeDirectory=\n",
			"LoadState=error", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newRequirementsHost(t, tc.requirements, tc.guard, tc.unit)

			found, err := h.check(t)

			switch {
			case tc.refused == "" && tc.found == "":
				// A GUARD THAT ANSWERED NEITHER WAY: a completed non-zero run that
				// is not an unknown command is a release that carries the guard.
				if err == nil || !strings.Contains(err.Error(), "the loaded unit does not") {
					t.Fatalf("checked to %q, %v; want the candidate held to the record's directory", found, err)
				}
			case tc.refused != "" && (err == nil || !strings.Contains(err.Error(), tc.refused)):
				t.Fatalf("checked to %q, %v; want a refusal saying %q", found, err, tc.refused)
			case tc.refused == "" && tc.found != "" && (err != nil || !strings.Contains(found, tc.found)):
				t.Fatalf("checked to %q, %v; want a pass saying %q", found, err, tc.found)
			}

			if strings.HasPrefix(tc.name, "the unit from before") &&
				!strings.Contains(err.Error(), "converge the host role, or install the package") {
				t.Errorf("the refusal %q does not name the preparation", err)
			}
		})
	}
}

// THE TRANSACTION'S HOST ASKS ITS OWN STAGED CANDIDATE about its own
// configuration: the systemd host's step is the check above against the
// binary it staged, the path it was started with and the unit it inspects.
func TestTheSystemdHostChecksItsStagedCandidate(t *testing.T) {
	h := newRequirementsHost(t, requiresRegistration, guardNeverAsked, legacyNodeUnit)

	var stdout bytes.Buffer

	env := processEnv()
	env.Stdout = &stdout

	host := &systemdHost{ledgerHost: ledgerHost{env: env, cfgPath: "/etc/billet/billet.yaml"}, staged: h.candidate,
		inspect: h.insp}

	if err := host.CheckUnitRequirements(t.Context()); err == nil || !strings.Contains(err.Error(), "the loaded unit does not") {
		t.Fatalf("the systemd host admitted a unit its candidate cannot run as it needs: %v", err)
	}

	writeFile(t, h.unit, currentNodeUnit, 0o644)

	if err := host.CheckUnitRequirements(t.Context()); err != nil {
		t.Fatalf("the systemd host refused a unit that meets its candidate's requirements: %v", err)
	}

	if !strings.Contains(stdout.String(), "unit requirements: the loaded node unit provides everything") {
		t.Errorf("the step reported %q, want what it found", stdout.String())
	}
}

// THE ANSWER IS THE BUILD'S OWN DECLARATION, through the command a candidate is
// asked by: a node's configuration requires every runtime directory
// deploy.NodeRuntimeDirectories names, and a server-only one requires none.
func TestReleaseRequirementsAnswersForTheConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name string
		node bool
		want []string
	}{
		{"a node", true, deploy.NodeRuntimeDirectories},
		{"no node section", false, []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newEndpointFixture(t)
			if !tc.node {
				f.writeConfig(t, f.serverOnly())
			}

			var stdout bytes.Buffer

			env := processEnv()
			env.Stdout = &stdout

			if err := Release(t.Context(), env, []string{"requirements", "--config", f.configPath, "--json"}); err != nil {
				t.Fatalf("release requirements: %v", err)
			}

			var got unitRequirements

			dec := json.NewDecoder(&stdout)
			dec.DisallowUnknownFields()

			if err := dec.Decode(&got); err != nil {
				t.Fatalf("decode %q: %v", stdout.String(), err)
			}

			if got.Schema != requirementsSchema || got.RuntimeDirectories == nil ||
				!slices.Equal(got.RuntimeDirectories, tc.want) {
				t.Errorf("answered %+v, want schema %d and %q", got, requirementsSchema, tc.want)
			}
		})
	}
}
