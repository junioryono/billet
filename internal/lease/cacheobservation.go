package lease

import (
	"errors"
	"fmt"
)

// ImageCache is what the guest's Docker image-store clone did, as the node saw
// it.
//
// A CLOSED VOCABULARY AND AN OBSERVATION, like Disruption: it says what the
// cache did for one job and decides nothing. Recorded from what the node saw
// at the moment the guest asked, never from what the tier intended, because a
// field that says "warm" without saying who observed it is the could-not-tell
// collapse this repository keeps removing. The empty string means nothing was
// observed.
type ImageCache string

const (
	// ImageCacheWarm means the guest's image store was cloned from a published
	// generation, which CacheObservation.CacheGeneration names.
	ImageCacheWarm ImageCache = "warm"
	// ImageCacheCold means no generation existed for the store and a fresh volume
	// was created for the guest.
	ImageCacheCold ImageCache = "cold"
	// ImageCacheUnavailable means the site store failed and the job continued
	// with no image store at all.
	ImageCacheUnavailable ImageCache = "unavailable"
	// ImageCacheUnused means the cache session closed without the guest ever
	// asking for an image store.
	ImageCacheUnused ImageCache = "unused"
)

// Valid reports whether this is a token billet may write.
func (c ImageCache) Valid() bool {
	switch c {
	case ImageCacheWarm, ImageCacheCold, ImageCacheUnavailable, ImageCacheUnused:
		return true
	}

	return false
}

// ActionsCache is what the Actions cache interception did for the FIRST
// CacheService call of a job whose disposition was recorded, as the node saw
// it.
//
// RECORDED WHEN THE DISPOSITION IS FINAL, not when it is intended: a call the
// site store failed to answer is retried through GitHub, and what the guest
// got was a splice, whatever billet set out to do. A job's calls run
// concurrently, so the one kept is the first completed outcome to take the
// session's lock and be written, which need not be the call dispatched first
// or even the one that finished first; any of them is an honest account of a
// call this job made.
type ActionsCache string

const (
	// ActionsCacheServed means the request was answered from the site store.
	ActionsCacheServed ActionsCache = "served"
	// ActionsCacheSpliced means interception was on and billet handed the
	// request to GitHub for a reason other than the kill switch: the policy
	// could not be read, the client was not one billet serves locally, or the
	// local handler failed. It says what billet did with the call, not what
	// GitHub answered; an upstream that could not be reached is GitHub's
	// unavailability, and the path fails open on it by design.
	ActionsCacheSpliced ActionsCache = "spliced"
	// ActionsCacheDisabled means the central kill switch refused the request.
	ActionsCacheDisabled ActionsCache = "disabled"
	// ActionsCacheUnavailable means billet answered the request with a failure
	// of its own and handed nothing to GitHub: a call bound to a reservation
	// only billet holds that failed locally, or a request billet refused before
	// reading it.
	ActionsCacheUnavailable ActionsCache = "unavailable"
	// ActionsCacheOff means the session had no interception at all: the tier does
	// not intercept, or the work was untrusted.
	ActionsCacheOff ActionsCache = "off"
	// ActionsCacheUnused means interception was on and the session closed
	// without a CacheService request ever arriving.
	ActionsCacheUnused ActionsCache = "unused"
)

// Valid reports whether this is a token billet may write.
func (c ActionsCache) Valid() bool {
	switch c {
	case ActionsCacheServed, ActionsCacheSpliced, ActionsCacheDisabled,
		ActionsCacheUnavailable, ActionsCacheOff, ActionsCacheUnused:
		return true
	}

	return false
}

// BuildCache is what one build cache (sticky disks, the Git mirror, the Bazel
// and Go content-addressed caches) did for a job, as the node saw it over the
// job's whole session and reported when the session ended.
//
// AN OBSERVATION, like ImageCache: it decides nothing. The empty string means
// nothing was observed, which is also what a session a restarted node recovered
// says about a cache it did not see the whole of.
type BuildCache string

const (
	// BuildCacheWarm means the cache already held something the job used.
	BuildCacheWarm BuildCache = "warm"
	// BuildCacheCold means the job used the cache and it held nothing the job
	// used.
	BuildCacheCold BuildCache = "cold"
	// BuildCacheDisabled means the kill switch refused the cache.
	BuildCacheDisabled BuildCache = "disabled"
	// BuildCacheUnavailable means the store failed and the job went on without
	// the cache.
	BuildCacheUnavailable BuildCache = "unavailable"
	// BuildCacheUnused means the tier enabled the cache and the session ended
	// without the guest asking for it.
	BuildCacheUnused BuildCache = "unused"
)

// Valid reports whether this is a token billet may write.
func (c BuildCache) Valid() bool {
	switch c {
	case BuildCacheWarm, BuildCacheCold, BuildCacheDisabled, BuildCacheUnavailable, BuildCacheUnused:
		return true
	}

	return false
}

// BuildCaches is what each build cache did for one job.
type BuildCaches struct {
	Sticky BuildCache `json:"sticky_cache,omitempty"`
	Git    BuildCache `json:"git_cache,omitempty"`
	Bazel  BuildCache `json:"bazel_cache,omitempty"`
	Go     BuildCache `json:"go_cache,omitempty"`
}

// each names every observation, for the loops that treat them alike.
func (b BuildCaches) each() map[string]BuildCache {
	return map[string]BuildCache{"sticky": b.Sticky, "git": b.Git, "bazel": b.Bazel, "go": b.Go}
}

// CacheObservation is what a node saw the cache do for one job. Either half
// may be empty, meaning that half has not been observed yet; a generation
// travels only with a warm image store.
type CacheObservation struct {
	ImageCache      ImageCache   `json:"image_cache,omitempty"`
	CacheGeneration string       `json:"cache_generation,omitempty"`
	ActionsCache    ActionsCache `json:"actions_cache,omitempty"`
	BuildCaches
}

// Validate refuses an observation billet must not write: an unknown token, a
// generation with no warm store to attribute it to, or nothing at all.
func (o CacheObservation) Validate() error {
	if o.ImageCache == "" && o.ActionsCache == "" && o.BuildCaches == (BuildCaches{}) {
		return errors.New("alloc: a cache observation must observe something")
	}
	for name, outcome := range o.each() {
		if outcome != "" && !outcome.Valid() {
			return fmt.Errorf("alloc: %q is not a %s cache outcome billet records", outcome, name)
		}
	}
	if o.ImageCache != "" && !o.ImageCache.Valid() {
		return fmt.Errorf("alloc: %q is not an image-cache outcome billet records", o.ImageCache)
	}
	if o.ActionsCache != "" && !o.ActionsCache.Valid() {
		return fmt.Errorf("alloc: %q is not an Actions cache outcome billet records", o.ActionsCache)
	}
	if o.CacheGeneration != "" && o.ImageCache != ImageCacheWarm {
		return fmt.Errorf("alloc: a cache generation is recorded only beside a warm image store, not %q",
			o.ImageCache)
	}
	if o.ImageCache == ImageCacheWarm && o.CacheGeneration == "" {
		return errors.New("alloc: a warm image store must name the generation it was cloned from")
	}

	return nil
}
