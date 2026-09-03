package runnerrelease

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ReleasesURL is the release history billet reads.
//
// THE HISTORY RATHER THAN /releases/latest, AND THAT IS THE WHOLE POINT OF THIS
// FILE. GitHub's window opens when the FIRST release newer than the installed one
// appears, so the newest release's date is the wrong number: a runner that missed
// two releases has been on the clock since the first of them, and counting from the
// second moves a deadline that has already passed. Measured against real dates:
// 2.334.0 went out of date when 2.335.0 was published on 2026-06-08, so its window
// closed on 2026-07-08 — and the old calculation reported 2026-08-19, because
// 2.336.0 landed on 2026-07-20. A fleet GitHub had stopped queueing to for six
// weeks read as having a month left.
//
// UNAUTHENTICATED, because this asks a question about an open-source project rather
// than about the operator's own account, and needing a token to find out whether
// your fleet is about to stop working would put this check behind exactly the
// credential most likely to be missing on the machine running it.
const ReleasesURL = "https://api.github.com/repos/actions/runner/releases"

// How much history one Resolve will read.
//
// BOUNDED, because an unauthenticated caller gets 60 requests an hour and this runs
// on a timer. Two pages is 200 releases — MEASURED, one page of 100 reaches back
// FIVE YEARS, from 2026-08 to v2.281.0 in 2021-08 — and a version older than the
// window is answered as UNKNOWN rather than by paging until something gives.
// maxPages is the reason Freshness.InstalledKnown exists, and that answer is still
// conclusive where it matters: a version older than the whole window has certainly
// passed a deadline counted from the oldest release billet can see.
//
// WHERE THE WALK STOPS, AND WHY THAT IS SOUND RATHER THAN CONVENIENT. The list is
// newest-first by publication, so it stops at the INSTALLED release: everything
// below is published earlier, and a release that was ALREADY AVAILABLE when yours
// shipped is not an update that became available afterwards — which is what GitHub's
// rule is about. That bound is what makes the walk terminate at all: actions/runner
// has published hundreds of releases and no budget could read "all" of them.
//
// IT IS NOT A STOP AT THE INSTALLED VERSION NUMBER, which is the unsound version of
// the same idea and the thing that has to be kept apart from it. Version order and
// publication order differ — MEASURED on one real page, v2.285.3 was published
// 2023-01-30, eleven days AFTER the higher v2.301.1 on 2023-01-19 — so a scan that
// stopped when it saw a higher NUMBER would miss the release that opened the window.
// This stops when it sees the installed RELEASE, by which point everything that
// became available after it has already been read.
//
// The cost is measured: each record carries its whole release-notes body and there
// is no way to ask for less, so a page is about 5 MB and reaches back five years.
// The budget exists for a fleet whose runner is older than that, and running out of
// it is reported (HistoryComplete) rather than treated as an ending.
const (
	perPage  = 100
	maxPages = 2
)

// maxPageBytes bounds one response.
//
// TWELVE TIMES THE MEASURED PAGE. This is a response from somewhere else and the
// alternative is letting an unexpected one decide how much memory a scheduled check
// allocates — but a bound close to the real size is its own outage, since exceeding
// it silently truncates and the check then fails forever on a JSON parse error that
// names nothing. Truncation is reported as itself below.
//
// A VARIABLE ONLY SO A TEST CAN SHRINK IT: proving the refusal against the real
// bound would mean generating 32 MB, and a guard with no test is a guard nobody
// knows is broken.
var maxPageBytes int64 = 32 << 20

// release is the half of GitHub's response this needs.
type release struct {
	TagName     string    `json:"tag_name"`
	PublishedAt time.Time `json:"published_at"`
	Draft       bool      `json:"draft"`
	Prerelease  bool      `json:"prerelease"`
}

