package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/cli"
	"github.com/junioryono/billet/internal/config"
	storecontract "github.com/junioryono/billet/internal/store"
)

// fakeCurrent answers each key from a script of answers, one per ask, repeating the last,
// and records every key it was asked.
type fakeCurrent struct {
	mu      sync.Mutex
	answers map[string][]fakeAnswer
	asked   []string
}

type fakeAnswer struct {
	generation string
	err        error
}

func (f *fakeCurrent) current(_ context.Context, key string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, key)
	script := f.answers[key]
	if len(script) == 0 {
		return "", storecontract.ErrMiss
	}
	a := script[0]
	if len(script) > 1 {
		f.answers[key] = script[1:]
	}

	return a.generation, a.err
}

const publishedNS = "dep1/site-a"

// A PUBLICATION IS WAITED FOR, AND THE ANSWER IS THREE-VALUED: every architecture
// published is success; a miss to the end is "not published"; an answer that was not an
// answer, last, is "could not tell", never a miss. The keys asked are the image store's
// for each guest's own name of its architecture.
func TestTheWarmImageRunsWaitForTheColdRunsPublication(t *testing.T) {
	t.Parallel()
	x64, arm := publishedNS+"/docker-images/amd64", publishedNS+"/docker-images/arm64"
	broken := errors.New("s3: connection reset")
	for name, c := range map[string]struct {
		answers map[string][]fakeAnswer
		code    int
		says    []string
	}{
		"both published at once": {answers: map[string][]fakeAnswer{
			x64: {{generation: "snap-x"}}, arm: {{generation: "snap-a"}}}},
		"one published only after a while": {answers: map[string][]fakeAnswer{
			x64: {{generation: "snap-x"}}, arm: {{err: storecontract.ErrMiss}, {err: broken}, {generation: "snap-a"}}}},
		"a miss to the end": {answers: map[string][]fakeAnswer{
			x64: {{generation: "snap-x"}}, arm: {{err: storecontract.ErrMiss}}},
			code: publishedMissing, says: []string{"arm64: no image store is published"}},
		"an empty pointer is a miss": {answers: map[string][]fakeAnswer{
			x64: {{generation: "snap-x"}}, arm: {{generation: ""}}},
			code: publishedMissing, says: []string{"arm64: no image store"}},
		"a store that could not be asked, last": {answers: map[string][]fakeAnswer{
			x64: {{err: storecontract.ErrMiss}, {err: broken}}, arm: {{generation: "snap-a"}}},
			code: publishedCouldNot, says: []string{"x64: the store could not be asked", "connection reset"}},
		"one missing and one unaskable": {answers: map[string][]fakeAnswer{
			x64: {{err: broken}}, arm: {{err: storecontract.ErrMiss}}},
			code: publishedCouldNot, says: []string{"x64: the store could not be asked", "arm64: no image store"}},
	} {
		f := &fakeCurrent{answers: c.answers}
		var out bytes.Buffer
		err := waitPublished(t.Context(), &out, f.current, publishedNS, []config.Arch{config.ArchX64, config.ArchARM64},
			50*time.Millisecond, time.Millisecond, time.Hour)
		switch {
		case c.code == 0 && err != nil:
			t.Errorf("%s: %v", name, err)
		case c.code != 0 && cli.ExitStatus(err) != c.code:
			t.Errorf("%s: exit %d (%v), want %d", name, cli.ExitStatus(err), err, c.code)
		}
		for _, s := range c.says {
			if err == nil || !strings.Contains(err.Error(), s) {
				t.Errorf("%s: %v does not say %q", name, err, s)
			}
		}
		if c.code == 0 && (!strings.Contains(out.String(), "x64: the image store is published as snap-x, after ") ||
			!strings.Contains(out.String(), "arm64: the image store is published as snap-a, after ")) {
			t.Errorf("%s: printed %q", name, out.String())
		}
		for _, k := range f.asked {
			if k != x64 && k != arm {
				t.Errorf("%s: asked for %q, which is no architecture's image store", name, k)
			}
		}
		// A PUBLISHED ARCHITECTURE IS NOT ASKED AGAIN.
		if n := slices.Index(f.asked, x64); c.answers[x64][0].generation != "" && n >= 0 &&
			slices.Index(f.asked[n+1:], x64) >= 0 {
			t.Errorf("%s: asked for x64's store again after it was published", name)
		}
	}
}

