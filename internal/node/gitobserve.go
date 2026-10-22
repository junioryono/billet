package node

import (
	"errors"
	"net/http"
	"time"
)

// GitObserver counts what the Git proxy does: each request by where it was
// answered and why, each refresh of a mirror by how it ended, and each question
// to GitHub's API about which repository a name is, by its answer. Every label
// is a closed set this package names, never anything a guest or GitHub wrote.
// internal/metrics satisfies it without importing this package.
type GitObserver interface {
	GitRequest(route, reason, status string, took time.Duration)
	GitRefresh(outcome string, took time.Duration)
	GitLookup(outcome string)
}

// noGitObserver counts nothing, the observer of a node with no metrics.
type noGitObserver struct{}

func (noGitObserver) GitRequest(string, string, string, time.Duration) {}
func (noGitObserver) GitRefresh(string, time.Duration)                 {}
func (noGitObserver) GitLookup(string)                                 {}

// SetGitObserver sets what the Git proxy reports to; nil reports nowhere.
func (s *CacheService) SetGitObserver(o GitObserver) {
	if o == nil {
		o = noGitObserver{}
	}
	s.git.observer.Store(&o)
}

// observe is the proxy's observer.
func (g *gitProxy) observe() GitObserver {
	if o := g.observer.Load(); o != nil {
		return *o
	}

	return noGitObserver{}
}

// Routes a git request is answered by.
const (
	gitRouteMirror  = "mirror"
	gitRouteGitHub  = "github"
	gitRouteRefused = "refused"
)

// GitRoutes is every route a request is counted under.
func GitRoutes() []string { return []string{gitRouteMirror, gitRouteGitHub, gitRouteRefused} }

// gitReason is why a request was answered where it was: a label for the count
// and a sentence for the request's line.
type gitReason struct{ label, text string }

var (
	gitServed                = gitReason{"served", ""}
	gitBusy                  = gitReason{"busy", "the git cache is busy"}
	gitCacheOff              = gitReason{"cache_off", "the git cache is off for this tier"}
	gitOutsideOwner          = gitReason{"outside_owner", "the repository is outside the tier's owner"}
	gitNotAFetch             = gitReason{"not_a_fetch", "not a fetch"}
	gitUnreadableRequest     = gitReason{"unreadable_request", "an unreadable request"}
	gitRequestTooLarge       = gitReason{"request_too_large", "a request larger than the git cache carries"}
	gitGitHubUnreachable     = gitReason{"github_unreachable", "github.com did not answer"}
	gitRenamedOutsideOwner   = gitReason{"renamed_outside_owner", "renamed outside the tier's owner"}
	gitNotAuthorised         = gitReason{"not_authorised", "github.com did not authorise the request"}
	gitNoRepositoryID        = gitReason{"no_repository_id", "could not learn which repository the name is"}
	gitMirrorUnavailable     = gitReason{"mirror_unavailable", "the mirror could not answer"}
	gitNoGrant               = gitReason{"no_grant", "github.com has not authorised this header within the minute"}
	gitProtocolV2            = gitReason{"protocol_v2", "protocol v2"}
	gitMirrorBeingRemoved    = gitReason{"mirror_being_removed", "the mirror is being removed"}
	gitNoMirror              = gitReason{"no_mirror", "no mirror yet"}
	gitMirrorNotServable     = gitReason{"mirror_not_servable", "the mirror is not one this repository can be served from"}
	gitNegotiationTooLarge   = gitReason{"negotiation_too_large", "a negotiation larger than the proxy reads"}
	gitNegotiationUnreadable = gitReason{"negotiation_unreadable", "a negotiation the proxy cannot read"}
	gitMirrorLacksObject     = gitReason{"mirror_lacks_object", "the mirror lacks an object the fetch names"}
	gitGrantEnded            = gitReason{"grant_ended", "github.com's grant ended before the mirror answered"}
	gitMirrorDidNotAnswer    = gitReason{"mirror_did_not_answer", "the mirror's upload-pack did not answer"}
	gitUnanswered            = gitReason{"unanswered", "the request ended before it was answered"}
	gitNotAGitPath           = gitReason{"not_a_git_path", "not a repository's git path"}
	gitCredentialsRequired   = gitReason{"credentials_required", "credentials required"}
	gitTransferRefused       = gitReason{"transfer_refused", "could not extend the transfer's deadline"}
	gitRedirected            = gitReason{"redirected", "github.com redirected the request"}
	gitGitHubFailed          = gitReason{"github_error", "github.com answered with an error"}
	gitReasons               = []gitReason{gitServed, gitBusy, gitCacheOff, gitOutsideOwner, gitNotAFetch,
		gitUnreadableRequest, gitRequestTooLarge, gitGitHubUnreachable, gitRenamedOutsideOwner, gitNotAuthorised,
		gitNoRepositoryID, gitMirrorUnavailable, gitNoGrant, gitProtocolV2, gitMirrorBeingRemoved, gitNoMirror,
		gitMirrorNotServable, gitNegotiationTooLarge, gitNegotiationUnreadable, gitMirrorLacksObject,
		gitGrantEnded, gitMirrorDidNotAnswer, gitUnanswered, gitNotAGitPath, gitCredentialsRequired,
		gitTransferRefused, gitRedirected, gitGitHubFailed}
)

