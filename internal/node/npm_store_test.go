package node

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// FRESHNESS IS COUNTED FROM THE REGISTRY'S CLOCK (RFC 9111 4.2.3): an answer
// npm's edge had held for 72 seconds of its 300 has 228 left here, not 300.
func TestAnEntrysFreshnessCountsDownFromTheRegistrysAge(t *testing.T) {
	t.Parallel()

	received := time.Date(2026, 10, 22, 21, 33, 0, 0, time.UTC)
	header := http.Header{
		"Cache-Control": {"public, max-age=300"},
		"Age":           {"72"},
		"Date":          {received.Format(http.TimeFormat)},
	}
	entry := npmEntry{Object: npmPackageDocument}
	npmFreshness(&entry, header, received.Add(-500*time.Millisecond), received)
	if entry.InitialAge < 72*time.Second || entry.InitialAge > 73*time.Second {
		t.Fatalf("initial age = %s, want the registry's 72s plus the request's delay", entry.InitialAge)
	}
	if !entry.fresh(received.Add(227 * time.Second)) {
		t.Fatal("an entry with 228s of freshness left is stale after 227s")
	}
	if entry.fresh(received.Add(229 * time.Second)) {
		t.Fatal("an entry is fresh past the registry's own lifetime")
	}
	// WITHIN THE STALE HOUR AND WITHOUT A DIRECTIVE AGAINST IT, a document may
	// answer a registry that gave no answer; not after.
	if !entry.staleUsable(received.Add(30 * time.Minute)) {
		t.Fatal("a document half an hour past expiry may not answer a silent registry")
	}
	if entry.staleUsable(received.Add(2 * time.Hour)) {
		t.Fatal("a document past the stale hour answers a silent registry")
	}
}

// A SHARED CACHE READS s-maxage BEFORE max-age, and s-maxage, like
// must-revalidate, forbids reusing what the registry's lifetime has ended.
func TestSharedMaxAgeDecidesTheLifetimeAndForbidsAStaleAnswer(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name, cacheControl string
		lifetime           time.Duration
		staleAllowed       bool
	}{
		{"max-age only", "public, max-age=300", 300 * time.Second, true},
		{"s-maxage wins", "public, max-age=300, s-maxage=60", 60 * time.Second, false},
		{"must-revalidate", "public, max-age=300, must-revalidate", 300 * time.Second, false},
		{"proxy-revalidate", "public, max-age=300, proxy-revalidate", 300 * time.Second, false},
		{"no lifetime at all", "public", 0, true},
		{"an unreadable max-age is zero", "max-age=soon", 0, true},
	} {
		entry := npmEntry{Object: npmPackageDocument}
		npmFreshness(&entry, http.Header{"Cache-Control": {tc.cacheControl}}, now, now)
		if entry.Lifetime != tc.lifetime {
			t.Errorf("%s: lifetime = %s, want %s", tc.name, entry.Lifetime, tc.lifetime)
		}
		if got := entry.staleUsable(now.Add(tc.lifetime + time.Minute)); got != tc.staleAllowed {
			t.Errorf("%s: stale answer allowed = %v, want %v", tc.name, got, tc.staleAllowed)
		}
	}
}

// A TARBALL'S YEAR IS CAPPED AT A DAY, which is billet's ceiling and not the
// registry's expiry: past the day it is asked for again, but a registry that
// gives no answer may still be answered from it for the stale hour, because
// by the registry's own lifetime it is still fresh.
func TestATarballIsAskedForAgainAfterADay(t *testing.T) {
	t.Parallel()

	received := time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)
	entry := npmEntry{Object: npmTarball}
	npmFreshness(&entry, http.Header{"Cache-Control": {"public, must-revalidate, max-age=31557600"}},
		received, received)
	if !entry.fresh(received.Add(23 * time.Hour)) {
		t.Fatal("a tarball is stale within its first day")
	}
	if entry.fresh(received.Add(25 * time.Hour)) {
		t.Fatal("a tarball is served for more than a day without asking the registry")
	}
	if !entry.staleUsable(received.Add(24*time.Hour + 30*time.Minute)) {
		t.Fatal("a tarball the registry still calls fresh may not answer a silent registry")
	}
	if entry.staleUsable(received.Add(26 * time.Hour)) {
		t.Fatal("a tarball past billet's ceiling and the stale hour answers a silent registry")
	}
}