// Freshness is what the release history says about one installed runner.
//
// ONE MODEL, AND EVERY CALLER ASKS IT. `billet runner check` and `billet images
// pull` used to compute a deadline each, from two different timestamps, and neither
// was the one GitHub counts from. A second calculation is a second answer.
type Freshness struct {
	// Installed is the version the fleet actually runs, as this package read it.
	//
	// REPORTED BY THE CALLER RATHER THAN ITS OWN COPY, which is the one-representation
	// rule this repository has been bitten by three times in internal/config: Resolve
	// normalizes what it was given (a leading "v", surrounding space) and answers about
	// the result, so a command printing the string it passed IN would attribute this
	// answer to a version it was not computed for.
	Installed string

	// Latest is the newest stable release, which is what a rebuild takes up. It is
	// NOT what the deadline is counted from.
	//
	// ITS PUBLICATION DATE IS DELIBERATELY NOT CARRIED. It decides nothing — the
	// deadline comes from FirstNewerPublished — so a field holding it would be one
	// nothing reads and one more value a caller could reach for by mistake, which
	// is exactly the substitution this package exists to remove.
	Latest string

	// FirstNewer is the earliest stable release newer than Installed — the one that
	// started GitHub's clock — and FirstNewerPublished is when it appeared. Both are
	// zero when nothing newer exists.
	//
	// EARLIEST BY PUBLICATION, NOT BY VERSION. A patch backported after a later
	// minor release is newer than Installed and did not start the clock; the
	// release that did is whichever appeared first.
	FirstNewer          string
	FirstNewerPublished time.Time

	// HistoryComplete says the walk finished because it had read everything it
	// needed, rather than because it ran out of budget.
	//
	// THREE WAYS IT IS TRUE and none of them is "read every release GitHub has": the
	// installed release was found (everything below it was published earlier and was
	// already available when it shipped), a page came back shorter than requested, or
	// an empty one did.
	//
	// WHAT IT PROMISES, EXACTLY: every release published after the installed one was
	// read, so FirstNewer and the deadline are settled. `Latest` is settled over that
	// plus the remainder of the page the installed release was on — which is where a
	// higher version published earlier realistically sits, and is free because those
	// records are already fetched. A higher version published earlier AND more than
	// one page further back would be missed, and the fleet would read as current
	// while sitting on an old maintenance branch; closing that costs a page per check
	// forever to catch a case nobody has produced.
	//
	// FALSE MEANS OLDER RELEASES WERE NEVER READ, and one of them could be newer than
	// Installed and published EARLIER than FirstNewer — the same thing that makes
	// stopping at the installed tag unsound (see maxPages). The true window would
	// then have opened before the one billet found, so the computed deadline is LATER
	// than the truth.
	//
	// WHICH DIRECTION THAT SPOILS IS THE POINT. An unseen earlier opener only moves
	// the deadline EARLIER, so Expired and Due can still only under-report: a proved
	// expiry stays a proof. What it costs is the other direction — "not expired" and
	// "current" are no longer conclusive — which is why a caller that would report
	// nothing-to-do has to look at this.
	//
	// In practice one page reaches back FIVE YEARS (measured), so this is false only
	// for a fleet whose runner predates everything two pages cover; such a runner is
	// expired many times over and the sound direction already says so. It is carried
	// because the claim has to match what was read, not because the gap is likely.
	HistoryComplete bool

	// InstalledKnown says the history billet read actually named Installed.
	//
	// THE THIRD ANSWER, AND IT DECIDES WHICH SENTENCES ARE PROOFS. False means the
	// installed release is older than everything fetched, so the true clock started
	// at or BEFORE FirstNewerPublished: Expired and Due stay sound (they can only
	// under-report), while Remaining is an over-estimate and must be spoken of as
	// "at most". Collapsing this into "current" is the false negative this package
	// exists to remove; collapsing it into "expired" would refuse a fleet that is
	// fine.
	InstalledKnown bool
}

