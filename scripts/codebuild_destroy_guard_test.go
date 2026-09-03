package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// THE DESTROY GUARD IS EXECUTED, NOT PATTERN-MATCHED. It is a shell script that
// decides whether `terraform destroy` may remove the roles and log group a running
// CodeBuild build depends on, and AWS does nothing to stop that removal: DeleteProject
// succeeds while a build is in progress and the build runs on (measured 2026-09-02,
// docs/aws-acceptance.md). So the script is run here against a fake `aws` that replays
// scripted pages and records every invocation — in both directions, because a gate
// that refuses a drained fleet is the failure ADR-005 names, and the next thing anybody
// does with one is delete it.

const guardScript = "../terraform/modules/billet/modules/fleet-codebuild/scripts/refuse-active-builds.sh"

// fakeAWS stands in for the CLI. It answers ListBuildsForProject from
// $FAKE_AWS_DIR/list-<token> ("first" for the untokened call) and BatchGetBuilds from
// $FAKE_AWS_DIR/batch-<first id>, appends every argv to $FAKE_AWS_LOG together with
// the TZ it was handed, and fails every call when FAKE_AWS_FAIL is set.
const fakeAWS = `#!/bin/sh
printf 'TZ=%s LC_ALL=%s :: %s\n' "${TZ:-unset}" "${LC_ALL:-unset}" "$*" >> "$FAKE_AWS_LOG"
if [ -n "${FAKE_AWS_FAIL:-}" ]; then
  echo "An error occurred (AccessDeniedException) when calling the $2 operation: fake refusal" >&2
  exit 254
fi
case "$1 $2" in
  "codebuild list-builds-for-project")
    token=first
    while [ $# -gt 0 ]; do
      if [ "$1" = "--next-token" ]; then token=$2; fi
      shift
    done
    f="$FAKE_AWS_DIR/list-$token"
    if [ ! -f "$f" ]; then echo "fake aws: no page for token $token" >&2; exit 253; fi
    cat "$f"
    ;;
  "codebuild batch-get-builds")
    first=""
    while [ $# -gt 0 ]; do
      if [ "$1" = "--ids" ]; then first=$2; fi
      shift
    done
    f="$FAKE_AWS_DIR/batch-$first"
    if [ ! -f "$f" ]; then echo "fake aws: no batch for $first" >&2; exit 253; fi
    cat "$f"
    ;;
  *)
    echo "fake aws: unexpected command $1 $2" >&2
    exit 252
    ;;
esac
`

// guardHarness is one run's fake CLI, fixture directory and invocation log.
type guardHarness struct {
	bin, fixtures, log string
	path               string
}

// newGuardHarness builds a PATH holding the fake `aws` and ONLY the tools the script
// needs (sh, cut, tr, sort, head, date), by symlink, so the machine's real aws can never
// be reached and the "no aws on PATH" case is expressible by leaving the fake out.
func newGuardHarness(t *testing.T, withAWS bool) guardHarness {
	t.Helper()

	root := t.TempDir()
	h := guardHarness{
		bin:      filepath.Join(root, "bin"),
		fixtures: filepath.Join(root, "fixtures"),
		log:      filepath.Join(root, "aws.log"),
	}

	for _, dir := range []string{h.bin, h.fixtures} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}

	for _, tool := range []string{"sh", "cut", "tr", "sort", "head", "date", "cat", "printf"} {
		resolved, err := exec.LookPath(tool)
		if err != nil {
			t.Fatalf("this test needs %s on PATH: %v", tool, err)
		}

		if err := os.Symlink(resolved, filepath.Join(h.bin, tool)); err != nil {
			t.Fatalf("symlink %s: %v", tool, err)
		}
	}

	if withAWS {
		if err := os.WriteFile(filepath.Join(h.bin, "aws"), []byte(fakeAWS), 0o755); err != nil {
			t.Fatalf("write fake aws: %v", err)
		}
	}

	h.path = h.bin

	return h
}

// page writes a ListBuildsForProject answer as the CLI renders it under
// --query '[join(`,`, ids), nextToken == `null`, nextToken]' --output text: the ids
// joined by commas, a tab, True or False for "the token is absent", a tab, then the
// token or "None". Measured against the real CLI, empty ids included.
func (h guardHarness) page(t *testing.T, token string, ids []string, next string) {
	t.Helper()

	absent := "False"
	if next == "" {
		absent, next = "True", "None"
	}

	body := strings.Join(ids, ",") + "\t" + absent + "\t" + next + "\n"
	if err := os.WriteFile(filepath.Join(h.fixtures, "list-"+token), []byte(body), 0o644); err != nil {
		t.Fatalf("write page %s: %v", token, err)
	}
}

// build is one BatchGetBuilds row.
type build struct {
	id, status string
	start      time.Time // zero means "None": SUBMITTED or QUEUED, not started yet
}

