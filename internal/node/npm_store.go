package node

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/junioryono/billet/internal/durablefile"
)

const (
	// npmStoreLimit bounds what the node's npm store keeps. The store is the
	// node's, shared by every tier on it, so the bound is the node's too.
	npmStoreLimit = 10 << 30
	// npmFillLimit bounds the bytes every fill in flight may have written toward
	// an entry. A fill past it goes on to its client and keeps nothing, so a job
	// asking for many large objects at once cannot fill the node's disk.
	npmFillLimit = 2 << 30
	// npmTarballCeiling is billet's own ceiling on how long a tarball is served
	// without asking the registry again, far shorter than the registry's year, so
	// a version npm's edge stops serving stops being served here within a day.
	npmTarballCeiling = 24 * time.Hour
	// npmStaleWindow is how long past its expiry an entry may answer a request
	// the registry gave no answer to.
	npmStaleWindow = time.Hour
	// npmEntryLimit bounds one entry's metadata file on disk.
	npmEntryLimit = 64 << 10
)

// npmEntry is one stored answer: the registry's own headers that a client needs,
// and what the store needs to say whether it is still fresh.
type npmEntry struct {
	Key          string     `json:"key"`
	Path         string     `json:"path"`
	Object       npmObject  `json:"object"`
	Variant      npmVariant `json:"variant,omitempty"`
	ContentType  string     `json:"content_type,omitempty"`
	ETag         string     `json:"etag,omitempty"`
	LastModified string     `json:"last_modified,omitempty"`
	CacheControl string     `json:"cache_control,omitempty"`
	Size         int64      `json:"size"`
	Body         string     `json:"body"`
	// Received is the node's clock when the answer arrived or was last
	// revalidated, and InitialAge its age then, as RFC 9111 corrects it.
	Received   time.Time     `json:"received"`
	InitialAge time.Duration `json:"initial_age"`
	// Lifetime is the registry's own: s-maxage when it named one, else max-age.
	Lifetime     time.Duration `json:"lifetime"`
	SharedMaxAge bool          `json:"shared_max_age,omitempty"`
	// MustRevalidate is must-revalidate or proxy-revalidate.
	MustRevalidate bool `json:"must_revalidate,omitempty"`

	lastServed time.Time
}

// age is the entry's current age.
func (e npmEntry) age(now time.Time) time.Duration {
	return e.InitialAge + max(0, now.Sub(e.Received))
}

// registryFresh reports whether the registry's own lifetime still holds.
func (e npmEntry) registryFresh(now time.Time) bool { return e.age(now) < e.Lifetime }

// expiry is when the store stops serving the entry without asking: the
// registry's expiry, or billet's ceiling on a tarball when that comes first.
func (e npmEntry) expiry() time.Time {
	end := e.Received.Add(e.Lifetime - e.InitialAge)
	if e.Object == npmTarball {
		end = minTime(end, e.Received.Add(npmTarballCeiling))
	}

	return end
}

// fresh reports whether the entry may be served without asking the registry.
func (e npmEntry) fresh(now time.Time) bool { return now.Before(e.expiry()) }

// staleUsable reports whether the entry may answer a request the registry gave
// no answer to: within the stale window of its expiry, and, once the registry's
// own lifetime has ended, only when the registry's directives allow reuse.
func (e npmEntry) staleUsable(now time.Time) bool {
	if !now.Before(e.expiry().Add(npmStaleWindow)) {
		return false
	}
	if e.registryFresh(now) {
		return true
	}

	return !e.MustRevalidate && !e.SharedMaxAge
}

func minTime(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}

	return a
}

// npmCacheControl is what the store reads of a Cache-Control header.
type npmCacheControl struct {
	maxAge, sharedMaxAge                      time.Duration
	hasMaxAge, hasSharedMaxAge                bool
	private, noStore, noCache, mustRevalidate bool
}

// parseNpmCacheControl reads every Cache-Control line of an answer. A directive
// it cannot read is ignored, as RFC 9111 says, except that an unreadable
// max-age or s-maxage counts as zero: never fresher than the registry said.
func parseNpmCacheControl(values []string) npmCacheControl {
	var cc npmCacheControl
	for _, value := range values {
		for _, part := range strings.Split(value, ",") {
			name, arg, _ := strings.Cut(strings.TrimSpace(part), "=")
			arg = strings.Trim(strings.TrimSpace(arg), `"`)
			switch strings.ToLower(strings.TrimSpace(name)) {
			case "max-age":
				cc.maxAge, cc.hasMaxAge = npmSeconds(arg), true
			case "s-maxage":
				cc.sharedMaxAge, cc.hasSharedMaxAge = npmSeconds(arg), true
			case "private":
				cc.private = true
			case "no-store":
				cc.noStore = true
			case "no-cache":
				cc.noCache = true
			case "must-revalidate", "proxy-revalidate":
				cc.mustRevalidate = true
			}
		}
	}

	return cc
}