// BehindWithoutAWindow reports the state where something newer exists and no
// ordinary window was derived for it.
//
// IT IS REACHABLE AND IT IS NOT A DEADLINE. A higher version published EARLIER than
// the installed release sits below the floor: it is read as evidence that the fleet
// is behind (Latest), and it is deliberately not read as an opener, because it was
// already available when the installed release shipped. So `Current()` is false
// while `FirstNewer` is empty, and every timed answer — Deadline, Remaining, Due,
// Expired — is meaningless.
//
// NAMED, BECAUSE THE CALLERS BOTH FELL THROUGH TO THEIR TIMED BRANCH and printed an
// empty release name, the year 0001 and a negative number of days, then exited 0.
// A state that reads as success while rendering nonsense is worse than either
// answer.
func (f Freshness) BehindWithoutAWindow() bool { return !f.Current() && f.FirstNewer == "" }

// Current reports that the history billet fetched holds no release newer than the
// installed one.
//
// ASKED OF THE NEWEST RELEASE RATHER THAN OF THE WINDOW-OPENER. They are different
// questions — "is this fleet behind" is about versions, and a release can be newer
// while having been published earlier — so Deadline, Due and Expired key on
// FirstNewer, which is what they are actually about, and this keys on Latest.
//
// AND IT IS A BOUNDED CLAIM, WHICH NO DESIGN CAN AVOID: proving that nothing newer
// EXISTS means reading every release GitHub has ever published, and the walk reads a
// bounded window. So the answer is about what was read, and how much that is worth
// depends on HistoryComplete — which is why a caller consults that first.
//
// With the floor reached it covers everything published after the installed release,
// which is where an ordinary newer release is. It is best-effort for a higher
// version published BEFORE it, since such a release sits further down a list of
// unbounded length: the rest of the floor's page is read for exactly that case, and
// one page past it when the floor ends a page. Without the floor — the budget ran
// out — it covers only the window that was fetched, and HistoryComplete is false.
func (f Freshness) Current() bool { return f.Latest == "" || !Older(f.Installed, f.Latest) }

// Deadline is when GitHub stops queueing jobs to the installed release, under the
// ordinary window. It is zero when nothing newer was found.
//
// COUNTED FROM THE EARLIEST NEWER RELEASE, which is what FirstNewerPublished holds
// and is the whole point of the model. A caller must ask Current() first: a zero
// here means the question does not apply, not that the deadline is the epoch.
func (f Freshness) Deadline() time.Time {
	if f.FirstNewer == "" {
		return time.Time{}
	}

	return f.FirstNewerPublished.Add(Grace)
}

// Expired reports whether GitHub will already refuse to send jobs.
func (f Freshness) Expired(now time.Time) bool {
	return f.FirstNewer != "" && !now.Before(f.Deadline())
}

// Due reports whether the image should be rebuilt now to stay inside the window.
func (f Freshness) Due(now time.Time) bool {
	return f.FirstNewer != "" && !now.Before(f.FirstNewerPublished.Add(Warn))
}

// Remaining is how long is left before GitHub stops queueing jobs. It is negative
// once that has happened, meaningless when the installed release is current, and an
// upper bound rather than an answer when InstalledKnown is false.
func (f Freshness) Remaining(now time.Time) time.Duration { return f.Deadline().Sub(now) }

// Resolve asks GitHub where an installed runner sits in the release history.
//
// A FAILURE HERE IS NOT A VERDICT. Every caller has to be able to tell "this fleet
// is out of date" apart from "billet could not find out", because the second is an
// ordinary thing that happens to a machine with no egress and must not be reported
// as a fleet about to stop working.
func Resolve(ctx context.Context, client *http.Client, installed string) (Freshness, error) {
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}

	return resolveFrom(ctx, client, ReleasesURL, installed)
}