// batch writes a BatchGetBuilds answer as the CLI renders it under
// --query '[builds[].[id, buildStatus, startTime], buildsNotFound]' --output text: one
// tab-separated line per build, then each not-found id on a line of its own. Measured.
func (h guardHarness) batch(t *testing.T, first string, builds []build, notFound []string) {
	t.Helper()

	var b strings.Builder

	for _, bd := range builds {
		start := "None"
		if !bd.start.IsZero() {
			start = bd.start.UTC().Format("2006-01-02T15:04:05.000000+00:00")
		}

		b.WriteString(bd.id + "\t" + bd.status + "\t" + start + "\n")
	}

	for _, id := range notFound {
		b.WriteString(id + "\n")
	}

	if err := os.WriteFile(filepath.Join(h.fixtures, "batch-"+first), []byte(b.String()), 0o644); err != nil {
		t.Fatalf("write batch %s: %v", first, err)
	}
}

// raw writes a fixture verbatim, for answers the CLI would never render but a
// truncated or drifted one might.
func (h guardHarness) raw(t *testing.T, name, body string) {
	t.Helper()

	if err := os.WriteFile(filepath.Join(h.fixtures, name), []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// fakeDate pins `date +%s` to $FAKE_NOW and hands every other invocation to the real
// date, so a cutoff can be known exactly and an epoch staged on either side of it.
const fakeDate = `#!/bin/sh
if [ "$1" = "+%s" ]; then
  echo "$FAKE_NOW"
  exit 0
fi
exec "$REAL_DATE" "$@"
`

// pinClock replaces the harness's date with fakeDate; the run supplies FAKE_NOW.
func (h guardHarness) pinClock(t *testing.T) {
	t.Helper()

	link := filepath.Join(h.bin, "date")
	if err := os.Remove(link); err != nil {
		t.Fatalf("unlink date: %v", err)
	}

	if err := os.WriteFile(link, []byte(fakeDate), 0o755); err != nil {
		t.Fatalf("write fake date: %v", err)
	}
}

// run executes the guard with the fake on PATH and the given extra environment.
func (h guardHarness) run(t *testing.T, extra ...string) (string, error) {
	t.Helper()

	script, err := filepath.Abs(guardScript)
	if err != nil {
		t.Fatalf("abs: %v", err)
	}

	cmd := exec.CommandContext(t.Context(), script)
	cmd.Env = append([]string{
		"PATH=" + h.path,
		"HOME=" + t.TempDir(),
		"FAKE_AWS_DIR=" + h.fixtures,
		"FAKE_AWS_LOG=" + h.log,
		"BILLET_GUARD_PROJECT=billet-test",
		"BILLET_GUARD_REGION=us-west-2",
	}, extra...)

	out, err := cmd.CombinedOutput()

	return string(out), err
}

// calls returns every recorded fake invocation.
func (h guardHarness) calls(t *testing.T) string {
	t.Helper()

	data, err := os.ReadFile(h.log)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read log: %v", err)
	}

	return string(data)
}

var (
	recent = time.Now().Add(-1 * time.Hour)
	// ancient is older than the abandon cutoff (2160+480+60+480 minutes ≈ 53h).
	ancient = time.Now().Add(-100 * time.Hour)
)

func TestTheGuardRefusesARunningBuildByName(t *testing.T) {
	t.Parallel()

	h := newGuardHarness(t, true)
	h.page(t, "first", []string{"billet-test:aaa", "billet-test:bbb"}, "")
	h.batch(t, "billet-test:aaa", []build{
		{id: "billet-test:aaa", status: "SUCCEEDED", start: recent},
		{id: "billet-test:bbb", status: "IN_PROGRESS", start: recent},
	}, nil)

	out, err := h.run(t)
	if err == nil {
		t.Fatalf("a running build did not refuse the destroy:\n%s", out)
	}

	for _, want := range []string{"billet-test:bbb(IN_PROGRESS)", "billet drain --wait", "BILLET_SKIP_ACTIVE_BUILD_GUARD"} {
		if !strings.Contains(out, want) {
			t.Errorf("the refusal does not say %q:\n%s", want, out)
		}
	}
}

// THE OTHER DIRECTION, or a script that refuses everything passes every test above.
func TestTheGuardLetsADrainedProjectBeDestroyed(t *testing.T) {
	t.Parallel()

	h := newGuardHarness(t, true)
	h.page(t, "first", []string{"billet-test:aaa", "billet-test:bbb"}, "")
	h.batch(t, "billet-test:aaa", []build{
		{id: "billet-test:aaa", status: "SUCCEEDED", start: recent},
		{id: "billet-test:bbb", status: "FAILED", start: recent},
	}, nil)

	out, err := h.run(t)
	if err != nil {
		t.Fatalf("a drained project was refused (%v):\n%s", err, out)
	}

	if !strings.Contains(out, "destroy may proceed") {
		t.Errorf("the pass does not say so:\n%s", out)
	}
}

// THE CLI OMITS THE FRACTION WHEN THE MICROSECONDS ARE ZERO — its ISO renderer prints
// 2026-09-02T01:37:02+00:00 for such a build — and a guard that demanded six digits
// would refuse a drained project over a build that happened to start on the second.
// Staged verbatim, because the fixture helper always renders a fraction.
func TestTheGuardAcceptsAnExactSecondStartTime(t *testing.T) {
	t.Parallel()

	h := newGuardHarness(t, true)
	h.page(t, "first", []string{"billet-test:aaa"}, "")
	h.raw(t, "batch-billet-test:aaa", "billet-test:aaa\tSUCCEEDED\t"+recent.UTC().Format("2006-01-02T15:04:05")+"+00:00\n")

	out, err := h.run(t)
	if err != nil {
		t.Fatalf("an exact-second start time refused a drained project (%v):\n%s", err, out)
	}
}

// THE CLI'S WIRE TIMESTAMP MODE RENDERS EPOCH SECONDS, not ISO — CLI v1's default, and
// v2's under cli_timestamp_format = wire, which TZ=UTC does not override. A guard that
// read only the ISO form would refuse such an operator's drained project forever. Both
// directions: an old epoch ends the walk without a second listing, a recent one keeps
// it going.
func TestTheGuardReadsWireFormatStartTimes(t *testing.T) {
	t.Parallel()

	t.Run("an old epoch ends the walk", func(t *testing.T) {
		t.Parallel()

		h := newGuardHarness(t, true)
		h.page(t, "first", []string{"billet-test:old"}, "t2")
		h.raw(t, "batch-billet-test:old", "billet-test:old\tSUCCEEDED\t"+strconv.FormatInt(ancient.Unix(), 10)+".123\n")
		h.page(t, "t2", []string{"billet-test:run"}, "")
		h.batch(t, "billet-test:run", []build{{id: "billet-test:run", status: "IN_PROGRESS", start: recent}}, nil)

		out, err := h.run(t)
		if err != nil {
			t.Fatalf("an old wire-format page did not end the walk (%v):\n%s", err, out)
		}

		if strings.Contains(h.calls(t), "--next-token t2") {
			t.Errorf("the walk read past a page whose wire-format times were all old:\n%s", h.calls(t))
		}
	})

	t.Run("a recent epoch keeps the walk going", func(t *testing.T) {
		t.Parallel()

		h := newGuardHarness(t, true)
		h.page(t, "first", []string{"billet-test:new"}, "t2")
		h.raw(t, "batch-billet-test:new", "billet-test:new\tSUCCEEDED\t"+strconv.FormatInt(recent.Unix(), 10)+"\n")
		h.page(t, "t2", []string{"billet-test:run"}, "")
		h.batch(t, "billet-test:run", []build{{id: "billet-test:run", status: "IN_PROGRESS", start: recent}}, nil)

		out, err := h.run(t)
		if err == nil {
			t.Fatalf("a recent wire-format page ended the walk before the running build:\n%s", out)
		}

		if !strings.Contains(out, "billet-test:run(IN_PROGRESS)") {
			t.Errorf("the running build on the second page was not what refused:\n%s", out)
		}
	})
}

// THE CALENDAR'S BOUNDARIES: February has 29 days in a leap year and 28 otherwise, and
// a century year is a leap year only every fourth century. A check that treated
// February as always 28 or always 29 passes the February-30 case above; these do not.
func TestTheGuardKnowsWhichFebruariesHaveTwentyNineDays(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		date string
		ok   bool
	}{
		{date: "2028-02-29", ok: true},
		{date: "2000-02-29", ok: true},
		{date: "2400-02-29", ok: true},
		{date: "2026-02-29", ok: false},
		{date: "1900-02-29", ok: false},
		{date: "2200-02-29", ok: false},
	} {
		t.Run(tc.date, func(t *testing.T) {
			t.Parallel()

			h := newGuardHarness(t, true)
			h.page(t, "first", []string{"billet-test:aaa"}, "")
			h.raw(t, "batch-billet-test:aaa", "billet-test:aaa\tSUCCEEDED\t"+tc.date+"T12:00:00.000000+00:00\n")

			out, err := h.run(t)

			if tc.ok && err != nil {
				t.Fatalf("a real date refused a drained project (%v):\n%s", err, out)
			}

			if !tc.ok && (err == nil || !strings.Contains(out, "start time")) {
				t.Fatalf("an impossible date was read as a timestamp:\n%s", out)
			}
		})
	}
}