func npmSeconds(value string) time.Duration {
	seconds, err := strconv.ParseInt(value, 10, 64)
	if err != nil || seconds < 0 {
		return 0
	}

	return time.Duration(min(seconds, int64(365*24*3600))) * time.Second
}

// npmShareable reports whether an answer's headers let the store keep it for
// every job: nothing in Cache-Control that confines it.
//
// A COOKIE DOES NOT CONFINE IT. The registry's CDN sets a bot-management cookie
// on every answer, documents and tarballs alike, while serving them from its
// own shared cache (measured 2026-10-22); the cookie answers the node's own
// anonymous request, and the node never stores, relays or sends it back.
func npmShareable(header http.Header) bool {
	cc := parseNpmCacheControl(header.Values("Cache-Control"))

	return !cc.private && !cc.noStore && !cc.noCache
}

// npmFreshness fills an entry's clock from the registry's answer, as RFC 9111
// section 4.2.3 computes an initial age.
func npmFreshness(entry *npmEntry, header http.Header, requested, received time.Time) {
	cc := parseNpmCacheControl(header.Values("Cache-Control"))
	switch {
	case cc.hasSharedMaxAge:
		entry.Lifetime, entry.SharedMaxAge = cc.sharedMaxAge, true
	case cc.hasMaxAge:
		entry.Lifetime, entry.SharedMaxAge = cc.maxAge, false
	default:
		entry.Lifetime, entry.SharedMaxAge = 0, false
	}
	entry.MustRevalidate = cc.mustRevalidate
	entry.CacheControl = strings.Join(header.Values("Cache-Control"), ", ")

	var apparent time.Duration
	if date, err := http.ParseTime(header.Get("Date")); err == nil {
		apparent = max(0, received.Sub(date))
	}
	ageValue := npmSeconds(header.Get("Age"))
	corrected := ageValue + max(0, received.Sub(requested))
	entry.InitialAge = max(apparent, corrected)
	entry.Received = received
}

// npmStore is the node's store of registry answers.
//
// A STORED ENTRY IS TWO FILES, its body and its metadata, installed in that
// order, each durably (durablefile's ordering), and the metadata names the body:
// after a crash a body nothing names is deleted at the next open, and metadata
// is never left naming a body that is not whole. A body is never rewritten: a
// new answer gets a new body file.
//
// A BODY IS DELETED ONLY AFTER NO ENTRY IN THE INDEX NAMES IT, and an entry's
// body is opened under the index's lock, so a reader that found an entry always
// opens the body that entry names. Every publication and removal on disk holds
// publish, so two of them never interleave their files.
type npmStore struct {
	root      string
	limit     int64
	fillLimit int64
	disk      durablefile.Installer
	// remove deletes a body file: os.Remove, a seam so a test can make the disk
	// refuse a deletion, which nothing else provokes.
	remove func(string) error

	publish sync.Mutex

	mu      sync.Mutex
	entries map[string]*npmEntry
	total   int64
	filling int64
	// pins counts the open readers of each body. A body removed while pinned is
	// retired rather than deleted: its bytes stay on disk until its last reader
	// closes, which deletes it. A body a disk error kept is debt instead: it may
	// still be named by metadata on disk, so nothing in this process deletes it,
	// and the next open keeps the pair or removes what nothing names. Both are
	// counted in retiredBytes, against the fill budget, until they are gone.
	pins         map[string]int
	retired      map[string]int64
	debt         map[string]int64
	retiredBytes int64
}

// npmBody is an entry's body, open for one reader. Closing it unpins it.
type npmBody struct {
	*os.File
	store *npmStore
	name  string
	once  sync.Once
}

func (b *npmBody) Close() error {
	err := b.File.Close()
	b.once.Do(func() { b.store.unpin(b.name) })

	return err
}

