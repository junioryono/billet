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
)

const (
	// npmStoreLimit bounds the node's npm store. The store is the node's, shared
	// by every tier on it, so the bound is the node's too.
	npmStoreLimit = 10 << 30
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
// every job: no cookie, and nothing in Cache-Control that confines it.
func npmShareable(header http.Header) bool {
	if len(header.Values("Set-Cookie")) > 0 {
		return false
	}
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
// A STORED ENTRY IS TWO FILES, its body and its metadata, and the metadata is
// written last and names the body, so a crash between them leaves a body nothing
// names, which the next open deletes. A body is never rewritten: a new answer
// gets a new body file, and the old one is removed once nothing names it.
type npmStore struct {
	root  string
	limit int64

	mu      sync.Mutex
	entries map[string]*npmEntry
	total   int64
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
	s := &npmStore{root: root, limit: limit, entries: make(map[string]*npmEntry)}
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

// lookup returns a copy of the entry stored under key.
func (s *npmStore) lookup(key string) (npmEntry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.entries[key]
	if !ok {
		return npmEntry{}, false
	}

	return *entry, true
}

// held reports whether the store has an entry under key.
func (s *npmStore) held(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.entries[key]

	return ok
}

// open opens an entry's body for reading, and marks it served.
func (s *npmStore) open(entry npmEntry, now time.Time) (*os.File, error) {
	file, err := os.Open(filepath.Join(s.bodiesDir(), entry.Body))
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil || info.Size() != entry.Size {
		_ = file.Close()

		return nil, errors.New("node: an npm store entry's body is not the size it was stored at")
	}
	s.mu.Lock()
	if current, ok := s.entries[entry.Key]; ok && current.Body == entry.Body {
		current.lastServed = now
	}
	s.mu.Unlock()

	return file, nil
}

// temp is a new file a fill writes its body into before commit names it.
func (s *npmStore) temp() (*os.File, error) {
	return os.CreateTemp(s.tempDir(), "fill-*")
}

// commit stores entry with the body written to temp, replacing what the key
// held. The temporary file is consumed whether or not commit succeeds.
func (s *npmStore) commit(entry npmEntry, temp string) error {
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		_ = os.Remove(temp)

		return err
	}
	entry.Body = entryName(entry.Key) + "-" + hex.EncodeToString(nonce[:])
	body := filepath.Join(s.bodiesDir(), entry.Body)
	if err := os.Rename(temp, body); err != nil {
		_ = os.Remove(temp)

		return err
	}
	if err := s.writeEntry(entry); err != nil {
		_ = os.Remove(body)

		return err
	}

	s.mu.Lock()
	previous := s.entries[entry.Key]
	stored := entry
	stored.lastServed = entry.Received
	s.entries[entry.Key] = &stored
	s.total += entry.Size
	if previous != nil {
		s.total -= previous.Size
	}
	s.mu.Unlock()
	if previous != nil && previous.Body != entry.Body {
		_ = os.Remove(filepath.Join(s.bodiesDir(), previous.Body))
	}
	s.evict()

	return nil
}

// refresh rewrites an entry's metadata after a revalidation, keeping its body.
func (s *npmStore) refresh(entry npmEntry) error {
	s.mu.Lock()
	current, ok := s.entries[entry.Key]
	if !ok || current.Body != entry.Body {
		s.mu.Unlock()

		return errors.New("node: an npm store entry changed while it was revalidated")
	}
	s.mu.Unlock()
	if err := s.writeEntry(entry); err != nil {
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

// remove deletes the entry under key.
func (s *npmStore) remove(key string) {
	s.mu.Lock()
	entry, ok := s.entries[key]
	if ok {
		delete(s.entries, key)
		s.total -= entry.Size
	}
	s.mu.Unlock()
	if !ok {
		return
	}
	_ = os.Remove(filepath.Join(s.entriesDir(), entryName(key)+".json"))
	_ = os.Remove(filepath.Join(s.bodiesDir(), entry.Body))
}

func (s *npmStore) writeEntry(entry npmEntry) error {
	raw, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(s.tempDir(), "entry-*")
	if err != nil {
		return err
	}
	if _, err := file.Write(raw); err != nil {
		_ = file.Close()
		_ = os.Remove(file.Name())

		return err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(file.Name())

		return err
	}
	if err := os.Rename(file.Name(), filepath.Join(s.entriesDir(), entryName(entry.Key)+".json")); err != nil {
		_ = os.Remove(file.Name())

		return err
	}

	return nil
}

// evict removes the least recently served entries until the store is back
// under its bound.
func (s *npmStore) evict() {
	s.mu.Lock()
	if s.total <= s.limit {
		s.mu.Unlock()

		return
	}
	order := make([]*npmEntry, 0, len(s.entries))
	for _, entry := range s.entries {
		order = append(order, entry)
	}
	slices.SortFunc(order, func(a, b *npmEntry) int { return a.lastServed.Compare(b.lastServed) })
	var victims []string
	total := s.total
	for _, entry := range order {
		if total <= s.limit {
			break
		}
		victims = append(victims, entry.Key)
		total -= entry.Size
	}
	s.mu.Unlock()
	for _, key := range victims {
		s.remove(key)
	}
}