// THE EPOCH CUTOFF IS STRICT, AND THE CLOCK IS PINNED SO THE BOUNDARY IS EXACT: an
// epoch one second before the abandon cutoff ends the walk, the cutoff itself does
// not, and a fraction leaves the integer part's verdict alone.
func TestTheWireCutoffIsExact(t *testing.T) {
	t.Parallel()

	const (
		now           = int64(1_800_000_000)
		abandonWindow = int64((2160 + 480 + 60 + 480) * 60)
		cutoff        = now - abandonWindow
	)

	for _, tc := range []struct {
		name  string
		epoch string
		ends  bool
	}{
		{name: "one second before the cutoff", epoch: strconv.FormatInt(cutoff-1, 10), ends: true},
		{name: "exactly the cutoff", epoch: strconv.FormatInt(cutoff, 10), ends: false},
		{name: "the cutoff with a fraction", epoch: strconv.FormatInt(cutoff, 10) + ".999", ends: false},
		{name: "before the cutoff with a fraction", epoch: strconv.FormatInt(cutoff-1, 10) + ".999", ends: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			h := newGuardHarness(t, true)
			h.pinClock(t)
			h.page(t, "first", []string{"billet-test:aaa"}, "t2")
			h.raw(t, "batch-billet-test:aaa", "billet-test:aaa\tSUCCEEDED\t"+tc.epoch+"\n")
			h.page(t, "t2", []string{"billet-test:run"}, "")
			h.batch(t, "billet-test:run", []build{{id: "billet-test:run", status: "IN_PROGRESS", start: recent}}, nil)

			realDate, err := exec.LookPath("date")
			if err != nil {
				t.Fatal(err)
			}

			out, err := h.run(t, "FAKE_NOW="+strconv.FormatInt(now, 10), "REAL_DATE="+realDate)
			walked := strings.Contains(h.calls(t), "--next-token t2")

			if tc.ends && (err != nil || walked) {
				t.Fatalf("an epoch before the cutoff did not end the walk (err=%v, walked=%v):\n%s", err, walked, out)
			}

			if !tc.ends && (err == nil || !walked) {
				t.Fatalf("an epoch at or after the cutoff ended the walk (err=%v, walked=%v):\n%s", err, walked, out)
			}
		})
	}
}

