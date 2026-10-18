package host

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// THE ROLE READS THE REPAIR'S OWN ANSWERS: every shape `node repair-unit`
// answers with is written here from the command (BILLET_UPDATE_FIXTURES=1) and
// compared on every run, so the role's strict parser (repair-answer.yml) and
// the collection's scenarios are held to what the command prints.
func TestTheRepairFixturesAreTheCommandsOwn(t *testing.T) {
	shapes := map[string]func(t *testing.T, f *repairFixture) endpointOut{
		"repaired": func(t *testing.T, f *repairFixture) endpointOut {
			t.Helper()

			return f.repair(t)
		},
		"reported-required": func(t *testing.T, f *repairFixture) endpointOut {
			t.Helper()

			return f.repair(t, "--dry-run")
		},
		"reported-not-required": func(t *testing.T, f *repairFixture) endpointOut {
			t.Helper()
			f.loaded(t, nodeUnit, "active", "running", inspectPID, nodeInvocation, "billet/locks billet/registration")

			return f.repair(t, "--dry-run")
		},
		"unchanged": func(t *testing.T, f *repairFixture) endpointOut {
			t.Helper()
			f.loaded(t, nodeUnit, "inactive", "dead", 0, "", "billet/locks")

			return f.repair(t)
		},
		"refused-unit-shape": func(t *testing.T, f *repairFixture) endpointOut {
			t.Helper()
			f.set(t, nodeUnit, "DropInPaths", "/etc/systemd/system/billet-node.service.d/x.conf")

			return f.repair(t)
		},
		"unknown-unproved-stop": func(t *testing.T, f *repairFixture) endpointOut {
			t.Helper()
			f.set(t, nodeUnit+".after-stop", "Result", "timeout")
			f.set(t, nodeUnit+".after-stop", "ActiveState", "failed")
			f.set(t, nodeUnit+".after-stop", "SubState", "failed")

			return f.repair(t)
		},
		"unknown-unit-installed": func(t *testing.T, f *repairFixture) endpointOut {
			t.Helper()
			writeFile(t, f.unitFile("start.mode"), "fail\n", 0o644)

			return f.repair(t)
		},
		"unknown-started-no-record": func(t *testing.T, f *repairFixture) endpointOut {
			t.Helper()
			mustOK(t, os.Remove(f.unitFile("record.after-start")))

			return f.repair(t)
		},
	}

	for name, produce := range shapes {
		t.Run(name, func(t *testing.T) {
			f := newRepairFixture(t)

			o := produce(t, f)

			out := strings.ReplaceAll(o.raw, f.deployment, strings.Repeat("d", 32))
			out = strings.ReplaceAll(out, f.unitPath, repairUnitInstalledPath)
			out = strings.ReplaceAll(out, f.configPath, "/etc/billet/billet.yaml")
			// TWO DIGESTS THAT DIFFER, as a repair's always do: the role holds
			// them to that.
			out = regexp.MustCompile(`"before_sha256": "[0-9a-f]{64}"`).
				ReplaceAllString(out, `"before_sha256": "`+strings.Repeat("ab", 32)+`"`)
			out = regexp.MustCompile(`"after_sha256": "[0-9a-f]{64}"`).
				ReplaceAllString(out, `"after_sha256": "`+strings.Repeat("cd", 32)+`"`)
			out = strings.ReplaceAll(out, f.recordPath, "/run/billet/registration/current")
			out = strings.ReplaceAll(out, f.dir, "/var/lib/billet-fixture")
			compareFixture(t, "node-repair-unit", name, out)
		})
	}

	fixtureSetIs(t, "node-repair-unit", fixtureNames(shapes))
}

// repairUnitInstalledPath is the unit the repair edits on a host, which the
// fixtures name in place of the test's own.
const repairUnitInstalledPath = "/etc/systemd/system/billet-node.service"
