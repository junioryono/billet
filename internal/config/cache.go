package config

import (
	"fmt"
	"strings"
)

// CachePublish says whose writes a tier's caches publish.
type CachePublish string

const (
	// CachePublishTrustedOnly publishes what a trusted pool writes and discards
	// everything else. It is the zero value, and it is exactly the behaviour
	// before tiers[].cache existed.
	CachePublishTrustedOnly CachePublish = "trusted-only"
	// CachePublishDefaultBranch publishes what a job GitHub proves ran on its
	// repository's default branch writes, under an event GitHub itself lets
	// write there, into a namespace scoped by the pool's trust and repository.
	// Every other job reads and discards.
	CachePublishDefaultBranch CachePublish = "default-branch"
	// CachePublishOff publishes nothing.
	CachePublishOff CachePublish = "off"
)

// Effective is the policy an omitted value means.
func (p CachePublish) Effective() CachePublish {
	if p == "" {
		return CachePublishTrustedOnly
	}

	return p
}

// Valid reports whether p is one billet understands.
func (p CachePublish) Valid() bool {
	switch p.Effective() {
	case CachePublishTrustedOnly, CachePublishDefaultBranch, CachePublishOff:
		return true
	default:
		return false
	}
}

// CacheVolumeLimit is the largest cache volume a guest may be given, and the
// ceiling on every max_size.
const CacheVolumeLimit = 100 * GiB

// ActionsArchiveLimit is the largest Actions cache archive billet serves,
// GitHub's own limit.
const ActionsArchiveLimit = 10 * GiB

// Default sizes for the caches a tier enables without naming one.
const (
	DefaultDockerCacheSize = 100 * GiB
	DefaultStickyDiskSize  = 100 * GiB
	DefaultGitCacheSize    = 50 * GiB
	DefaultBazelCacheSize  = 50 * GiB
	DefaultGoCacheSize     = 20 * GiB
)

// TierCache is one tier's cache configuration, `tiers[].cache`.
//
// Every field is optional and a tier that says nothing gets exactly what it
// had before this block existed: the Docker image store and sticky disks on,
// publication from trusted pools only, everything else off.
type TierCache struct {
	Publish     CachePublish  `yaml:"publish,omitempty"`
	Docker      *CacheToggle  `yaml:"docker,omitempty"`
	StickyDisks *CacheToggle  `yaml:"sticky_disks,omitempty"`
	Actions     *ActionsCache `yaml:"actions,omitempty"`
	Git         *CacheToggle  `yaml:"git,omitempty"`
	Bazel       *CacheToggle  `yaml:"bazel,omitempty"`
	Go          *GoCache      `yaml:"go,omitempty"`
}

// CacheToggle turns one cache on or off and bounds its size.
type CacheToggle struct {
	// Enabled is a pointer so an omitted value takes the cache's own default.
	Enabled *bool    `yaml:"enabled,omitempty"`
	MaxSize ByteSize `yaml:"max_size,omitempty"`
}

// ActionsCache configures the transparent Actions cache.
type ActionsCache struct {
	Enabled    *bool    `yaml:"enabled,omitempty"`
	MaxArchive ByteSize `yaml:"max_archive,omitempty"`
}

// GoCache configures the Go build cache and, opt-in, its test results.
type GoCache struct {
	Enabled *bool    `yaml:"enabled,omitempty"`
	MaxSize ByteSize `yaml:"max_size,omitempty"`
	// TestResults publishes `go test` results. Off, jobs run with
	// GOFLAGS=-count=1, because a cached pass hides a test that depends on
	// something outside its inputs.
	TestResults bool `yaml:"test_results,omitempty"`
}

// CacheKind names one cache. The strings are the kill switch's vocabulary.
type CacheKind string

const (
	CacheDocker  CacheKind = "docker"
	CacheSticky  CacheKind = "sticky"
	CacheActions CacheKind = "actions"
	CacheGit     CacheKind = "git"
	CacheBazel   CacheKind = "bazel"
	CacheGo      CacheKind = "go"
)

// CacheKinds are every cache billet serves, in the order reports list them.
var CacheKinds = []CacheKind{CacheDocker, CacheSticky, CacheActions, CacheGit, CacheBazel, CacheGo}

// Valid reports whether k names a cache.
func (k CacheKind) Valid() bool {
	for _, known := range CacheKinds {
		if k == known {
			return true
		}
	}

	return false
}

// CacheSetting is one cache's effective setting.
type CacheSetting struct {
	Enabled bool     `json:"enabled"`
	MaxSize ByteSize `json:"max_size,omitempty"`
}