// AN EMPTY PROJECT IS DRAINED TOO: the CLI renders no ids as an empty field and no
// token as None, and a guard that could not read that would refuse a project that has
// never run a build.
func TestTheGuardLetsAnEmptyProjectBeDestroyed(t *testing.T) {
	t.Parallel()

	h := newGuardHarness(t, true)
	h.page(t, "first", nil, "")

	out, err := h.run(t)
	if err != nil {
		t.Fatalf("an empty project was refused (%v):\n%s", err, out)
	}
}

// A STATUS THE SCRIPT HAS NEVER HEARD OF IS RUNNING, the provider's own rule: the caller
// destroys what is not running, and a state billet does not know is not evidence a
// job is over.
func TestTheGuardTreatsAnUnknownStatusAsRunning(t *testing.T) {
	t.Parallel()

	h := newGuardHarness(t, true)
	h.page(t, "first", []string{"billet-test:aaa"}, "")
	h.batch(t, "billet-test:aaa", []build{
		{id: "billet-test:aaa", status: "FINISHING_UP", start: recent},
	}, nil)

	out, err := h.run(t)
	if err == nil {
		t.Fatalf("an unknown status let the destroy proceed:\n%s", out)
	}

	if !strings.Contains(out, "billet-test:aaa(FINISHING_UP)") {
		t.Errorf("the refusal does not name the build and its status:\n%s", out)
	}
}

// "COULD NOT TELL" IS NEVER "NOTHING RUNNING", in each of its three shapes.
func TestTheGuardRefusesWhatItCannotEstablish(t *testing.T) {
	t.Parallel()

	t.Run("an id CodeBuild did not know", func(t *testing.T) {
		t.Parallel()

		h := newGuardHarness(t, true)
		h.page(t, "first", []string{"billet-test:aaa", "billet-test:ghost"}, "")
		h.batch(t, "billet-test:aaa", []build{
			{id: "billet-test:aaa", status: "SUCCEEDED", start: recent},
		}, []string{"billet-test:ghost"})

		out, err := h.run(t)
		if err == nil {
			t.Fatalf("a not-found id let the destroy proceed:\n%s", out)
		}

		if !strings.Contains(out, "did not know build billet-test:ghost") {
			t.Errorf("the refusal does not name the unknown id:\n%s", out)
		}
	})

	t.Run("a failing CLI", func(t *testing.T) {
		t.Parallel()

		h := newGuardHarness(t, true)
		h.page(t, "first", []string{"billet-test:aaa"}, "")

		out, err := h.run(t, "FAKE_AWS_FAIL=1")
		if err == nil {
			t.Fatalf("a failing aws let the destroy proceed:\n%s", out)
		}

		if !strings.Contains(out, "ListBuildsForProject on billet-test failed") ||
			!strings.Contains(out, "fake refusal") {
			t.Errorf("the refusal does not carry the CLI's own error:\n%s", out)
		}
	})

	t.Run("no aws on PATH", func(t *testing.T) {
		t.Parallel()

		h := newGuardHarness(t, false)

		out, err := h.run(t)
		if err == nil {
			t.Fatalf("a missing aws let the destroy proceed:\n%s", out)
		}

		if !strings.Contains(out, "aws CLI is not on PATH") ||
			!strings.Contains(out, "BILLET_SKIP_ACTIVE_BUILD_GUARD=1") {
			t.Errorf("the refusal does not name the CLI and the waiver:\n%s", out)
		}
	})

	t.Run("a pagination token that repeats", func(t *testing.T) {
		t.Parallel()

		h := newGuardHarness(t, true)
		h.page(t, "first", []string{"billet-test:aaa"}, "t2")
		h.batch(t, "billet-test:aaa", []build{{id: "billet-test:aaa", status: "SUCCEEDED", start: recent}}, nil)
		h.page(t, "t2", []string{"billet-test:bbb"}, "t2")
		h.batch(t, "billet-test:bbb", []build{{id: "billet-test:bbb", status: "SUCCEEDED", start: recent}}, nil)

		out, err := h.run(t)
		if err == nil {
			t.Fatalf("a cycling token let the walk end:\n%s", out)
		}

		if !strings.Contains(out, "token it already issued") {
			t.Errorf("the refusal does not name the cycle:\n%s", out)
		}
	})
}