// unpin ends one reader of a body, deleting it if it was retired and this was
// its last.
func (s *npmStore) unpin(name string) {
	s.mu.Lock()
	s.pins[name]--
	if s.pins[name] > 0 {
		s.mu.Unlock()

		return
	}
	delete(s.pins, name)
	_, retired := s.retired[name]
	s.mu.Unlock()
	if retired {
		s.deleteRetired(name)
	}
}

// retire deletes a body no index entry names any more, or, while a reader
// still holds it, leaves it to that reader's close. Either way its bytes stay
// counted until the file is gone. A body already kept as debt stays debt.
func (s *npmStore) retire(name string, size int64) {
	s.mu.Lock()
	if _, owed := s.debt[name]; owed {
		s.mu.Unlock()

		return
	}
	if _, retired := s.retired[name]; !retired {
		s.retired[name] = size
		s.retiredBytes += size
	}
	pinned := s.pins[name] > 0
	s.mu.Unlock()
	if !pinned {
		s.deleteRetired(name)
	}
}

// keepAsDebt counts a body a disk error left on disk, which metadata may still
// name, so a disk that keeps refusing stops the store keeping rather than
// letting such bodies grow without bound. Nothing in this process deletes it.
func (s *npmStore) keepAsDebt(name string, size int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, owed := s.debt[name]; owed {
		return
	}
	if retired, ok := s.retired[name]; ok {
		delete(s.retired, name)
		s.debt[name] = retired

		return
	}
	s.debt[name] = size
	s.retiredBytes += size
}

// deleteRetired deletes a retired body no reader holds and stops counting it,
// only once it is gone. One the disk will not delete becomes debt.
func (s *npmStore) deleteRetired(name string) {
	s.mu.Lock()
	size, retired := s.retired[name]
	pinned := s.pins[name] > 0
	s.mu.Unlock()
	if !retired || pinned {
		return
	}
	if err := s.remove(filepath.Join(s.bodiesDir(), name)); err != nil && !errors.Is(err, os.ErrNotExist) {
		s.keepAsDebt(name, size)

		return
	}
	s.mu.Lock()
	if size, retired := s.retired[name]; retired && s.pins[name] == 0 {
		delete(s.retired, name)
		s.retiredBytes -= size
	}
	s.mu.Unlock()
}

func entryName(key string) string {
	sum := sha256.Sum256([]byte(key))

	return hex.EncodeToString(sum[:])
}

func (s *npmStore) entriesDir() string { return filepath.Join(s.root, "entries") }
func (s *npmStore) bodiesDir() string  { return filepath.Join(s.root, "bodies") }
func (s *npmStore) tempDir() string    { return filepath.Join(s.root, "tmp") }

// openNpmStore loads the store at root, removing what no entry names.
func openNpmStore(root string, limit int64) (*npmStore, error) {
	s := &npmStore{root: root, limit: limit, fillLimit: npmFillLimit, entries: make(map[string]*npmEntry),
		pins: make(map[string]int), retired: make(map[string]int64), debt: make(map[string]int64), remove: os.Remove}
	for _, dir := range []string{s.entriesDir(), s.bodiesDir(), s.tempDir()} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("node: create the npm store: %w", err)
		}
	}
	// A TEMPORARY FILE OUTLIVES ONLY A PROCESS THAT DIED WRITING IT.
	if leftovers, err := os.ReadDir(s.tempDir()); err == nil {
		for _, leftover := range leftovers {
			_ = os.Remove(filepath.Join(s.tempDir(), leftover.Name()))
		}
	}
	named := make(map[string]bool)
	files, err := os.ReadDir(s.entriesDir())
	if err != nil {
		return nil, fmt.Errorf("node: read the npm store: %w", err)
	}
	for _, file := range files {
		path := filepath.Join(s.entriesDir(), file.Name())
		entry, ok := s.loadEntry(path)
		if !ok {
			_ = os.Remove(path)

			continue
		}
		entry.lastServed = entry.Received
		s.entries[entry.Key] = entry
		s.total += entry.Size
		named[entry.Body] = true
	}
	if bodies, err := os.ReadDir(s.bodiesDir()); err == nil {
		for _, body := range bodies {
			if !named[body.Name()] {
				_ = os.Remove(filepath.Join(s.bodiesDir(), body.Name()))
			}
		}
	}
	s.evict()

	return s, nil
}

