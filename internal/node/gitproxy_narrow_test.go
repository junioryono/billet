package node

import (
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/provider"
)

// A REFRESH FETCHES ONLY WHAT MOVED: a mirror is made with every ref, and a
// later refresh asks GitHub for the refs its advertisement moved, offers the
// default branch as what the mirror has, so a rebased branch brings only its
// own commits, and removes the refs GitHub dropped; the advertisement after it
// is still answered from the mirror.
func TestARefreshFetchesOnlyWhatMoved(t *testing.T) {
	t.Parallel()

	upstream := newGitUpstream(t)
	first := runIn(t, upstream.repo, "rev-parse", "main")
	branch := func(name, parent string) string {
		commit := runIn(t, upstream.repo, "commit-tree", parent+"^{tree}", "-p", parent, "-m", name)
		runIn(t, upstream.repo, "update-ref", "refs/heads/"+name, commit)

		return commit
	}
	branch("feature", first)
	branch("other", first)
	// A BRANCH NOTHING MOVED, whose tip no other ref reaches: git's own
	// negotiation would offer it, and the refresh's tips never do.
	unrelated := branch("unrelated", first)
	main := upstream.commitTo(t, "main", map[string]string{"m": "2\n"})
	service, node, token := gitNode(t, upstream)
	if output, err := gitClient(t, node, token, githubBasic)("clone", "-q",
		"https://github.com/acme/api.git", "c"); err != nil {
		t.Fatalf("clone: %v\n%s", err, output)
	}
	if made := strings.Join(upstream.takeMirrorRequests(), ""); !strings.Contains(made, "ref-prefix refs/\n") {
		t.Fatalf("the new mirror was not fetched whole: %q", made)
	}

	rebased := branch("feature", main)
	// A DROPPED REF'S NAME TAKEN BY A NEW ONE beneath it, which the mirror can
	// hold only once the dropped one is gone.
	runIn(t, upstream.repo, "update-ref", "-d", "refs/heads/other")
	nested := branch("other/nested", main)
	observer := &recordingGitObserver{}
	service.SetGitObserver(observer)
	advertise(t, node, token, "api")

	asked := upstream.takeMirrorRequests()
	all := strings.Join(asked, "")
	// NOT EVEN EVERY TAG, which git lists to follow tags into what it fetched
	// unless told not to: a tag that moved is a moved ref of its own.
	if strings.Contains(all, "ref-prefix refs/\n") || strings.Contains(all, "ref-prefix refs/tags/\n") ||
		!strings.Contains(all, "ref-prefix refs/heads/feature\n") {
		t.Errorf("the refresh did not ask for the moved ref alone: %q", all)
	}
	if !strings.Contains(all, "want "+rebased) {
		t.Errorf("the refresh did not want the rebased branch %s: %q", rebased, all)
	}
	if !strings.Contains(all, "have "+main) {
		t.Errorf("the refresh did not offer the default branch %s as a tip: %q", main, all)
	}
	if strings.Contains(all, "have "+unrelated) {
		t.Errorf("the refresh offered a branch that did not move, %s, so its tips were not used: %q", unrelated, all)
	}
	refs := mirrorRefsOf(t, service.git.mirrorPath("untrusted/acme/api"))
	if refs["refs/heads/feature"] != rebased {
		t.Errorf("the mirror's feature is %s, want %s", refs["refs/heads/feature"], rebased)
	}
	if _, ok := refs["refs/heads/other"]; ok {
		t.Error("the mirror still has the branch GitHub dropped")
	}
	if refs["refs/heads/other/nested"] != nested {
		t.Errorf("the mirror's other/nested is %q, want %s", refs["refs/heads/other/nested"], nested)
	}
	if requests, _, _ := observer.seen(); !slices.Equal(requests, []string{"mirror served 2xx"}) {
		t.Errorf("the advertisement after the refresh was counted as %v, want served from the mirror", requests)
	}
}

// AN ADVERTISEMENT ASKED FOR BEFORE THE LAST REFRESH FINISHED NEVER REMOVES A
// REF: that refresh may have brought a branch GitHub made after the
// advertisement was answered, which read narrowly would look dropped, so such
// a refresh fetches GitHub's refs whole. Both an older advertisement and the
// very one whose own whole fetch brought the branch are held to it.
func TestAnOlderAdvertisementNeverRemovesANewerRef(t *testing.T) {
	t.Parallel()

	for name, c := range map[string]struct {
		// cold is a mirror the first refresh makes, fetching every ref.
		cold bool
		// newer is whether the refresh that brings the branch answers a
		// question asked after the branch was made.
		newer bool
	}{
		"an older advertisement":      {newer: true},
		"its own advertisement again": {cold: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			upstream := newGitUpstream(t)
			service, node, token := gitNode(t, upstream)
			if !c.cold {
				if output, err := gitClient(t, node, token, githubBasic)("clone", "-q",
					"https://github.com/acme/api.git", "c"); err != nil {
					t.Fatalf("clone: %v\n%s", err, output)
				}
			}
			request := gitRequest{session: service.sessionOf(provider.InstanceName(scopedLease)),
				owner: "acme", repo: "api", auth: gitAuth(githubBasic)}
			path := service.git.mirrorPath(request.key())
			refresh := func(refs map[string]string, sequence uint64) {
				t.Helper()

				started := service.startRefresh(t.Context(), request, upstream.repositoryID("api"),
					service.git.mirror(request.key()), "refs/heads/main", refs, sequence)
				if err := gitWithin(t, "a refresh", func() error {
					<-started.done

					return started.err
				}); err != nil {
					t.Fatalf("a refresh: %v", err)
				}
			}

			// THE ADVERTISEMENT WITHOUT THE BRANCH, answered before it was made.
			asked, before := service.git.ask(), mirrorRefsOf(t, upstream.repo)
			added := upstream.commitTo(t, "added", map[string]string{"a": "1\n"})
			if c.newer {
				refresh(mirrorRefsOf(t, upstream.repo), service.git.ask())
			} else {
				refresh(before, asked)
			}
			if got := mirrorRefsOf(t, path)["refs/heads/added"]; got != added {
				t.Fatalf("the refresh that should bring the branch left it at %q, want %s", got, added)
			}
			refresh(before, asked)
			if got := mirrorRefsOf(t, path)["refs/heads/added"]; got != added {
				t.Fatalf("an advertisement asked before the last refresh finished left the branch at %q, want %s",
					got, added)
			}
		})
	}
}