// AN ANSWER THAT IS NOT THE SHAPE THE CLI WAS ASKED FOR IS NOT AN ANSWER. A review
// found the first version reading an empty or tab-less listing as "no builds" and a
// batch that omitted an id as "nothing running" — the exact could-not-tell state the
// guard promises to refuse, produced by a truncated response or a CLI whose text
// rendering drifted. Each shape is staged verbatim, because the fake's helpers can
// only render well-formed answers.
func TestTheGuardRefusesAnAnswerItCannotAccountFor(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		stage func(h guardHarness)
		want  string
	}{
		{
			name:  "a listing with no output at all",
			stage: func(h guardHarness) { h.raw(t, "list-first", "") },
			want:  "fewer fields than were asked for",
		},
		{
			name:  "a listing with no tab",
			stage: func(h guardHarness) { h.raw(t, "list-first", "billet-test:aaa\n") },
			want:  "fewer fields than were asked for",
		},
		{
			name: "a listing of several lines",
			stage: func(h guardHarness) {
				h.raw(t, "list-first", "billet-test:aaa\tTrue\tNone\nbillet-test:bbb\tTrue\tNone\n")
			},
			want: "shape this script cannot read",
		},
		{
			name:  "a listing with only the id field and a tab",
			stage: func(h guardHarness) { h.raw(t, "list-first", "billet-test:aaa\t\n") },
			want:  "fewer fields than were asked for",
		},
		{
			name:  "a listing that is only two tabs",
			stage: func(h guardHarness) { h.raw(t, "list-first", "\t\t\n") },
			want:  "shape this script cannot read",
		},
		{
			name:  "a listing whose presence flag is neither True nor False",
			stage: func(h guardHarness) { h.raw(t, "list-first", "billet-test:aaa\tNull\tNone\n") },
			want:  "shape this script cannot read",
		},
		{
			name:  "a listing that says no token and then names one",
			stage: func(h guardHarness) { h.raw(t, "list-first", "billet-test:aaa\tTrue\tt2\n") },
			want:  "no token and then a token",
		},
		{
			name:  "a listing that says a token and then names none",
			stage: func(h guardHarness) { h.raw(t, "list-first", "billet-test:aaa\tFalse\t\n") },
			want:  "a token and then none",
		},
		{
			name:  "a listing with a fourth field",
			stage: func(h guardHarness) { h.raw(t, "list-first", "billet-test:aaa\tTrue\tNone\textra\n") },
			want:  "more fields than were asked for",
		},
		{
			name:  "an id list with a trailing comma",
			stage: func(h guardHarness) { h.raw(t, "list-first", "billet-test:aaa,\tTrue\tNone\n") },
			want:  "an id this script cannot read",
		},
		{
			name:  "an id list with a leading comma",
			stage: func(h guardHarness) { h.raw(t, "list-first", ",billet-test:aaa\tTrue\tNone\n") },
			want:  "an id this script cannot read",
		},
		{
			name:  "an id list with an empty component",
			stage: func(h guardHarness) { h.raw(t, "list-first", "billet-test:aaa,,billet-test:bbb\tTrue\tNone\n") },
			want:  "an id this script cannot read",
		},
		{
			name:  "an id list that is only whitespace",
			stage: func(h guardHarness) { h.raw(t, "list-first", " \tTrue\tNone\n") },
			want:  "an id this script cannot read",
		},
		{
			name: "a build row with a fourth field",
			stage: func(h guardHarness) {
				h.page(t, "first", []string{"billet-test:aaa"}, "")
				h.raw(t, "batch-billet-test:aaa", "billet-test:aaa\tSUCCEEDED\t2026-09-02T01:37:02.895000+00:00\textra\n")
			},
			want: "more fields than were asked for",
		},
		{
			name: "a start time with a plausible prefix and garbage after it",
			stage: func(h guardHarness) {
				h.page(t, "first", []string{"billet-test:aaa"}, "")
				h.raw(t, "batch-billet-test:aaa", "billet-test:aaa\tSUCCEEDED\t0000-00-00T00:00:00garbage\n")
			},
			want: "start time",
		},
		{
			name: "a start time rendered in a zone other than UTC",
			stage: func(h guardHarness) {
				h.page(t, "first", []string{"billet-test:aaa"}, "")
				h.raw(t, "batch-billet-test:aaa", "billet-test:aaa\tSUCCEEDED\t2026-09-01T18:37:02.895000-07:00\n")
			},
			want: "start time",
		},
		{
			name: "a start time with an impossible calendar",
			stage: func(h guardHarness) {
				h.page(t, "first", []string{"billet-test:aaa"}, "t2")
				h.raw(t, "batch-billet-test:aaa", "billet-test:aaa\tSUCCEEDED\t2026-00-15T00:00:00.000000+00:00\n")
				h.page(t, "t2", []string{"billet-test:run"}, "")
				h.batch(t, "billet-test:run", []build{{id: "billet-test:run", status: "IN_PROGRESS", start: recent}}, nil)
			},
			want: "start time",
		},
		{
			name: "a start time whose day does not exist in its month",
			stage: func(h guardHarness) {
				h.page(t, "first", []string{"billet-test:aaa"}, "t2")
				h.raw(t, "batch-billet-test:aaa", "billet-test:aaa\tSUCCEEDED\t2026-02-30T00:00:00.000000+00:00\n")
				h.page(t, "t2", []string{"billet-test:run"}, "")
				h.batch(t, "billet-test:run", []build{{id: "billet-test:run", status: "IN_PROGRESS", start: recent}}, nil)
			},
			want: "start time",
		},
		{
			name: "a wire-format start time that is not a number",
			stage: func(h guardHarness) {
				h.page(t, "first", []string{"billet-test:aaa"}, "")
				h.raw(t, "batch-billet-test:aaa", "billet-test:aaa\tSUCCEEDED\t1756778222.8x\n")
			},
			want: "start time",
		},
		{
			name: "a batch that omits a build it was asked about",
			stage: func(h guardHarness) {
				h.page(t, "first", []string{"billet-test:aaa", "billet-test:bbb"}, "")
				h.batch(t, "billet-test:aaa", []build{{id: "billet-test:aaa", status: "SUCCEEDED", start: recent}}, nil)
			},
			want: "answered about 1 of the 2 builds",
		},
		{
			name: "a batch that answers about a build it was not asked about",
			stage: func(h guardHarness) {
				h.page(t, "first", []string{"billet-test:aaa"}, "")
				h.batch(t, "billet-test:aaa", []build{
					{id: "billet-test:aaa", status: "SUCCEEDED", start: recent},
					{id: "billet-test:zzz", status: "SUCCEEDED", start: recent},
				}, nil)
			},
			want: "did not ask about",
		},
		{
			name: "a batch that answers twice about one build",
			stage: func(h guardHarness) {
				h.page(t, "first", []string{"billet-test:aaa"}, "")
				h.batch(t, "billet-test:aaa", []build{
					{id: "billet-test:aaa", status: "SUCCEEDED", start: recent},
					{id: "billet-test:aaa", status: "SUCCEEDED", start: recent},
				}, nil)
			},
			want: "answered twice",
		},
		{
			name: "a start time that is not a timestamp",
			stage: func(h guardHarness) {
				h.page(t, "first", []string{"billet-test:aaa"}, "")
				h.raw(t, "batch-billet-test:aaa", "billet-test:aaa\tSUCCEEDED\t2026/09/02 01:37:02\n")
			},
			want: "start time",
		},
		{
			name: "a batch with no output at all",
			stage: func(h guardHarness) {
				h.page(t, "first", []string{"billet-test:aaa"}, "")
				h.raw(t, "batch-billet-test:aaa", "")
			},
			want: "answered about 0 of the 1 builds",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			h := newGuardHarness(t, true)
			tc.stage(h)

			out, err := h.run(t)
			if err == nil {
				t.Fatalf("an answer the script cannot account for let the destroy proceed:\n%s", out)
			}

			if !strings.Contains(out, tc.want) {
				t.Errorf("the refusal does not say %q:\n%s", tc.want, out)
			}
		})
	}
}