func TestOnlyAShareableAnswerIsKept(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		header http.Header
		want   bool
	}{
		{"public", http.Header{"Cache-Control": {"public, max-age=300"}}, true},
		{"private", http.Header{"Cache-Control": {"private, max-age=300"}}, false},
		{"no-store", http.Header{"Cache-Control": {"no-store"}}, false},
		{"no-cache", http.Header{"Cache-Control": {"public, no-cache"}}, false},
		{"a cookie", http.Header{"Cache-Control": {"public"}, "Set-Cookie": {"a=b"}}, false},
	} {
		if got := npmShareable(tc.header); got != tc.want {
			t.Errorf("%s: shareable = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// writeNpmTemp writes body to a fill's temporary file.
func writeNpmTemp(t *testing.T, store *npmStore, body string) string {
	t.Helper()

	temp, err := store.temp()
	if err != nil {
		t.Fatalf("temp: %v", err)
	}
	if _, err := temp.WriteString(body); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := temp.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	return temp.Name()
}

// A STORE SURVIVES THE NODE: what it committed is there after a reopen, and
// what a crash left half written (a body no entry names, an entry whose body is
// the wrong size, a fill's temporary file) is gone.
func TestTheStoreKeepsWhatItCommittedAndDropsWhatACrashLeft(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	store, err := openNpmStore(root, 1<<20)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	now := time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)
	kept := npmEntry{Key: "full /left-pad", Path: "/left-pad", Object: npmPackageDocument, Variant: npmFull,
		Size: 5, Received: now, Lifetime: time.Minute}
	if err := store.commit(kept, writeNpmTemp(t, store, "hello")); err != nil {
		t.Fatalf("commit: %v", err)
	}
	torn := npmEntry{Key: "full /torn", Path: "/torn", Object: npmPackageDocument, Variant: npmFull,
		Size: 4, Received: now}
	if err := store.commit(torn, writeNpmTemp(t, store, "four")); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := os.WriteFile(filepath.Join(store.bodiesDir(), storedBody(t, store, torn.Key)), []byte("cut"),
		0o600); err != nil {
		t.Fatalf("tear a body: %v", err)
	}
	orphan := filepath.Join(store.bodiesDir(), "orphan")
	if err := os.WriteFile(orphan, []byte("x"), 0o600); err != nil {
		t.Fatalf("write an orphan: %v", err)
	}
	leftover := writeNpmTemp(t, store, "half a fill")

	reopened, err := openNpmStore(root, 1<<20)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	entry, body, ok := reopened.acquire(kept.Key, now)
	if !ok || entry.Size != 5 {
		t.Fatalf("a committed entry after reopen = %+v, %v", entry, ok)
	}
	_ = body.Close()
	if reopened.held(torn.Key) {
		t.Fatal("an entry whose body is the wrong size was loaded")
	}
	for _, gone := range []string{orphan, leftover} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Errorf("%s survived a reopen: %v", gone, err)
		}
	}
}

// A STORE PAST ITS BOUND GIVES UP WHAT WAS SERVED LEAST RECENTLY.
func TestTheStoreEvictsWhatWasServedLeastRecently(t *testing.T) {
	t.Parallel()

	store, err := openNpmStore(t.TempDir(), 10)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	start := time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)
	for i, name := range []string{"a", "b"} {
		entry := npmEntry{Key: "full /" + name, Path: "/" + name, Object: npmPackageDocument,
			Variant: npmFull, Size: 4, Received: start.Add(time.Duration(i) * time.Second)}
		if err := store.commit(entry, writeNpmTemp(t, store, "four")); err != nil {
			t.Fatalf("commit %s: %v", name, err)
		}
	}
	_, body, ok := store.acquire("full /a", start.Add(time.Minute))
	if !ok {
		t.Fatal("serve a: not held")
	}
	_ = body.Close()
	third := npmEntry{Key: "full /c", Path: "/c", Object: npmPackageDocument, Variant: npmFull, Size: 4,
		Received: start.Add(2 * time.Minute)}
	if err := store.commit(third, writeNpmTemp(t, store, "four")); err != nil {
		t.Fatalf("commit c: %v", err)
	}
	if store.held("full /b") || !store.held("full /a") || !store.held("full /c") {
		t.Fatalf("after eviction: a=%v b=%v c=%v, want b evicted",
			store.held("full /a"), store.held("full /b"), store.held("full /c"))
	}
}

// storedBody is the body file the index names for key.
func storedBody(t *testing.T, store *npmStore, key string) string {
	t.Helper()

	store.mu.Lock()
	defer store.mu.Unlock()
	entry, ok := store.entries[key]
	if !ok {
		t.Fatalf("%s is not stored", key)
	}

	return entry.Body
}

// A REMOVAL NAMES THE GENERATION IT MEANT: one that a replacement superseded
// leaves the replacement, its metadata and its body alone, so an eviction or a
// failed read of an old body can never take a newer answer with it.
func TestARemovalOfASupersededGenerationLeavesTheReplacement(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	store, err := openNpmStore(root, 1<<20)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	now := time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)
	entry := npmEntry{Key: "full /x", Path: "/x", Object: npmPackageDocument, Variant: npmFull, Size: 3,
		Received: now}
	if err := store.commit(entry, writeNpmTemp(t, store, "old")); err != nil {
		t.Fatalf("commit: %v", err)
	}
	old := storedBody(t, store, entry.Key)
	if err := store.commit(entry, writeNpmTemp(t, store, "new")); err != nil {
		t.Fatalf("replace: %v", err)
	}
	store.removeIf(entry.Key, old)
	got, body, ok := store.acquire(entry.Key, now)
	if !ok {
		t.Fatal("removing the superseded generation removed its replacement")
	}
	defer body.Close()
	raw, err := io.ReadAll(body)
	if err != nil || string(raw) != "new" || got.Body == old {
		t.Fatalf("after removing the old generation: body %q (%v), generation %s", raw, err, got.Body)
	}
	reopened, err := openNpmStore(root, 1<<20)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if !reopened.held(entry.Key) {
		t.Fatal("the replacement's metadata did not survive the old generation's removal")
	}
}

// A FILL PAST ITS BUDGET IS NOT KEPT, and gives the budget back when it ends.
func TestTheFillBudgetRefusesToKeepAndIsGivenBack(t *testing.T) {
	t.Parallel()

	store, err := openNpmStore(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	store.fillLimit = 10
	within := &npmBudget{store: store, keep: true}
	if _, err := within.Write([]byte("six by")); err != nil || !within.keep {
		t.Fatalf("a fill within the budget: keep %v, %v", within.keep, err)
	}
	past := &npmBudget{store: store, keep: true}
	if _, err := past.Write([]byte("five!")); err != nil || past.keep {
		t.Fatalf("a fill past the budget: keep %v, %v", past.keep, err)
	}
	within.done()
	past.done()
	again := &npmBudget{store: store, keep: true}
	if _, err := again.Write([]byte("ten bytes!")); err != nil || !again.keep {
		t.Fatal("the budget was not given back when its fills ended")
	}
	again.done()
}