// loadEntry reads one metadata file, refusing one whose body is not the size it
// says or whose name is not its key's.
func (s *npmStore) loadEntry(path string) (*npmEntry, bool) {
	file, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, npmEntryLimit+1))
	if err != nil || len(raw) > npmEntryLimit {
		return nil, false
	}
	var entry npmEntry
	if err := json.Unmarshal(raw, &entry); err != nil {
		return nil, false
	}
	if entry.Key == "" || filepath.Base(path) != entryName(entry.Key)+".json" ||
		entry.Body == "" || filepath.Base(entry.Body) != entry.Body {
		return nil, false
	}
	info, err := os.Stat(filepath.Join(s.bodiesDir(), entry.Body))
	if err != nil || !info.Mode().IsRegular() || info.Size() != entry.Size {
		return nil, false
	}

	return &entry, true
}

// held reports whether the store has an entry under key.
func (s *npmStore) held(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.entries[key]

	return ok
}

// acquire returns the entry under key with its body open and pinned, the two
// read together so that no replacement or eviction between them can hand the
// caller another generation's body or none. The caller closes the body.
func (s *npmStore) acquire(key string, now time.Time) (npmEntry, *npmBody, bool) {
	s.mu.Lock()
	entry, ok := s.entries[key]
	if !ok {
		s.mu.Unlock()

		return npmEntry{}, nil, false
	}
	snapshot := *entry
	file, err := os.Open(filepath.Join(s.bodiesDir(), entry.Body))
	if err == nil {
		if info, statErr := file.Stat(); statErr != nil || info.Size() != entry.Size {
			_ = file.Close()
			err = errors.New("node: an npm store entry's body is not the size it was stored at")
		}
	}
	if err == nil {
		entry.lastServed = now
		s.pins[entry.Body]++
	}
	s.mu.Unlock()
	if err != nil {
		s.removeIf(key, snapshot.Body)

		return npmEntry{}, nil, false
	}

	return snapshot, &npmBody{File: file, store: s, name: snapshot.Body}, true
}

// reserve claims n bytes of the fill budget, reporting whether it could. The
// bytes of retired bodies still held open count against it, since they are on
// disk beyond the store's bound until their readers close.
func (s *npmStore) reserve(n int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.filling+s.retiredBytes+n > s.fillLimit {
		return false
	}
	s.filling += n

	return true
}

// release returns n bytes of the fill budget.
func (s *npmStore) release(n int64) {
	s.mu.Lock()
	s.filling -= n
	s.mu.Unlock()
}

// temp is a new file a fill writes its body into before commit names it.
func (s *npmStore) temp() (*os.File, error) {
	return os.CreateTemp(s.tempDir(), "fill-*")
}

// commit stores entry with the body written to the temporary file at temp,
// replacing what the key held. The temporary file is consumed whether or not
// commit succeeds.
func (s *npmStore) commit(entry npmEntry, temp string) error {
	if _, err := s.publishEntry(entry, temp, false); err != nil {
		return err
	}
	// AFTER THE PUBLICATION'S LOCK IS RELEASED: eviction removes, and a removal
	// takes the same lock.
	s.evict()

	return nil
}

// commitAndAcquire commits entry and returns its body already pinned, so an
// eviction the commit triggers can retire it but never delete it from under
// the reader that is about to serve it.
func (s *npmStore) commitAndAcquire(entry npmEntry, temp string) (npmEntry, *npmBody, error) {
	stored, err := s.publishEntry(entry, temp, true)
	if err != nil {
		return npmEntry{}, nil, err
	}
	file, err := os.Open(filepath.Join(s.bodiesDir(), stored.Body))
	if err != nil {
		s.unpin(stored.Body)
		s.evict()

		return npmEntry{}, nil, err
	}
	s.evict()

	return stored, &npmBody{File: file, store: s, name: stored.Body}, nil
}