// THE WAIVER IS THE OPERATOR'S ASSERTION AND CHECKS NOTHING: no CLI call is made, and
// the output says so rather than reading like a pass.
func TestTheWaiverSkipsTheCheckAndSaysSo(t *testing.T) {
	t.Parallel()

	h := newGuardHarness(t, true)
	h.page(t, "first", []string{"billet-test:aaa"}, "")
	h.batch(t, "billet-test:aaa", []build{{id: "billet-test:aaa", status: "IN_PROGRESS", start: recent}}, nil)

	out, err := h.run(t, "BILLET_SKIP_ACTIVE_BUILD_GUARD=1")
	if err != nil {
		t.Fatalf("the waiver did not let the destroy proceed (%v):\n%s", err, out)
	}

	if !strings.Contains(out, "nothing was checked") {
		t.Errorf("the waiver reads like a pass:\n%s", out)
	}

	if calls := h.calls(t); calls != "" {
		t.Errorf("the waiver still asked the CLI:\n%s", calls)
	}
}

// THE WALK ENDS AT THE WINDOW'S EDGE, NOT AT THE END OF A YEAR OF HISTORY. A page whose
// every build started before the abandon cutoff ends the walk, and the page behind it
// is never requested — asserted from the fake's log, because a walk that quietly read
// one more page would pass every other test here.
func TestTheWalkStopsOnceEveryBuildOnAPageIsOld(t *testing.T) {
	t.Parallel()

	h := newGuardHarness(t, true)
	h.page(t, "first", []string{"billet-test:old1", "billet-test:old2"}, "t2")
	h.batch(t, "billet-test:old1", []build{
		{id: "billet-test:old1", status: "SUCCEEDED", start: ancient},
		{id: "billet-test:old2", status: "FAILED", start: ancient},
	}, nil)
	// The page behind it would refuse if read; the fake also fails loudly if asked
	// for a page it does not have, so give it one and check the log instead.
	h.page(t, "t2", []string{"billet-test:run"}, "")
	h.batch(t, "billet-test:run", []build{{id: "billet-test:run", status: "IN_PROGRESS", start: recent}}, nil)

	out, err := h.run(t)
	if err != nil {
		t.Fatalf("the walk did not stop at the window's edge (%v):\n%s", err, out)
	}

	if strings.Contains(h.calls(t), "--next-token t2") {
		t.Errorf("the walk read past a page that was entirely outside the window:\n%s", h.calls(t))
	}
}