// A NARROW FETCH THAT FAILS IS DONE AGAIN WHOLE: a ref GitHub dropped after
// advertising it refuses a fetch that names it.
func TestANarrowFetchThatFailsIsDoneWhole(t *testing.T) {
	t.Parallel()

	upstream := newGitUpstream(t)
	service, node, token := gitNode(t, upstream)
	if output, err := gitClient(t, node, token, githubBasic)("clone", "-q",
		"https://github.com/acme/api.git", "c"); err != nil {
		t.Fatalf("clone: %v\n%s", err, output)
	}
	pushed := upstream.commitTo(t, "main", map[string]string{"m": "2\n"})
	request := gitRequest{session: service.sessionOf(provider.InstanceName(scopedLease)),
		owner: "acme", repo: "api", auth: gitAuth(githubBasic)}
	path := service.git.mirrorPath(request.key())

	plan, err := service.fetchMirror(t.Context(), path, request, upstream.repositoryID("api"), 1<<40,
		gitFetchPlan{narrow: true, moved: []string{"refs/heads/dropped"}})
	if err != nil {
		t.Fatalf("a narrow fetch of a dropped ref was not done again whole: %v", err)
	}
	if plan.narrow {
		t.Error("the fetch says it was narrow after it fell back to every ref")
	}
	if got := mirrorRefsOf(t, path)["refs/heads/main"]; got != pushed {
		t.Errorf("the whole fetch left main at %s, want %s", got, pushed)
	}
}

// WHAT A REFRESH FETCHES is decided from the mirror's refs and GitHub's: an
// empty mirror, a ref no refspec may name, or more refs moved than the bound
// are fetched whole; otherwise only what moved, from the old values and the
// default branch, with what GitHub dropped removed.
func TestTheFetchPlanFollowsWhatMoved(t *testing.T) {
	t.Parallel()

	a, b, c, d := strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("c", 40), strings.Repeat("d", 40)
	mirror := map[string]string{"refs/heads/main": a, "refs/heads/feature": b, "refs/heads/old": c}
	github := map[string]string{"refs/heads/main": a, "refs/heads/feature": d, "refs/pull/1/head": d}

	plan := planGitFetch(mirror, github, "refs/heads/main", gitNarrowMost)
	if !plan.narrow || !slices.Equal(plan.moved, []string{"refs/heads/feature", "refs/pull/1/head"}) ||
		!slices.Equal(plan.tips, []string{a, b}) || !maps.Equal(plan.gone, map[string]string{"refs/heads/old": c}) {
		t.Errorf("planGitFetch = %+v", plan)
	}
	for name, whole := range map[string]gitFetchPlan{
		"an empty mirror":    planGitFetch(map[string]string{}, github, "refs/heads/main", gitNarrowMost),
		"more than the most": planGitFetch(mirror, github, "refs/heads/main", 2),
		"an option":          planGitFetch(mirror, map[string]string{"-x": d}, "refs/heads/main", gitNarrowMost),
		"a refspec's colon":  planGitFetch(mirror, map[string]string{"refs/heads/a:b": d}, "refs/heads/main", gitNarrowMost),
		"a pattern":          planGitFetch(mirror, map[string]string{"refs/heads/*": d}, "refs/heads/main", gitNarrowMost),
		"a space":            planGitFetch(mirror, map[string]string{"refs/heads/a b": d}, "refs/heads/main", gitNarrowMost),
		"a parent":           planGitFetch(mirror, map[string]string{"refs/heads/../x": d}, "refs/heads/main", gitNarrowMost),
	} {
		if whole.narrow {
			t.Errorf("%s was planned narrow: %+v", name, whole)
		}
	}
}

// mirrorRefsOf is the refs of the bare repository at path.
func mirrorRefsOf(t *testing.T, path string) map[string]string {
	t.Helper()

	refs := make(map[string]string)
	for line := range strings.SplitSeq(runIn(t, path, "for-each-ref", "--format=%(objectname) %(refname)"), "\n") {
		if object, name, ok := strings.Cut(line, " "); ok {
			refs[name] = object
		}
	}

	return refs
}
