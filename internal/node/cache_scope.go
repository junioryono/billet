package node

import (
	"errors"
	"fmt"
	"strings"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/server"
)

// scopedNamespaceSuffix separates default-branch namespaces from every key a
// guest can name. A guest's key becomes `<namespace>/<key>`, always with a
// slash after the namespace, so no key any node build accepts can spell a
// key under `<namespace>.scoped/`, and a trusted session can never clone what
// an untrusted pool published there.
const scopedNamespaceSuffix = ".scoped"

// sessionPolicy is the publication policy a session was scoped with. A session
// with no cache configuration (an older control plane, or a record from before
// tiers[].cache) is trusted-only, which is the legacy behaviour.
func sessionPolicy(session *cacheSession) config.CachePublish {
	if session.cache == nil {
		return config.CachePublishTrustedOnly
	}

	return session.cache.Publish.Effective()
}

// sessionSetting is one cache's setting for a session, the legacy default when
// the session carries no configuration.
func sessionSetting(session *cacheSession, kind config.CacheKind) config.CacheSetting {
	if session.cache != nil {
		return session.cache.Setting(kind)
	}

	legacy := config.Tier{Intercept: session.intercept}.EffectiveCache()

	return legacy.Setting(kind)
}

// cacheKeyFor is the store key a session's cache of one kind uses. EVERY key a
// session's cache reads or writes is built here.
//
// A trusted-only or off session keeps exactly the keys it always had, so no
// published generation is orphaned by an upgrade: the Docker store at
// `<ns>/docker-images/<arch>`, a sticky disk at `<ns>/<key>`. A default-branch
// session's keys live under a namespace of their own, scoped by the pool's trust
// class, the repository and the architecture.
func (s *CacheService) cacheKeyFor(session *cacheSession, kind config.CacheKind, arch,
	rest string,
) string {
	if sessionPolicy(session) != config.CachePublishDefaultBranch {
		if kind == config.CacheDocker {
			return s.qualifiedKey(dockerStoreKey + arch)
		}

		return s.qualifiedKey(rest)
	}

	if arch == "" {
		arch = "any"
	}
	key := s.namespace + scopedNamespaceSuffix + "/" + session.trust.String() + "/" +
		strings.ToLower(session.cache.Owner) + "/" + strings.ToLower(session.cache.Repository) +
		"/" + arch + "/" + string(kind)
	if rest != "" {
		key += "/" + rest
	}

	return key
}

// defaultBranchScope reports whether a cache configuration scopes its caches by
// the job's proven ref rather than a static workflow.
func defaultBranchScope(spec *config.CacheSpec) bool {
	return spec != nil && spec.Publish.Effective() == config.CachePublishDefaultBranch
}

// validateSessionCache refuses a cache configuration a session cannot be
// scoped by: a default-branch namespace needs the repository it belongs to.
func validateSessionCache(spec *config.CacheSpec) error {
	if spec == nil {
		return nil
	}
	if !spec.Publish.Valid() {
		return fmt.Errorf("node: unknown cache publication policy %q", spec.Publish)
	}
	if spec.Publish.Effective() != config.CachePublishDefaultBranch {
		return nil
	}

	return errors.Join(config.CheckCacheScopeSegment(spec.Owner),
		config.CheckCacheScopeSegment(spec.Repository))
}

// authorisesPublication reports whether a completion's authority lets this
// session publish what the default branch reads.
//
// EVERYTHING IS CHECKED AGAIN HERE, on the node that holds the volumes: the
// authority names the exact lease the session belongs to and the repository
// its namespace is scoped by, and it must be proven. A grant for another lease
// on the same request, or for another repository in the same organization
// pool, publishes nothing.
func authorisesPublication(session *cacheSession, authority server.CacheAuthority) bool {
	return sessionPolicy(session) == config.CachePublishDefaultBranch &&
		session.leaseID != "" && authority.LeaseID == session.leaseID &&
		authority.Proven && authority.PublishDefault &&
		strings.EqualFold(authority.Owner, session.cache.Owner) &&
		strings.EqualFold(authority.Repository, session.cache.Repository)
}