// THE NOTES SAY A MISS APART FROM A STORE THAT COULD NOT BE ASKED, with the error, and say
// nothing of an architecture already published.
func TestTheWaitsNotesSayWhatEachArchitectureLastAnswered(t *testing.T) {
	t.Parallel()
	x64, arm := publishedNS+"/docker-images/amd64", publishedNS+"/docker-images/arm64"
	broken := errors.New("s3: connection reset")
	for name, c := range map[string]struct {
		answers map[string][]fakeAnswer
		says    []string
		never   []string
	}{
		"a miss and a store not asked": {
			answers: map[string][]fakeAnswer{x64: {{err: storecontract.ErrMiss}}, arm: {{err: broken}}},
			says: []string{"x64: no image store is published yet, after ",
				"arm64: the store could not be asked, after ", "connection reset"},
			never: []string{"x64: the store could not be asked", "arm64: no image store is published yet"},
		},
		"one already published": {
			answers: map[string][]fakeAnswer{x64: {{generation: "snap-x"}}, arm: {{err: storecontract.ErrMiss}}},
			says:    []string{"arm64: no image store is published yet, after "},
			never:   []string{"x64: no image store", "x64: the store could not be asked"},
		},
	} {
		f := &fakeCurrent{answers: c.answers}
		var out bytes.Buffer
		if err := waitPublished(t.Context(), &out, f.current, publishedNS,
			// A SECOND, not 50 ms: the deadline is checked before a note, so a first pass
			// slowed past the deadline (the race suite, coverage) would print none.
			[]config.Arch{config.ArchX64, config.ArchARM64}, time.Second, time.Millisecond,
			time.Millisecond); err == nil {
			t.Errorf("%s: the wait passed with an architecture never published", name)
		}
		for _, s := range c.says {
			if !strings.Contains(out.String(), s) {
				t.Errorf("%s: the notes %q do not say %q", name, out.String(), s)
			}
		}
		for _, s := range c.never {
			if strings.Contains(out.String(), s) {
				t.Errorf("%s: the notes %q say %q", name, out.String(), s)
			}
		}
	}
}

// A STORE THAT WAS NEVER ASKED IS COULD-NOT-TELL, NEVER A MISS: whatever stops the
// command before it asks (its flags, a missing workspace) answers 2, which the lane reads
// apart from a cold run that published nothing.
func TestAPublicationNeverAskedForIsNotAMiss(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	env := cli.Env{Stdout: &out, Stderr: &out}
	missing := filepath.Join(t.TempDir(), "no-workspace")
	for name, args := range map[string][]string{
		"no workspace":            {"--archs", "x64", "--workspace", missing},
		"no architecture":         {"--workspace", missing},
		"a wait that is no wait":  {"--archs", "x64", "--wait", "0s", "--workspace", missing},
		"an architecture unknown": {"--archs", "sparc", "--workspace", missing},
	} {
		if err := cmdMatrixPublished(t.Context(), env, args); cli.ExitStatus(err) != publishedCouldNot {
			t.Errorf("%s: exit %d (%v), want %d", name, cli.ExitStatus(err), err, publishedCouldNot)
		}
	}
}

// THE ARCHITECTURE'S NAME IS THE GUEST'S: the image store's key ends with what the
// guest's docker-cache.sh sends, which maps uname's x86_64 and aarch64.
func TestTheMatrixNamesEachArchitectureAsTheGuestDoes(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join("..", "..", "internal", "guestassets", "docker-cache.sh"))
	if err != nil {
		t.Fatal(err)
	}
	for arch, uname := range map[config.Arch]string{config.ArchX64: `x86_64`, config.ArchARM64: `aarch64[^)]*`} {
		line := regexp.MustCompile(`(?m)^\s*` + uname + `\)\s*architecture=([a-z0-9]+)\s*;;`).FindSubmatch(raw)
		if line == nil {
			t.Fatalf("docker-cache.sh maps no %s", uname)
		}
		if got := matrixGuestArch[arch]; got != string(line[1]) {
			t.Errorf("%s: the matrix names it %q, the guest %q", arch, got, line[1])
		}
	}
	if len(matrixGuestArch) != len(matrixArchs) {
		t.Errorf("the matrix names %d architectures for the guest and declares %d", len(matrixGuestArch),
			len(matrixArchs))
	}
}