// GitReasons is every reason a request is counted under.
func GitReasons() []string {
	labels := make([]string, 0, len(gitReasons))
	for _, r := range gitReasons {
		labels = append(labels, r.label)
	}

	return labels
}

// gitStatusClass is a response status as the class it is counted under; none
// is a request that ended before it answered.
func gitStatusClass(status int) string {
	switch {
	case status < 200:
		return "none"
	case status < 300:
		return "2xx"
	case status < 400:
		return "3xx"
	case status < 500:
		return "4xx"
	default:
		return "5xx"
	}
}

// GitStatusClasses is every status class a request is counted under.
func GitStatusClasses() []string { return []string{"none", "2xx", "3xx", "4xx", "5xx"} }

// How a refresh of a mirror ended.
const (
	gitRefreshOK         = "ok"
	gitRefreshUnverified = "unverified"
	gitRefreshTooLarge   = "too_large"
	gitRefreshBusy       = "busy"
	gitRefreshFull       = "disk_full"
	gitRefreshClosed     = "closed"
	gitRefreshFailed     = "failed"
)

// GitRefreshOutcomes is every outcome a refresh is counted under.
func GitRefreshOutcomes() []string {
	return []string{gitRefreshOK, gitRefreshUnverified, gitRefreshTooLarge, gitRefreshBusy, gitRefreshFull,
		gitRefreshClosed, gitRefreshFailed}
}

// Sentinels a refresh's error carries, so how it ended is counted by what it
// was rather than by its text.
var (
	errFetchSlotsBusy   = errors.New("every mirror fetch slot is busy; github.com answers this one")
	errGitDiskFull      = errors.New("the git cache's filesystem is full")
	errFetchNotVerified = errors.New("github.com did not vouch for what the fetch brought")
)

// gitRefreshOutcome is how a refresh that ended with err is counted.
func gitRefreshOutcome(err error) string {
	switch {
	case err == nil:
		return gitRefreshOK
	// CLOSE FIRST: a refresh close cut ends with whatever its fetch looked like
	// joined to errGitClosed.
	case errors.Is(err, errGitClosed):
		return gitRefreshClosed
	case errors.Is(err, errFetchNotVerified):
		return gitRefreshUnverified
	case errors.Is(err, errMirrorTooLarge):
		return gitRefreshTooLarge
	case errors.Is(err, errFetchSlotsBusy):
		return gitRefreshBusy
	case errors.Is(err, errGitDiskFull):
		return gitRefreshFull
	default:
		return gitRefreshFailed
	}
}

// What GitHub's API answered when asked which repository a name is.
const (
	gitLookupOK      = "ok"
	gitLookupRefused = "refused"
	gitLookupLimited = "rate_limited"
	gitLookupFailed  = "failed"
)

// GitLookupOutcomes is every outcome a lookup is counted under.
func GitLookupOutcomes() []string {
	return []string{gitLookupOK, gitLookupRefused, gitLookupLimited, gitLookupFailed}
}

// gitLookupOutcome is how a lookup answered status is counted, status zero
// being one that got no answer at all. A 403 is counted as rate limited only
// when GitHub says the limit is spent, since it also refuses a header that
// cannot read the repository with one.
func gitLookupOutcome(status int, remaining string) string {
	switch {
	case status == http.StatusOK:
		return gitLookupOK
	case status == http.StatusTooManyRequests, status == http.StatusForbidden && remaining == "0":
		return gitLookupLimited
	case status == http.StatusUnauthorized, status == http.StatusForbidden, status == http.StatusNotFound:
		return gitLookupRefused
	default:
		return gitLookupFailed
	}
}
