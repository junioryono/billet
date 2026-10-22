package node

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/provider"
)

// FORGETTING A REPOSITORY STOPS EVERY MIRROR OF IT BEING SERVED AT ONCE, in
// every trust class and whatever the case of its name, and nothing else; the
// node removes it itself, at the next advertisement, which fetches a new one
// and is served from it.
func TestForgettingARepositoryStopsItsMirrorsBeingServed(t *testing.T) {
	t.Parallel()

	upstream := newGitUpstream(t)
	service, node, token := gitNode(t, upstream)
	if output, err := gitClient(t, node, token, githubBasic)("clone", "-q",
		"https://github.com/acme/api.git", "c"); err != nil {
		t.Fatalf("clone: %v\n%s", err, output)
	}
	key := "untrusted/acme/api"
	awaitIdleMirror(t, service, key)
	stateDir := filepath.Dir(service.git.root)
	for _, other := range []string{"trusted/ACME/Api.git", "untrusted/acme/other.git", "untrusted/acme/api-two.git"} {
		if err := os.MkdirAll(filepath.Join(service.git.root, other), 0o700); err != nil {
			t.Fatal(err)
		}
	}

	forgotten, err := ForgetGitMirrors(stateDir, "Acme", "API")
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(forgotten)
	if want := []string{"trusted/ACME/Api", "untrusted/acme/api"}; !slices.Equal(forgotten, want) {
		t.Fatalf("forgot %v, want %v", forgotten, want)
	}
	if _, _, err := mirrorRepository(service.git.mirrorPath(key)); !errors.Is(err, errMirrorBroken) {
		t.Fatalf("the forgotten mirror reads %v, want broken", err)
	}
	for _, kept := range []string{"untrusted/acme/other.git", "untrusted/acme/api-two.git"} {
		if _, err := os.Stat(filepath.Join(service.git.root, kept, gitBrokenStamp)); !os.IsNotExist(err) {
			t.Errorf("forgetting acme/api marked %s too: %v", kept, err)
		}
	}

	observer := &recordingGitObserver{}
	service.SetGitObserver(observer)
	if body := negotiate(t, node, token, githubBasic, runIn(t, upstream.repo, "rev-parse", "main")); !strings.Contains(string(body), "PACK") {
		t.Fatalf("the fetch after forgetting was not answered: %q", body[:min(len(body), 80)])
	}
	advertise(t, node, token, "api")
	requests := gitWithin(t, "the two counted requests", func() []string {
		for t.Context().Err() == nil {
			if requests, _, _ := observer.seen(); len(requests) >= 2 {
				return requests
			}
			time.Sleep(10 * time.Millisecond)
		}

		return nil
	})
	if want := []string{"github mirror_not_servable 2xx", "mirror served 2xx"}; !slices.Equal(requests, want) {
		t.Errorf("the fetch and the advertisement after forgetting were %v, want %v", requests, want)
	}
	if _, exists, err := mirrorRepository(service.git.mirrorPath(key)); !exists || err != nil {
		t.Errorf("the mirror fetched in the forgotten one's place is there %v and reads %v", exists, err)
	}
}

// THE NODE'S REAPER REMOVES A FORGOTTEN MIRROR however recently it was used, so
// one no job asks for again goes too.
func TestTheReaperRemovesAForgottenMirror(t *testing.T) {
	t.Parallel()

	upstream := newGitUpstream(t)
	service, node, token := gitNode(t, upstream)
	if output, err := gitClient(t, node, token, githubBasic)("clone", "-q",
		"https://github.com/acme/api.git", "c"); err != nil {
		t.Fatalf("clone: %v\n%s", err, output)
	}
	key := "untrusted/acme/api"
	awaitIdleMirror(t, service, key)
	stateDir := filepath.Dir(service.git.root)
	// ONE A FAILED REFRESH LEFT UNVERIFIED, with no repository or used stamp.
	failed := service.git.mirrorPath("untrusted/acme/failed")
	if err := os.MkdirAll(failed, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := markUnverified(failed); err != nil {
		t.Fatal(err)
	}
	for _, repo := range []string{"api", "failed"} {
		if _, err := ForgetGitMirrors(stateDir, "acme", repo); err != nil {
			t.Fatal(err)
		}
	}
	if err := service.ReapGitMirrors(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{service.git.mirrorPath(key), failed} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("the reaper left the forgotten mirror %s: %v", path, err)
		}
	}

	// A HEALTHY MIRROR FETCHED IN ITS PLACE is not the reaper's, whatever an
	// earlier walk found at that path.
	advertise(t, node, token, "api")
	awaitIdleMirror(t, service, key)
	if err := service.reapBrokenMirror(t.Context(), key, service.git.mirrorPath(key)); err != nil {
		t.Fatal(err)
	}
	if _, exists, err := mirrorRepository(service.git.mirrorPath(key)); !exists || err != nil {
		t.Fatalf("the reaper took the healthy mirror fetched in a forgotten one's place (there %v): %v", exists, err)
	}
}