func resolveFrom(
	ctx context.Context,
	client *http.Client,
	endpoint, installed string,
) (Freshness, error) {
	installed = strings.TrimPrefix(strings.TrimSpace(installed), "v")

	if !isStableVersion(installed) {
		return Freshness{}, fmt.Errorf("runnerrelease: %q is not a runner release, so nothing "+
			"can say where it sits in github's history", installed)
	}

	f := Freshness{Installed: installed}

	// atFloor says the installed record has been passed. Everything after it was
	// published earlier, so it no longer opens a window — but it is still evidence
	// about which release is NEWEST, which is a different question.
	//
	// sawPastFloor says at least one record after it has actually been read. The
	// floor can land on the LAST record of a full page, and then the page that
	// settles the window question has produced no evidence at all about a higher
	// version published earlier — which sits below it, on the next page. One more
	// page is fetched in that case and in no other: it costs nothing in the ordinary
	// run, where the floor is somewhere in the middle of page one.
	atFloor, sawPastFloor := false, false

	for page := 1; ; page++ {
		if page > maxPages {
			// THE BUDGET RAN OUT WITH THE HISTORY STILL GOING. Recorded rather than
			// treated as an ending, because the two are different facts and only one
			// of them supports "nothing to do".
			break
		}

		releases, err := fetchPage(ctx, client, endpoint, page)
		if err != nil {
			return Freshness{}, err
		}

		if len(releases) == 0 {
			f.HistoryComplete = true

			break
		}

		for _, rel := range releases {
			version := strings.TrimPrefix(strings.TrimSpace(rel.TagName), "v")

			// A PRERELEASE IS NOT AN AVAILABLE UPDATE. GitHub's rule is about
			// releases, and counting a release candidate would start a clock nobody
			// is expected to answer — and would name it as the remediation.
			if rel.Draft || rel.Prerelease || !isStableVersion(version) {
				continue
			}

			if f.Latest == "" || Older(f.Latest, version) {
				f.Latest = version
			}

			if version == installed {
				f.InstalledKnown = true
				atFloor = true

				continue
			}

			// PAST THE FLOOR, ONLY `Latest` KEEPS BEING ANSWERED. Every record here was
			// published earlier and was already available when the installed release
			// shipped, so under the rule maxPages states none of them opened its
			// window -- and letting one replace FirstNewer would move the deadline
			// EARLIER than the model says it is.
			//
			// BUT IT IS STILL EVIDENCE THAT THE FLEET IS BEHIND, which is what Latest
			// and Current() are about and is a different question from when a window
			// opened. Stopping the record loop outright answered "current" for a fleet
			// sitting on an old maintenance branch: a higher version published before
			// the installed one is below it in the list, so it was never seen at all.
			if atFloor {
				sawPastFloor = true

				continue
			}

			if !Older(installed, version) {
				continue
			}

			// A RELEASE THAT STARTS A CLOCK MUST SAY WHEN. Skipping a dated-less one
			// would silently drop the release the deadline is counted from, and the
			// answer would then be later than the truth — which is the direction that
			// reports an expired fleet as healthy.
			if rel.PublishedAt.IsZero() {
				return Freshness{}, fmt.Errorf("runnerrelease: github's history does not say "+
					"when %s was published, and the deadline for %s is counted from it",
					version, installed)
			}

			if f.FirstNewer == "" || rel.PublishedAt.Before(f.FirstNewerPublished) {
				f.FirstNewer, f.FirstNewerPublished = version, rel.PublishedAt
			}
		}

		// THE INSTALLED RELEASE IS THE FLOOR, and the walk stops once it has actually
		// LOOKED past it: those records are already fetched, they cost nothing to
		// read, and they are where a higher version published earlier will be. When
		// the floor was the page's last record there is nothing past it to have seen,
		// so one more page is read — for Latest only, since atFloor stays set.
		//
		// THE WINDOW QUESTION IS SETTLED EITHER WAY, which is what HistoryComplete
		// governs: everything published after the installed release is above it and
		// has been read.
		// AND ONLY ONCE IT HAS ACTUALLY LOOKED. Setting this the moment the floor is
		// found and then continuing leaves it TRUE when the next iteration runs out of
		// budget — so a page that was never read is reported as one that did not need
		// to be. The layouts that reach it are real: the floor as the last record of
		// the last allowed page, or a following page holding nothing but prereleases.
		if f.InstalledKnown && sawPastFloor {
			f.HistoryComplete = true

			break
		}

		// A SHORT PAGE IS THE END OF THE HISTORY, so there is nothing further back to
		// look at and the installed release is genuinely absent from it.
		if len(releases) < perPage {
			f.HistoryComplete = true

			break
		}
	}

	if f.Latest == "" {
		return Freshness{}, fmt.Errorf("runnerrelease: github's history names no stable release, "+
			"so nothing can say whether %s is still accepted", installed)
	}

	return f, nil
}