func (s *npmStore) publishEntry(entry npmEntry, temp string, pin bool) (npmEntry, error) {
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		_ = os.Remove(temp)

		return npmEntry{}, err
	}
	entry.Body = entryName(entry.Key) + "-" + hex.EncodeToString(nonce[:])
	body := filepath.Join(s.bodiesDir(), entry.Body)

	s.publish.Lock()
	defer s.publish.Unlock()
	// THE BODY IS WHOLE ON DISK, UNDER ITS NAME, BEFORE METADATA NAMES IT.
	if err := s.installBody(temp, body); err != nil {
		_ = os.Remove(temp)
		// A RENAME THAT HAPPENED BEFORE A SYNC FAILED left the body under its name;
		// one the disk then refuses to delete is debt, never silently dropped.
		if removeErr := s.remove(body); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			s.keepAsDebt(entry.Body, entry.Size)
		}

		return npmEntry{}, err
	}
	if err := s.installEntry(entry); err != nil {
		// THE BODY STAYS: a metadata install whose directory sync failed may still
		// have put the metadata in place, and that metadata names this body. The
		// next open keeps the pair or deletes a body nothing names; until then the
		// body is owed, charged to the fill budget.
		s.keepAsDebt(entry.Body, entry.Size)

		return npmEntry{}, err
	}

	s.mu.Lock()
	previous := s.entries[entry.Key]
	stored := entry
	stored.lastServed = entry.Received
	s.entries[entry.Key] = &stored
	s.total += entry.Size
	// PINNED IN THE SAME CRITICAL SECTION THAT PUBLISHES IT, before any eviction
	// can choose it.
	if pin {
		s.pins[entry.Body]++
	}
	if previous != nil {
		s.total -= previous.Size
	}
	s.mu.Unlock()
	if previous != nil && previous.Body != entry.Body {
		s.retire(previous.Body, previous.Size)
	}

	return entry, nil
}

func (s *npmStore) installBody(temp, body string) error {
	file, err := os.OpenFile(temp, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	if err := s.disk.SyncFileHandle(file); err != nil {
		_ = file.Close()

		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(temp, body); err != nil {
		return err
	}

	return s.disk.SyncDirectory(s.bodiesDir())
}

func (s *npmStore) installEntry(entry npmEntry) error {
	raw, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	_, err = s.disk.Install(s.entriesDir(), entryName(entry.Key)+".json", 0o600, func(w io.Writer) error {
		_, err := w.Write(raw)

		return err
	})

	return err
}

// refresh rewrites an entry's metadata after a revalidation, keeping its body.
func (s *npmStore) refresh(entry npmEntry) error {
	s.publish.Lock()
	defer s.publish.Unlock()
	s.mu.Lock()
	current, ok := s.entries[entry.Key]
	if !ok || current.Body != entry.Body {
		s.mu.Unlock()

		return errors.New("node: an npm store entry changed while it was revalidated")
	}
	s.mu.Unlock()
	if err := s.installEntry(entry); err != nil {
		return err
	}
	s.mu.Lock()
	if current, ok := s.entries[entry.Key]; ok && current.Body == entry.Body {
		served := current.lastServed
		*current = entry
		current.lastServed = served
	}
	s.mu.Unlock()

	return nil
}

// removeIf deletes the entry under key when it still names body, or whatever
// it names when body is empty. A generation that replaced the one the caller
// meant is left alone.
func (s *npmStore) removeIf(key, body string) {
	s.publish.Lock()
	defer s.publish.Unlock()
	s.mu.Lock()
	entry, ok := s.entries[key]
	if !ok || body != "" && entry.Body != body {
		s.mu.Unlock()

		return
	}
	delete(s.entries, key)
	s.total -= entry.Size
	s.mu.Unlock()
	// THE METADATA GOES FIRST, DURABLY, so a crash never leaves it naming a body
	// that was deleted. A REMOVAL NOT YET DURABLE KEEPS ITS BODY: metadata a crash
	// brings back still names a whole file, and the next open deletes a body
	// nothing names.
	if err := os.Remove(filepath.Join(s.entriesDir(), entryName(key)+".json")); err != nil &&
		!errors.Is(err, os.ErrNotExist) {
		s.keepAsDebt(entry.Body, entry.Size)

		return
	}
	if err := s.disk.SyncDirectory(s.entriesDir()); err != nil {
		s.keepAsDebt(entry.Body, entry.Size)

		return
	}
	s.retire(entry.Body, entry.Size)
}

// evict removes the least recently served entries until the store is back
// under its bound, each only if it is still the generation chosen.
func (s *npmStore) evict() {
	s.mu.Lock()
	if s.total <= s.limit {
		s.mu.Unlock()

		return
	}
	order := make([]npmEntry, 0, len(s.entries))
	for _, entry := range s.entries {
		order = append(order, *entry)
	}
	s.mu.Unlock()
	slices.SortFunc(order, func(a, b npmEntry) int { return a.lastServed.Compare(b.lastServed) })
	for i := range order {
		s.mu.Lock()
		over := s.total > s.limit
		s.mu.Unlock()
		if !over {
			return
		}
		s.removeIf(order[i].Key, order[i].Body)
	}
}