// A MIRROR IN ANOTHER CASE IS THE SAME MIRROR, as it is on a filesystem that
// ignores case: forgetting finds api.GIT and refuses a link so named, and the
// reaper takes a mirror found as Acme/Api only under the hold requests take
// for acme/api, so it never removes one a fetch is being served from.
func TestAMirrorInAnotherCaseIsTheSameMirror(t *testing.T) {
	t.Parallel()

	service, _, _, _ := casService(t, provider.TrustUntrusted, gitCache(), &fakeCacheStore{})
	stateDir := filepath.Dir(service.git.root)
	upper := filepath.Join(service.git.root, "untrusted", "Acme", "Api.GIT")
	if err := os.MkdirAll(upper, 0o700); err != nil {
		t.Fatal(err)
	}
	forgotten, err := ForgetGitMirrors(stateDir, "acme", "api")
	if err != nil || !slices.Equal(forgotten, []string{"untrusted/Acme/Api"}) {
		t.Fatalf("forgetting acme/api forgot %v, %v; want the mirror spelled Acme/Api.GIT", forgotten, err)
	}

	held := service.git.mirror("untrusted/acme/api")
	held.use.RLock()
	if err := service.ReapGitMirrors(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(upper); err != nil {
		t.Errorf("the reaper took a mirror a fetch was being served from, under another case's hold: %v", err)
	}
	held.use.RUnlock()
	if err := service.ReapGitMirrors(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(upper); !os.IsNotExist(err) {
		t.Errorf("the reaper left a forgotten mirror spelled in another case once free: %v", err)
	}

	link := filepath.Join(service.git.root, "untrusted", "acme", "api.GIT")
	if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), link); err != nil {
		t.Fatal(err)
	}
	if _, err := ForgetGitMirrors(stateDir, "acme", "api"); err == nil {
		t.Error("forgetting through a link named api.GIT was not refused")
	}
}

// ONLY A NAME GITHUB COULD GIVE IS FORGOTTEN, typed or found on disk, so no
// path reaches outside the mirrors and no lookalike folds into a match; a link
// is refused rather than passed over; and a node with no mirrors has none to
// forget.
func TestForgettingRefusesWhatIsNotAMirrorName(t *testing.T) {
	t.Parallel()

	stateDir := t.TempDir()
	for _, name := range [][2]string{{"..", "x"}, {"acme", ".."}, {"acme", ".hidden"}, {"a/b", "c"}, {"acme", ""}} {
		if _, err := ForgetGitMirrors(stateDir, name[0], name[1]); err == nil {
			t.Errorf("forgetting %s/%s was not refused", name[0], name[1])
		}
	}
	if forgotten, err := ForgetGitMirrors(stateDir, "acme", "api"); err != nil || len(forgotten) != 0 {
		t.Errorf("a node with no mirrors forgot %v, %v", forgotten, err)
	}

	root := filepath.Join(stateDir, gitMirrorsDirectory)
	lookalike := filepath.Join(root, "untrusted", "acme", "ſ.git") // the long s, which folds to s
	if err := os.MkdirAll(lookalike, 0o700); err != nil {
		t.Fatal(err)
	}
	if forgotten, err := ForgetGitMirrors(stateDir, "acme", "s"); err != nil || len(forgotten) != 0 {
		t.Errorf("forgetting acme/s forgot %v, %v", forgotten, err)
	}
	if _, err := os.Stat(filepath.Join(lookalike, gitBrokenStamp)); !os.IsNotExist(err) {
		t.Errorf("forgetting acme/s marked a lookalike: %v", err)
	}

	// EACH LINK IN A TREE OF ITS OWN, so no directory already there takes its
	// name on a filesystem that ignores case.
	for name, link := range map[string]string{
		"a trust class": "linked",
		"an owner":      filepath.Join("untrusted", "Acme"),
		"a mirror":      filepath.Join("untrusted", "acme", "api.git"),
	} {
		stateDir := t.TempDir()
		root := filepath.Join(stateDir, gitMirrorsDirectory)
		link = filepath.Join(root, link)
		if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
			t.Fatal(err)
		}
		target := t.TempDir()
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		if _, err := ForgetGitMirrors(stateDir, "acme", "api"); err == nil {
			t.Errorf("forgetting through %s that is a link was not refused", name)
		}
		if _, err := os.Stat(filepath.Join(target, gitBrokenStamp)); !os.IsNotExist(err) {
			t.Errorf("forgetting followed %s that is a link: %v", name, err)
		}
	}
}