// CacheSpec is a tier's effective cache configuration, every default applied.
// It is what travels to a node, so a node needs no copy of the rules.
type CacheSpec struct {
	Publish CachePublish `json:"publish"`
	// Owner and Repository are the canonical scope of a default-branch
	// namespace: the tier's cache_scope, or its repository target. Empty
	// under trusted-only, whose keys are scoped by the deployment and site
	// alone, as they always were.
	Owner         string       `json:"owner,omitempty"`
	Repository    string       `json:"repository,omitempty"`
	Docker        CacheSetting `json:"docker"`
	StickyDisks   CacheSetting `json:"sticky_disks"`
	Actions       CacheSetting `json:"actions"`
	Git           CacheSetting `json:"git"`
	Bazel         CacheSetting `json:"bazel"`
	Go            CacheSetting `json:"go"`
	GoTestResults bool         `json:"go_test_results,omitempty"`
}

// Setting is the effective setting of one kind.
func (s CacheSpec) Setting(kind CacheKind) CacheSetting {
	switch kind {
	case CacheDocker:
		return s.Docker
	case CacheSticky:
		return s.StickyDisks
	case CacheActions:
		return s.Actions
	case CacheGit:
		return s.Git
	case CacheBazel:
		return s.Bazel
	case CacheGo:
		return s.Go
	default:
		return CacheSetting{}
	}
}

// IsLegacy reports whether s is what a tier had before tiers[].cache existed:
// trusted-only publication, the Docker store and sticky disks at their default
// sizes, nothing else. Anything else needs a node that understands it.
func (s CacheSpec) IsLegacy() bool {
	return s.Publish == CachePublishTrustedOnly && s.Owner == "" && s.Repository == "" &&
		s.Docker == (CacheSetting{Enabled: true, MaxSize: DefaultDockerCacheSize}) &&
		s.StickyDisks == (CacheSetting{Enabled: true, MaxSize: DefaultStickyDiskSize}) &&
		!s.Git.Enabled && !s.Bazel.Enabled && !s.Go.Enabled && !s.GoTestResults &&
		s.Actions.MaxSize == ActionsArchiveLimit
}

// EffectiveCache is the tier's cache configuration with every default applied.
//
// `intercept: true` is the deprecated spelling of `cache.actions.enabled`, and
// reads as it.
func (t Tier) EffectiveCache() CacheSpec {
	c := t.Cache
	if c == nil {
		c = &TierCache{}
	}

	spec := CacheSpec{
		Publish:     c.Publish.Effective(),
		Docker:      toggle(c.Docker, true, DefaultDockerCacheSize),
		StickyDisks: toggle(c.StickyDisks, true, DefaultStickyDiskSize),
		Git:         toggle(c.Git, false, DefaultGitCacheSize),
		Bazel:       toggle(c.Bazel, false, DefaultBazelCacheSize),
		Actions:     CacheSetting{Enabled: t.Intercept, MaxSize: ActionsArchiveLimit},
	}
	if c.Actions != nil {
		if c.Actions.Enabled != nil {
			spec.Actions.Enabled = *c.Actions.Enabled
		}
		if c.Actions.MaxArchive > 0 {
			spec.Actions.MaxSize = c.Actions.MaxArchive
		}
	}
	if c.Go != nil {
		spec.Go = toggle(&CacheToggle{Enabled: c.Go.Enabled, MaxSize: c.Go.MaxSize}, false,
			DefaultGoCacheSize)
		spec.GoTestResults = spec.Go.Enabled && c.Go.TestResults
	} else {
		spec.Go = CacheSetting{MaxSize: DefaultGoCacheSize}
	}
	if spec.Publish == CachePublishDefaultBranch && t.CacheScope != nil {
		spec.Owner, spec.Repository = t.CacheScope.Owner, t.CacheScope.Repository
	}

	return spec
}

func toggle(t *CacheToggle, enabled bool, size ByteSize) CacheSetting {
	setting := CacheSetting{Enabled: enabled, MaxSize: size}
	if t == nil {
		return setting
	}
	if t.Enabled != nil {
		setting.Enabled = *t.Enabled
	}
	if t.MaxSize > 0 {
		setting.MaxSize = t.MaxSize
	}

	return setting
}

// NeedsRunEvidence reports whether any tier publishes from a default branch.
func (c *Config) NeedsRunEvidence() bool {
	for i := range c.Tiers {
		if c.Tiers[i].EffectiveCache().Publish == CachePublishDefaultBranch {
			return true
		}
	}

	return false
}