// AND A PAGE WITH ONE RECENT BUILD DOES NOT END IT, even if that build is terminal:
// the listing is ordered by submission, and a build that queued for hours started
// after one submitted later. The running build on the second page must be found.
func TestTheWalkContinuesWhileAPageHasARecentBuild(t *testing.T) {
	t.Parallel()

	h := newGuardHarness(t, true)
	h.page(t, "first", []string{"billet-test:old1", "billet-test:new1"}, "t2")
	h.batch(t, "billet-test:old1", []build{
		{id: "billet-test:old1", status: "SUCCEEDED", start: ancient},
		{id: "billet-test:new1", status: "SUCCEEDED", start: recent},
	}, nil)
	h.page(t, "t2", []string{"billet-test:run"}, "")
	h.batch(t, "billet-test:run", []build{{id: "billet-test:run", status: "IN_PROGRESS", start: recent}}, nil)

	out, err := h.run(t)
	if err == nil {
		t.Fatalf("the walk stopped before the page holding the running build:\n%s", out)
	}

	if !strings.Contains(h.calls(t), "--next-token t2") {
		t.Errorf("the second page was never requested:\n%s", h.calls(t))
	}

	if !strings.Contains(out, "billet-test:run(IN_PROGRESS)") {
		t.Errorf("the running build on the second page was not what refused:\n%s", out)
	}
}

// A BUILD WITH NO START TIME IS INSIDE THE WINDOW: it is SUBMITTED or QUEUED and about
// to run somebody's job, and it is also not terminal — so it refuses on its own, and it
// keeps the walk going.
func TestAQueuedBuildIsInsideTheWindowAndNotTerminal(t *testing.T) {
	t.Parallel()

	h := newGuardHarness(t, true)
	h.page(t, "first", []string{"billet-test:queued"}, "")
	h.batch(t, "billet-test:queued", []build{{id: "billet-test:queued", status: "IN_PROGRESS"}}, nil)

	out, err := h.run(t)
	if err == nil {
		t.Fatalf("a queued build let the destroy proceed:\n%s", out)
	}

	if !strings.Contains(out, "billet-test:queued(IN_PROGRESS)") {
		t.Errorf("the refusal does not name the queued build:\n%s", out)
	}
}

// A TOKEN THAT SPELLS "None" IS A TOKEN. Tokens are opaque, so nothing rules one out,
// and before the presence flag existed such a page ended the walk over the page it
// named — the fake serves that page from list-None, and the running build there is
// what must refuse.
func TestALiteralNoneTokenIsFollowed(t *testing.T) {
	t.Parallel()

	h := newGuardHarness(t, true)
	h.page(t, "first", []string{"billet-test:aaa"}, "None")
	h.batch(t, "billet-test:aaa", []build{{id: "billet-test:aaa", status: "SUCCEEDED", start: recent}}, nil)
	h.page(t, "None", []string{"billet-test:run"}, "")
	h.batch(t, "billet-test:run", []build{{id: "billet-test:run", status: "IN_PROGRESS", start: recent}}, nil)

	out, err := h.run(t)
	if err == nil {
		t.Fatalf("a token spelling None ended the walk before the page it named:\n%s", out)
	}

	if !strings.Contains(h.calls(t), "--next-token None") {
		t.Errorf("the page behind the None token was never requested:\n%s", h.calls(t))
	}
}

// AN EMPTY PAGE WITH A TOKEN IS A PAGE TO WALK PAST, the provider's own rule: treating
// it as the end turned an incomplete listing into a short successful inventory.
func TestAnEmptyPageWithATokenDoesNotEndTheGuardsWalk(t *testing.T) {
	t.Parallel()

	h := newGuardHarness(t, true)
	h.page(t, "first", nil, "t2")
	h.page(t, "t2", []string{"billet-test:run"}, "")
	h.batch(t, "billet-test:run", []build{{id: "billet-test:run", status: "IN_PROGRESS", start: recent}}, nil)

	out, err := h.run(t)
	if err == nil {
		t.Fatalf("an empty first page ended the walk before the running build:\n%s", out)
	}
}

// THE CLI IS ASKED IN UTC. It renders timestamps in the machine's zone — measured on a
// Pacific laptop as -07:00 — and the script compares them lexically against a UTC
// cutoff, so a guard that forgot to pin TZ would be seven hours wrong here and right on
// a UTC server, which is the worst kind of wrong.
func TestTheGuardPinsTheCLIToUTC(t *testing.T) {
	t.Parallel()

	h := newGuardHarness(t, true)
	h.page(t, "first", []string{"billet-test:aaa"}, "")
	h.batch(t, "billet-test:aaa", []build{{id: "billet-test:aaa", status: "SUCCEEDED", start: recent}}, nil)

	if out, err := h.run(t, "TZ=America/Los_Angeles"); err != nil {
		t.Fatalf("run: %v\n%s", err, out)
	}

	// AND IN THE C LOCALE, because sort(1) follows the locale's collation and the
	// timestamp comparison is chronological only under byte order.
	for _, line := range strings.Split(strings.TrimSpace(h.calls(t)), "\n") {
		if !strings.HasPrefix(line, "TZ=UTC LC_ALL=C ::") {
			t.Errorf("the CLI was invoked without TZ=UTC and LC_ALL=C: %s", line)
		}
	}
}

