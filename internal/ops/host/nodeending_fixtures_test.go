package host

import (
	"regexp"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/nodeending"
)

// THE ROLE READS `billet node ending` THROUGH FIXTURES THE COMMAND WROTE: each
// answer the role's parser (tasks/ending-answer.yml) must accept or refuse, by
// the command itself over a planted host. A time, a boot and a path that
// differ by machine are written as fixed stand-ins.
func TestTheEndingFixturesAreTheCommandsOwn(t *testing.T) {
	shapes := map[string]func(t *testing.T, f *endingFixture) endpointOut{
		"check-proved": func(t *testing.T, f *endingFixture) endpointOut {
			t.Helper()
			f.begin(t, "inv-1", nodeending.Drained)

			return f.run(t, "check", "--invocation", "inv-1", "--drain")
		},
		"check-proved-floor": func(t *testing.T, f *endingFixture) endpointOut {
			t.Helper()

			return f.run(t, "check", "--invocation", "inv-old")
		},
		"check-unproved": func(t *testing.T, f *endingFixture) endpointOut {
			t.Helper()
			f.begin(t, "inv-1", nodeending.Running)
			f.unit(t, "failed", "failed", "timeout", "0", "inv-1")

			return f.run(t, "check", "--invocation", "inv-1")
		},
		"check-unknown": func(t *testing.T, f *endingFixture) endpointOut {
			t.Helper()
			f.begin(t, "inv-1", nodeending.Drained)
			writeFile(t, f.unitFile, "", 0o644)

			return f.run(t, "check", "--invocation", "inv-1")
		},
		"admit-start-proved": func(t *testing.T, f *endingFixture) endpointOut {
			t.Helper()
			f.begin(t, "inv-1", nodeending.Drained)

			return f.run(t, "admit-start")
		},
		"admit-start-running": func(t *testing.T, f *endingFixture) endpointOut {
			t.Helper()
			f.begin(t, "inv-1", nodeending.Running)
			f.unit(t, "active", "running", "success", "41", "inv-1")

			return f.run(t, "admit-start")
		},
		"admit-start-unproved": func(t *testing.T, f *endingFixture) endpointOut {
			t.Helper()
			f.begin(t, "inv-1", nodeending.Running)

			return f.run(t, "admit-start")
		},
		"admit-start-unknown": func(t *testing.T, f *endingFixture) endpointOut {
			t.Helper()
			writeFile(t, nodeending.Path(f.stateDir), "{", 0o600)

			return f.run(t, "admit-start")
		},
	}

	times := regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?Z`)
	boots := regexp.MustCompile(`"boot_id": "[^"]*"`)

	for name, produce := range shapes {
		t.Run(name, func(t *testing.T) {
			f := newEndingFixture(t)

			o := produce(t, f)

			out := times.ReplaceAllString(o.raw, "2026-10-17T18:00:00Z")
			out = boots.ReplaceAllString(out, `"boot_id": "boot-a"`)
			out = strings.ReplaceAll(out, f.stateDir, "/var/lib/billet/node")
			compareFixture(t, "node-ending", name, out)
		})
	}

	fixtureSetIs(t, "node-ending", fixtureNames(shapes))
}