// TargetNeedsRunEvidence reports whether a tier of the named target publishes
// from a default branch, which is what asks that target's GitHub App for
// `actions: read`. PER TARGET, because each target has its own App, and one
// target's cache policy must not make another's minimal App look deficient.
func (c *Config) TargetNeedsRunEvidence(name string) bool {
	for i := range c.Tiers {
		target, ok := c.TierTarget(&c.Tiers[i])
		if ok && target.Name == name &&
			c.Tiers[i].EffectiveCache().Publish == CachePublishDefaultBranch {
			return true
		}
	}

	return false
}

// needsGuestCacheServices reports whether a tier enables a cache that is
// served by the node for a guest the node controls: the Actions cache, the Git
// proxy, the Bazel and Go caches.
func (s CacheSpec) needsGuestCacheServices() bool {
	return s.Actions.Enabled || s.Git.Enabled || s.Bazel.Enabled || s.Go.Enabled
}

// materializeCacheScope writes a repository target's identity as the cache
// scope of a default-branch tier that names none, so the scope travels with
// the tier to every node that launches it.
func (c *Config) materializeCacheScope() {
	for i := range c.Tiers {
		t := &c.Tiers[i]
		if t.Cache == nil || t.Cache.Publish.Effective() != CachePublishDefaultBranch ||
			t.CacheScope != nil {
			continue
		}
		target, ok := c.TierTarget(t)
		if !ok || !target.IsRepository() {
			continue
		}
		t.CacheScope = &CacheScope{Owner: target.Owner(), Repository: target.RepositoryName()}
	}
}

// cachePolicyErrors reports what a tier's cache block may not say.
func (t Tier) cachePolicyErrors(where string) []error {
	var errs []error

	c := t.Cache
	if c != nil {
		if !c.Publish.Valid() {
			errs = append(errs, fmt.Errorf("%s: cache.publish %q is not one of [%s %s %s]", where,
				c.Publish, CachePublishTrustedOnly, CachePublishDefaultBranch, CachePublishOff))
		}
		if c.Actions != nil && t.Intercept {
			errs = append(errs, fmt.Errorf("%s: intercept and cache.actions both configure the "+
				"Actions cache; intercept is the deprecated spelling, so keep cache.actions only", where))
		}
		for name, size := range map[string]ByteSize{
			"docker": sizeOf(c.Docker), "sticky_disks": sizeOf(c.StickyDisks),
			"git": sizeOf(c.Git), "bazel": sizeOf(c.Bazel),
		} {
			if size < 0 || size > CacheVolumeLimit {
				errs = append(errs, fmt.Errorf("%s: cache.%s.max_size %s is outside (0, %s]",
					where, name, size, ByteSize(CacheVolumeLimit)))
			}
		}
		if c.Go != nil && (c.Go.MaxSize < 0 || c.Go.MaxSize > CacheVolumeLimit) {
			errs = append(errs, fmt.Errorf("%s: cache.go.max_size %s is outside (0, %s]",
				where, c.Go.MaxSize, ByteSize(CacheVolumeLimit)))
		}
		if c.Go != nil && c.Go.TestResults && (c.Go.Enabled == nil || !*c.Go.Enabled) {
			errs = append(errs, fmt.Errorf("%s: cache.go.test_results needs cache.go.enabled", where))
		}
		if c.Actions != nil && (c.Actions.MaxArchive < 0 || c.Actions.MaxArchive > ActionsArchiveLimit) {
			errs = append(errs, fmt.Errorf("%s: cache.actions.max_archive %s is outside (0, %s]",
				where, c.Actions.MaxArchive, ByteSize(ActionsArchiveLimit)))
		}
	}

	spec := t.EffectiveCache()
	if spec.Publish == CachePublishDefaultBranch &&
		(t.CacheScope == nil || t.CacheScope.Owner == "" || t.CacheScope.Repository == "") {
		errs = append(errs, fmt.Errorf("%s: cache.publish: default-branch needs a static "+
			"repository, a repository target or cache_scope.owner and cache_scope.repository, "+
			"because a pooled runner's caches are attached before GitHub chooses its job", where))
	}

	return errs
}

func sizeOf(t *CacheToggle) ByteSize {
	if t == nil {
		return 0
	}

	return t.MaxSize
}

// CheckCacheScopeSegment reports why a name cannot be one component of a cache
// namespace, or nil.
func CheckCacheScopeSegment(name string) error {
	if name == "" || strings.TrimSpace(name) != name || len(name) > 100 ||
		strings.ContainsAny(name, "/\\\x00\r\n") || name == "." || name == ".." {
		return fmt.Errorf("%q must be one trimmed path component no longer than 100 bytes", name)
	}

	return nil
}