// THE MODULE WIRES THE SCRIPT AT DESTROY TIME, AND THE GUARD DEPENDS ON THE PROJECT.
// tftest cannot see provisioner blocks or dependency edges, so the file is read: a
// guard whose provisioner ran at create time, or that did not depend on the project,
// would be torn down after it and refuse nothing.
func TestTheModuleRunsTheGuardBeforeDestroyingTheProject(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile("../terraform/modules/billet/modules/fleet-codebuild/main.tf")
	if err != nil {
		t.Fatalf("read main.tf: %v", err)
	}

	src := string(data)

	start := strings.Index(src, `resource "terraform_data" "active_build_guard"`)
	if start < 0 {
		t.Fatal("main.tf has no terraform_data.active_build_guard")
	}

	// The resource body runs to the next top-level resource.
	body := src[start:]
	if end := strings.Index(body[1:], "\nresource "); end >= 0 {
		body = body[:end+1]
	}

	// WHOLE BLOCKS, NOT SUBSTRINGS: a commented-out line or an extra trigger would
	// still satisfy a substring test, and an extra trigger that is unknown at plan
	// would replace the guard on every apply. `triggers_replace` IS LOAD-BEARING: a
	// terraform_data's `input` updates in place, so with the project in `input` alone
	// a rename replaces the PROJECT while merely updating the guard, and the
	// provisioner never runs. A review found exactly that.
	for _, want := range []string{
		"  triggers_replace = [\n" +
			"    local.project_name,\n" +
			"    local.region,\n" +
			"    var.name,\n" +
			"    local.parameter_path,\n" +
			"    local.log_group_name,\n" +
			"    local.build_role_name,\n" +
			"    local.create_project,\n" +
			"    local.create_log_group,\n" +
			"    local.create_fleet,\n" +
			"    local.create_fleet_network_role,\n" +
			"    local.create_kms,\n" +
			"  ]\n",
		"  depends_on = [\n" +
			"    aws_codebuild_project.this,\n" +
			"    aws_codebuild_fleet.this,\n" +
			"    aws_kms_key.registrations,\n" +
			"    aws_iam_role_policy.node,\n" +
			"    aws_iam_instance_profile.node,\n" +
			"    aws_iam_role_policy.build,\n" +
			"    aws_iam_role_policy.fleet,\n" +
			"    aws_cloudwatch_log_group.this,\n" +
			"  ]\n",
		"  provisioner \"local-exec\" {\n" +
			"    when    = destroy\n" +
			"    command = \"\\\"$BILLET_GUARD_SCRIPT\\\"\"\n" +
			"\n" +
			"    environment = {\n" +
			"      BILLET_GUARD_SCRIPT  = \"${path.module}/scripts/refuse-active-builds.sh\"\n" +
			"      BILLET_GUARD_PROJECT = self.input.project\n" +
			"      BILLET_GUARD_REGION  = self.input.region\n" +
			"    }\n" +
			"  }\n",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the guard resource does not carry this block exactly:\n%s", want)
		}
	}

	if _, err := os.Stat(guardScript); err != nil {
		t.Fatalf("the script the module names is not there: %v", err)
	}

	// THE SHARED SELECTOR IS SHARED: the fleet's network role and its policy were
	// counted on `create_fleet && use_vpc` written out twice, which is how removing
	// the VPC destroyed both with the guard untouched. The trigger entry above is
	// only a guard while the local means what it says and both counts use it.
	if !strings.Contains(src, "  create_fleet_network_role = local.create_fleet && local.use_vpc\n") {
		t.Error("local.create_fleet_network_role is not defined as create_fleet && use_vpc")
	}

	iam, err := os.ReadFile("../terraform/modules/billet/modules/fleet-codebuild/iam.tf")
	if err != nil {
		t.Fatalf("read iam.tf: %v", err)
	}

	// EACH OF THE TWO BLOCKS, not two occurrences anywhere: an unrelated resource
	// picking the selector up while one of these dropped it would keep a bare count
	// green.
	for _, header := range []string{`resource "aws_iam_role" "fleet"`, `resource "aws_iam_role_policy" "fleet"`} {
		block := resourceBlock(t, string(iam), header)
		if strings.Count(block, "  count = local.create_fleet_network_role ? 1 : 0\n") != 1 {
			t.Errorf("%s does not count on local.create_fleet_network_role exactly once:\n%s", header, block)
		}
	}

	if strings.Contains(string(iam), "local.create_fleet && local.use_vpc") {
		t.Error("iam.tf spells the fleet network selector out again instead of using the shared local")
	}
}

// resourceBlock returns the text of one top-level resource block, up to the next.
func resourceBlock(t *testing.T, src, header string) string {
	t.Helper()

	start := strings.Index(src, header)
	if start < 0 {
		t.Fatalf("no %s in the file", header)
	}

	block := src[start:]
	if end := strings.Index(block[1:], "\nresource "); end >= 0 {
		block = block[:end+1]
	}

	return block
}