func fetchPage(
	ctx context.Context,
	client *http.Client,
	endpoint string,
	page int,
) ([]release, error) {
	target, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("runnerrelease: %q is not a release history url: %w", endpoint, err)
	}

	query := target.Query()
	query.Set("per_page", strconv.Itoa(perPage))
	query.Set("page", strconv.Itoa(page))
	target.RawQuery = query.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("runnerrelease: build the request: %w", err)
	}

	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-Github-Api-Version", "2022-11-28")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("runnerrelease: ask github for the runner releases: %w", err)
	}

	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxPageBytes+1))
	if err != nil {
		return nil, fmt.Errorf("runnerrelease: read github's answer: %w", err)
	}

	// TRUNCATION IS REPORTED AS ITSELF. Reading exactly the bound and handing the
	// prefix to a decoder turns "the page was bigger than expected" into "unexpected
	// end of JSON input", which names neither the cause nor anything to do — and it
	// would be permanent, since the next check reads the same oversized page.
	if int64(len(body)) > maxPageBytes {
		return nil, fmt.Errorf("runnerrelease: github's page %d of the release history is "+
			"larger than the %d bytes this reads, so it was not parsed rather than parsed "+
			"in part", page, maxPageBytes)
	}

	if resp.StatusCode != http.StatusOK {
		// RATE LIMITING IS NAMED, because it is the answer an unauthenticated check
		// gets on a busy machine, and "403" alone sends somebody looking for a
		// permissions problem that is not there.
		if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests {
			return nil, fmt.Errorf("runnerrelease: github refused the request (%s), which "+
				"unauthenticated is usually its rate limit rather than a permission: %s",
				resp.Status, firstLine(string(body)))
		}

		return nil, fmt.Errorf("runnerrelease: github answered %s: %s",
			resp.Status, firstLine(string(body)))
	}

	var releases []release
	if err := json.Unmarshal(body, &releases); err != nil {
		return nil, fmt.Errorf("runnerrelease: parse github's answer: %w", err)
	}

	return releases, nil
}

// isStableVersion reports whether a tag is an ordinary three-part release.
//
// ANYTHING ELSE IS NOT COMPARED AT ALL. actions/runner has published tags that are
// not releases in this sense, and a version billet cannot order is one it must not
// place relative to the installed one — silently guessing would either invent a
// deadline or hide one.
func isStableVersion(v string) bool {
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return false
	}

	for _, part := range parts {
		if part == "" {
			return false
		}

		if _, err := strconv.Atoi(part); err != nil {
			return false
		}
	}

	return true
}

// Older reports whether a is an earlier release than b.
//
// COMPARED NUMERICALLY, PART BY PART, because these are versions rather than
// strings: "2.9.0" sorts after "2.10.0" lexically and is older in fact, and picking
// the wrong one means watching the tier that is fine while the stale one expires.
//
// HERE RATHER THAN IN THE COMMAND THAT FIRST NEEDED IT. `billet runner check` used
// this to find the oldest tier and the history calculation needs the same order;
// two comparators is one comparator that is wrong.
//
// SOMETHING THAT IS NOT A VERSION STILL GETS AN ORDER. This runs over metadata
// recorded on a generation, which is whatever was written there, on a scheduled
// path where crashing is a worse answer than a stable guess. Callers that need a
// real version ask isStableVersion first.
func Older(a, b string) bool {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")

	for i := 0; i < len(as) && i < len(bs); i++ {
		an, aerr := strconv.Atoi(as[i])
		bn, berr := strconv.Atoi(bs[i])

		if aerr != nil || berr != nil {
			return a < b
		}

		if an != bn {
			return an < bn
		}
	}

	return len(as) < len(bs)
}

// firstLine keeps a foreign error message to something that fits in a log line.
func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}

	if len(s) > 200 {
		s = s[:200] + "…"
	}

	return s
}
