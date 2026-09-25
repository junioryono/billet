package node

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
)

// The Git proxy serves `git fetch` of github.com repositories from a mirror on
// the node. A guest's git reaches it through url.insteadOf, which drops the
// header actions/checkout scopes to https://github.com/ (measured, git 2.51.0,
// 2026-09-25), so the guest's credential helper hands that header back as the
// Basic password, beside the session bearer as the user name.
//
// EVERY FETCH IS AUTHORISED BY GITHUB, with the job's own header, before a byte
// of the mirror is served: the mirror holds what GitHub gave some job that could
// read the repository, and this job is served it only when GitHub says this job
// can read it too. Nothing a job sends is written into a mirror.
//
// PROTOCOL v0 ONLY. Under v2 upload-pack serves any object in the repository,
// advertised or not, even with uploadpack.allowAnySHA1InWant=false; under v0 it
// refuses an object no ref reaches (measured, git 2.51.0, 2026-09-25). A mirror
// is pruned to upstream's refs, so an object only a deleted branch reached is
// never served from it.
const (
	gitPathPrefix = "/v1/git/github.com/"
	// gitAuthorisationAge is how long GitHub's yes for one header and one
	// repository stands, so a fetch's POST is not authorised twice.
	gitAuthorisationAge = time.Minute
	// gitAdvertisementLimit bounds upstream's ref advertisement.
	gitAdvertisementLimit = 256 << 20
	// gitRequestLimit bounds a fetch negotiation a client sends.
	gitRequestLimit = 64 << 20
	// gitMirrorRetention is how long a mirror no job has used is kept.
	gitMirrorRetention = 7 * 24 * time.Hour
	// gitFullFraction is how full the mirrors' filesystem may get before the
	// least recently used are removed and new ones are not made.
	gitFullFraction = 0.8
	// gitTooLargeFor is how long a repository whose mirror exceeded its tier's
	// ceiling is forwarded rather than mirrored again.
	gitTooLargeFor = time.Hour
	gitUsedStamp   = "billet-used"
	// gitFetches bounds the mirror fetches the node runs at once, so what they
	// write together is bounded by that many tiers' ceilings; gitWork bounds
	// every git request the node is serving, each a subprocess or an upstream
	// connection.
	gitFetches = 2
	gitWork    = 32
	// gitFetchWait is how long a refresh waits for a fetch slot before GitHub
	// answers instead: a slot held by another job's large fetch must not stall
	// this job's checkout.
	gitFetchWait = 2 * time.Second
	// gitSessionWork bounds one session's git requests at once, apart from its
	// content-addressed transfers, so neither can starve the other.
	gitSessionWork = 8
	// gitGrantMemory is how long an answer's sequence is kept after it expires:
	// longer than any request that could still be asking, so an older answer
	// finishing late always meets the newer one it must not overtake.
	gitGrantMemory = casRequestLife + 5*time.Minute
	// gitWatchEvery is how often a fetch in progress is measured against its
	// ceiling and the filesystem's fill.
	gitWatchEvery = 500 * time.Millisecond
)

// errMirrorTooLarge says a mirror passed its tier's ceiling and is removed.
var errMirrorTooLarge = errors.New("the repository is larger than the tier's git cache")

var gitSegment = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)

// uploadPackConfig is how the mirror is served, advertised and fetched alike: a
// want any ref reaches is allowed, as GitHub allows it, so checkout's fetch of a
// commit that is no longer a tip works; one no ref reaches is not.
var uploadPackConfig = []string{"-c", "uploadpack.allowReachableSHA1InWant=true",
	"-c", "uploadpack.allowFilter=true"}

// gitProxy is the node's mirrors and what it remembers about them.
type gitProxy struct {
	upstream string
	root     string
	binary   string
	client   *http.Client
	// fullFraction is gitFullFraction, a field so a test is not decided by the
	// fill of the disk it runs on.
	fullFraction float64

	fetches chan struct{}
	work    chan struct{}
	reaping atomic.Bool

	mu         sync.Mutex
	mirrors    map[string]*gitMirror
	authorised map[string]gitAuthorisation
	tooLarge   map[string]time.Time
	// renamed maps a repository GitHub redirected to where it now lives.
	renamed map[string]gitRename
	// sequence orders GitHub's answers, so a later one always wins over an
	// earlier one that finished after it.
	sequence uint64
}

// gitRename is where GitHub said a repository moved, and until when that is
// believed without asking again. It is ordered by the question it answered like
// a grant, and a later answer that is not a redirect leaves a withdrawal with
// no destination, kept as a refusal is.
type gitRename struct {
	owner, repo string
	until       time.Time
	sequence    uint64
	at          time.Time
}

// gitRenameAge is how long a redirect GitHub gave to one header's advertisement
// is followed for that header's fetch: as long as the grant it came with.
const gitRenameAge = gitAuthorisationAge

// gitAuthorisation is GitHub's yes to one header for one repository, and
// whether the mirror answered the advertisement it came with: a fetch is served
// from the mirror only then, because a mirror whose refresh failed is stale.
type gitAuthorisation struct {
	until    time.Time
	mirrored bool
	sequence uint64
	at       time.Time
}

type gitMirror struct {
	// fetch serialises refreshes; use is held for reading while a mirror is
	// served or refreshed and for writing while it is removed.
	fetch sync.Mutex
	use   sync.RWMutex
}

func newGitProxy(root string) *gitProxy {
	return &gitProxy{
		upstream: "https://github.com", root: root, binary: "git",
		client: &http.Client{
			Timeout: 2 * time.Minute,
			// A redirect is the client's to follow, never the node's: following
			// it would carry the job's header wherever upstream pointed.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		fullFraction: gitFullFraction,
		fetches:      make(chan struct{}, gitFetches),
		work:         make(chan struct{}, gitWork),
		mirrors:      make(map[string]*gitMirror), authorised: make(map[string]gitAuthorisation),
		tooLarge: make(map[string]time.Time),
		renamed:  make(map[string]gitRename),
	}
}

// gitAuth is the job's upstream Authorization header value. It prints as
// redacted, so no log or error can carry it.
type gitAuth string

func (gitAuth) String() string   { return "[redacted]" }
func (gitAuth) GoString() string { return "[redacted]" }

// gitRequest is one parsed proxy request.
type gitRequest struct {
	session *cacheSession
	owner   string
	repo    string
	// suffix is the repository path as the client spelled it (".git" or not),
	// rest what follows it.
	suffix string
	rest   string
	auth   gitAuth
}

func (g gitRequest) key() string {
	return filepath.Join(g.session.trust.String(), strings.ToLower(g.owner), strings.ToLower(g.repo))
}

func (g gitRequest) upstreamPath() string {
	return "/" + g.owner + "/" + g.repo + g.suffix + "/" + g.rest
}

// parseGitRequest reads `/v1/git/github.com/{owner}/{repo}[.git]/{rest}` and
// the Basic credentials: the session bearer, and the job's header or "-".
func (s *CacheService) parseGitRequest(r *http.Request) (gitRequest, bool, bool) {
	path, ok := strings.CutPrefix(r.URL.Path, gitPathPrefix)
	if !ok {
		return gitRequest{}, false, false
	}
	parts := strings.SplitN(path, "/", 3)
	if len(parts) != 3 || !gitSegment.MatchString(parts[0]) || parts[2] == "" {
		return gitRequest{}, false, false
	}
	repo, suffix := strings.CutSuffix(parts[1], ".git")
	request := gitRequest{owner: parts[0], repo: repo, rest: parts[2]}
	if suffix {
		request.suffix = ".git"
	}
	if !gitSegment.MatchString(repo) || strings.HasPrefix(repo, ".") || strings.HasPrefix(parts[0], ".") {
		return gitRequest{}, false, false
	}

	user, password, ok := r.BasicAuth()
	if !ok {
		return request, true, false
	}
	s.mu.Lock()
	session := s.byToken[user]
	s.mu.Unlock()
	if session == nil || len(password) > 8<<10 || strings.ContainsAny(password, "\r\n") {
		return request, true, false
	}
	request.session = session
	if password != "-" {
		request.auth = gitAuth(password)
	}

	return request, true, true
}

// serveGit answers one request under gitPathPrefix.
func (s *CacheService) serveGit(w http.ResponseWriter, r *http.Request) {
	request, ok, authenticated := s.parseGitRequest(r)
	if !ok {
		http.NotFound(w, r)

		return
	}
	// ASKED FOR CREDENTIALS, which makes git run the guest's helper: the first
	// request of every fetch carries none, because the rewrite dropped them.
	if !authenticated {
		w.Header().Set("WWW-Authenticate", `Basic realm="billet"`)
		http.Error(w, "credentials required", http.StatusUnauthorized)

		return
	}
	ctx, cancel := extendTransfer(r.Context(), w)
	defer cancel()
	// THE TRANSFER'S DEADLINE GOES WITH EVERY BRANCH, a forward included.
	r = r.WithContext(ctx)

	// ADMITTED LIKE ANY OTHER TRANSFER, per session and per node, because each
	// request is a subprocess or an upstream connection a guest could multiply.
	release, err := admitGit(ctx, request.session.gitAdmit, s.git.work)
	if err != nil {
		http.Error(w, "the git cache is busy", http.StatusServiceUnavailable)

		return
	}
	defer release()

	// A FETCH GOES WHERE ITS OWN ADVERTISEMENT WAS JUST REDIRECTED. git keeps
	// its old base URL when it follows a redirect on the retry after a 401
	// (measured, git 2.51.0, 2026-09-25), and every fetch through here takes that
	// retry, so the node follows it; an advertisement always asks GitHub about
	// the name it was given, so a rename is never believed past what GitHub is
	// saying now to this header.
	if r.Method == http.MethodPost {
		request = s.git.follow(request, s.now())
	}

	switch {
	case !s.gitAllowed(ctx, request.session) || !s.mirrorsOwner(request):
		s.forwardGit(w, r, request)
	case r.Method == http.MethodGet && request.rest == "info/refs" &&
		r.URL.Query().Get("service") == "git-upload-pack":
		s.gitAdvertise(ctx, w, r, request)
	case r.Method == http.MethodPost && request.rest == "git-upload-pack":
		s.gitUploadPack(ctx, w, r, request)
	default:
		s.forwardGit(w, r, request)
	}
}

// admitGit takes a slot of each bound in turn, waiting as long as ctx lets it.
func admitGit(ctx context.Context, bounds ...chan struct{}) (func(), error) {
	var held []chan struct{}
	release := func() {
		for _, bound := range held {
			<-bound
		}
	}
	for _, bound := range bounds {
		select {
		case bound <- struct{}{}:
			held = append(held, bound)
		case <-ctx.Done():
			release()

			return nil, ctx.Err()
		}
	}

	return release, nil
}

// gitAllowed reports whether the session's tier enables the git cache and no
// kill switch blocks it; could-not-tell forwards, which is never wrong.
func (s *CacheService) gitAllowed(ctx context.Context, session *cacheSession) bool {
	if err := lockCacheSession(ctx, session); err != nil {
		return false
	}
	defer session.mu.Unlock()
	if session.closed || !sessionSetting(session, config.CacheGit).Enabled {
		return false
	}
	if s.now().Sub(session.gitAllowedAt) < casPolicyAge {
		return true
	}
	if !s.sessionKindAllowed(ctx, session, config.CacheGit) {
		noteBuildCache(session, config.CacheGit, alloc.BuildCacheDisabled)

		return false
	}
	session.gitAllowedAt = s.now()

	return true
}

// forwardGit sends the request to GitHub as the job would have, with the job's
// own header and nothing of billet's.
func (s *CacheService) forwardGit(w http.ResponseWriter, r *http.Request, request gitRequest) {
	target, err := url.Parse(s.git.upstream)
	if err != nil {
		http.Error(w, "the git upstream is misconfigured", http.StatusBadGateway)

		return
	}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(out *httputil.ProxyRequest) {
			out.Out.URL.Scheme, out.Out.URL.Host = target.Scheme, target.Host
			out.Out.URL.Path, out.Out.URL.RawPath = request.upstreamPath(), ""
			out.Out.URL.RawQuery = r.URL.RawQuery
			out.Out.Host = target.Host
			out.Out.Header.Del("Authorization")
			if request.auth != "" {
				out.Out.Header.Set("Authorization", string(request.auth))
			}
		},
		Transport: s.git.client.Transport,
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
			http.Error(w, "github.com is unreachable", http.StatusBadGateway)
		},
	}
	proxy.ServeHTTP(w, r)
}

// upstreamRefs asks GitHub for the repository's v0 advertisement with the job's
// header. A status other than 200 is GitHub's answer to this job, relayed.
func (s *CacheService) upstreamRefs(ctx context.Context, request gitRequest) (*http.Response, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		s.git.upstream+"/"+request.owner+"/"+request.repo+".git/info/refs?service=git-upload-pack", http.NoBody)
	if err != nil {
		return nil, nil, err
	}
	if request.auth != "" {
		req.Header.Set("Authorization", string(request.auth))
	}
	req.Header.Set("User-Agent", "git/2 billet")
	resp, err := s.git.client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, gitAdvertisementLimit+1))
	if err != nil {
		return nil, nil, err
	}
	if len(body) > gitAdvertisementLimit {
		return nil, nil, errors.New("the advertisement is larger than the proxy reads")
	}

	return resp, body, nil
}

func (g *gitProxy) authorisationKey(request gitRequest) string {
	sum := sha256.Sum256([]byte(string(request.auth) + "\x00" + request.key()))

	return hex.EncodeToString(sum[:])
}

// follow is request aimed where GitHub just told this header its repository
// moved.
func (g *gitProxy) follow(request gitRequest, now time.Time) gitRequest {
	g.mu.Lock()
	defer g.mu.Unlock()
	moved, ok := g.renamed[g.authorisationKey(request)]
	if ok && now.Before(moved.until) {
		request.owner, request.repo = moved.owner, moved.repo
	}

	return request
}

// mirrorsOwner reports whether the node keeps mirrors of this repository's
// owner for the session: its scope's owner when it has one, so a job cannot
// fill the node with mirrors of arbitrary public repositories.
func (s *CacheService) mirrorsOwner(request gitRequest) bool {
	owner := request.session.cacheOwner()

	return owner == "" || strings.EqualFold(owner, request.owner)
}

// learnRename records GitHub's answer to question sequence about request's
// name: a redirect to another repository on github.com is followed by this
// header's fetch, and reported; anything else withdraws an earlier redirect,
// so a fetch never follows a rename GitHub has since stopped giving.
func (g *gitProxy) learnRename(
	request gitRequest, resp *http.Response, now time.Time, sequence uint64,
) (gitRequest, bool) {
	moved, ok := g.renameTarget(request, resp)
	g.mu.Lock()
	defer g.mu.Unlock()
	for key, held := range g.renamed {
		if now.Sub(held.at) > gitGrantMemory {
			delete(g.renamed, key)
		}
	}
	key := g.authorisationKey(request)
	if g.renamed[key].sequence > sequence {
		return request, false
	}
	held := gitRename{sequence: sequence, at: now}
	if ok {
		held.owner, held.repo, held.until = moved.owner, moved.repo, now.Add(gitRenameAge)
	}
	g.renamed[key] = held

	return moved, ok
}

// renameTarget is where resp redirects request's advertisement, when that is
// another repository on github.com.
func (g *gitProxy) renameTarget(request gitRequest, resp *http.Response) (gitRequest, bool) {
	if resp == nil || resp.StatusCode < 300 || resp.StatusCode > 399 {
		return request, false
	}
	rest, ok := strings.CutPrefix(resp.Header.Get("Location"), g.upstream+"/")
	if !ok {
		return request, false
	}
	path, _, _ := strings.Cut(rest, "?")
	parts := strings.SplitN(path, "/", 3)
	if len(parts) != 3 || parts[2] != "info/refs" || !gitSegment.MatchString(parts[0]) {
		return request, false
	}
	repo := strings.TrimSuffix(parts[1], ".git")
	if !gitSegment.MatchString(repo) || strings.HasPrefix(repo, ".") || strings.HasPrefix(parts[0], ".") ||
		strings.EqualFold(parts[0]+"/"+repo, request.owner+"/"+request.repo) {
		return request, false
	}
	request.owner, request.repo = parts[0], repo

	return request, true
}

// ask numbers one question to GitHub about a header and a repository.
func (g *gitProxy) ask() uint64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.sequence++

	return g.sequence
}

// remember records GitHub's answer to question sequence: a grant for
// gitAuthorisationAge, or, with granted false, the end of any earlier grant. An
// answer to an older question than the one recorded changes nothing, so a
// refusal is never overtaken by a grant asked for before it.
func (g *gitProxy) remember(request gitRequest, now time.Time, sequence uint64, granted, mirrored bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	// KEPT PAST ITS EXPIRY, a refusal's included, until no older question can
	// still be in flight: deleting it sooner lets that question's late grant
	// land as though nothing newer had been said.
	for key, held := range g.authorised {
		if now.Sub(held.at) > gitGrantMemory {
			delete(g.authorised, key)
		}
	}
	key := g.authorisationKey(request)
	if g.authorised[key].sequence > sequence {
		return
	}
	held := gitAuthorisation{sequence: sequence, at: now}
	if granted {
		held.until, held.mirrored = now.Add(gitAuthorisationAge), mirrored
	}
	g.authorised[key] = held
}

// servesFromMirror reports whether GitHub authorised this header for this
// repository within gitAuthorisationAge and the mirror answered it then.
func (g *gitProxy) servesFromMirror(request gitRequest, now time.Time) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	held := g.authorised[g.authorisationKey(request)]

	return held.mirrored && now.Before(held.until)
}

func (g *gitProxy) mirror(key string) *gitMirror {
	g.mu.Lock()
	defer g.mu.Unlock()
	m := g.mirrors[key]
	if m == nil {
		m = &gitMirror{}
		g.mirrors[key] = m
	}

	return m
}

func (g *gitProxy) mirrorPath(key string) string { return filepath.Join(g.root, key+".git") }

// gitAdvertise authorises the job with GitHub, brings the mirror to the refs
// GitHub advertised, and answers with the mirror's own advertisement. Anything
// the mirror cannot do sends GitHub's answer instead, and the fetch after it to
// GitHub too.
func (s *CacheService) gitAdvertise(ctx context.Context, w http.ResponseWriter, r *http.Request, request gitRequest) {
	sequence := s.git.ask()
	resp, body, err := s.upstreamRefs(ctx, request) //nolint:bodyclose // upstreamRefs read and closed it; the status and headers travel
	// ONE HOP, followed here for the reason serveGit follows a known one. Could
	// not tell withdraws an earlier rename as any other answer does.
	if moved, ok := s.git.learnRename(request, resp, s.now(), sequence); ok && err == nil {
		// A RENAME OUT OF THE SCOPE'S OWNER IS NOT MIRRORED, as a request for
		// that repository by its own name would not be: GitHub's redirect goes
		// back to the client, whose fetch then follows it to GitHub.
		if !s.mirrorsOwner(moved) {
			s.git.remember(moved, s.now(), sequence, false, false)
			s.relayGit(w, r, resp, body)

			return
		}
		// THE FOLLOWED NAME'S ANSWER IS A QUESTION OF ITS OWN, numbered when it
		// is asked and recorded against that name's rename as well as its grant,
		// so a refusal of it outranks whatever was said about it before. A second
		// redirect is recorded and relayed, never followed.
		request, sequence = moved, s.git.ask()
		resp, body, err = s.upstreamRefs(ctx, request) //nolint:bodyclose // as above
		s.git.learnRename(request, resp, s.now(), sequence)
	}
	if err != nil {
		// COULD NOT TELL ENDS WHATEVER WAS GRANTED BEFORE, as a refusal does.
		s.git.remember(request, s.now(), sequence, false, false)
		s.log.Warn("could not ask github.com about a repository; forwarding",
			"repository", request.owner+"/"+request.repo, "error", err)
		s.forwardGit(w, r, request)

		return
	}
	if resp.StatusCode != http.StatusOK {
		s.git.remember(request, s.now(), sequence, false, false)
		s.relayGit(w, r, resp, body)

		return
	}

	s.gitBegin(request.session)
	advertisement, existed, err := s.refreshMirror(ctx, request, body)
	s.git.remember(request, s.now(), sequence, true, err == nil)
	s.noteGit(ctx, request.session, err, existed)
	if err != nil {
		s.log.Info("the git cache could not serve a repository; github.com serves it",
			"repository", request.owner+"/"+request.repo, "error", err)
		s.relayGit(w, r, resp, body)

		return
	}
	w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
	w.Header().Set("Cache-Control", "no-cache")
	s.writeGit(w, pktLine("# service=git-upload-pack\n"), []byte("0000"), advertisement)
}

// writeGit writes an answer's parts. A write that fails is a client that went
// away, which ends this answer and nothing else.
func (s *CacheService) writeGit(w http.ResponseWriter, parts ...[]byte) {
	for _, part := range parts {
		if _, err := w.Write(part); err != nil {
			s.log.Debug("a git client went away mid-answer", "error", err)

			return
		}
	}
}

// relayGit answers with what GitHub answered. A redirect keeps its
// destination, and one within github.com is sent back through the proxy, so the
// client's next request carries its credentials here rather than going to
// GitHub without the header the rewrite dropped.
func (s *CacheService) relayGit(w http.ResponseWriter, r *http.Request, resp *http.Response, body []byte) {
	for _, name := range []string{"Content-Type", "WWW-Authenticate", "Cache-Control"} {
		if value := resp.Header.Get(name); value != "" {
			w.Header().Set(name, value)
		}
	}
	if location := resp.Header.Get("Location"); location != "" {
		if rest, ok := strings.CutPrefix(location, s.git.upstream+"/"); ok {
			location = "http://" + r.Host + gitPathPrefix + rest
		}
		w.Header().Set("Location", location)
	}
	w.WriteHeader(resp.StatusCode)
	s.writeGit(w, body)
}

func pktLine(payload string) []byte { return fmt.Appendf(nil, "%04x%s", len(payload)+4, payload) }

// advertisedRefs reads a v0 upload-pack advertisement: every ref and its object,
// peeled entries and HEAD aside, and what HEAD points to.
func advertisedRefs(body []byte) (map[string]string, string, error) {
	refs := make(map[string]string)
	head := ""
	for len(body) > 0 {
		if len(body) < 4 {
			return nil, "", errors.New("a truncated pkt-line")
		}
		var size int
		if _, err := fmt.Sscanf(string(body[:4]), "%04x", &size); err != nil {
			return nil, "", fmt.Errorf("a malformed pkt-line: %w", err)
		}
		if size == 0 {
			body = body[4:]

			continue
		}
		if size < 4 || size > len(body) {
			return nil, "", errors.New("a pkt-line longer than the advertisement")
		}
		line := strings.TrimSuffix(string(body[4:size]), "\n")
		body = body[size:]
		if strings.HasPrefix(line, "#") {
			continue
		}
		line, capabilities, _ := strings.Cut(line, "\x00")
		for _, capability := range strings.Fields(capabilities) {
			if target, ok := strings.CutPrefix(capability, "symref=HEAD:"); ok {
				head = target
			}
		}
		object, name, ok := strings.Cut(line, " ")
		if !ok || len(object) != 40 && len(object) != 64 {
			return nil, "", fmt.Errorf("a malformed ref line %q", line)
		}
		if name == "HEAD" || strings.HasSuffix(name, "^{}") || name == "capabilities^{}" {
			continue
		}
		refs[name] = object
	}

	return refs, head, nil
}

// noteGit records what the git cache did for one advertisement: served from a
// mirror that already existed, served from one made for it, or not served.
func (s *CacheService) noteGit(_ context.Context, session *cacheSession, err error, existed bool) {
	outcome := alloc.BuildCacheCold
	switch {
	case err != nil:
		outcome = alloc.BuildCacheUnavailable
	case existed:
		outcome = alloc.BuildCacheWarm
	}
	// UNDER THE SESSION'S LOCK EVEN WHEN THE REQUEST'S CONTEXT HAS ENDED, so the
	// pending mark is always cleared and the outcome always kept.
	session.mu.Lock()
	session.gitPending--
	noteBuildCache(session, config.CacheGit, outcome)
	session.mu.Unlock()
}

// gitBegin marks a refresh in flight, so settlement does not call the cache
// unused while its outcome is still being decided.
func (s *CacheService) gitBegin(session *cacheSession) {
	session.mu.Lock()
	session.gitPending++
	session.mu.Unlock()
}

// refreshMirror brings the repository's mirror to the refs upstream advertised,
// fetching only when they differ, and returns the mirror's advertisement and
// whether the mirror existed before this request.
func (s *CacheService) refreshMirror(
	ctx context.Context, request gitRequest, upstream []byte,
) ([]byte, bool, error) {
	key := request.key()
	s.git.mu.Lock()
	until := s.git.tooLarge[key]
	s.git.mu.Unlock()
	if s.now().Before(until) {
		return nil, false, errors.New("the repository is larger than the tier's git cache")
	}
	refs, head, err := advertisedRefs(upstream)
	if err != nil {
		return nil, false, err
	}
	if len(refs) == 0 {
		return nil, false, errors.New("upstream advertised no refs")
	}

	mirror := s.git.mirror(key)
	mirror.use.RLock()
	advertisement, existed, err := s.refreshHeld(ctx, request, mirror, refs, head)
	mirror.use.RUnlock()
	// REMOVED UNDER THE EXCLUSIVE LOCK, once this request's own hold is gone,
	// so no fetch being served loses its repository mid-pack.
	if errors.Is(err, errMirrorTooLarge) {
		s.git.mu.Lock()
		s.git.tooLarge[key] = s.now().Add(gitTooLargeFor)
		s.git.mu.Unlock()
		if lockWithin(ctx, &mirror.use, casReleaseWait) {
			if removeErr := os.RemoveAll(s.git.mirrorPath(key)); removeErr != nil {
				err = errors.Join(err, removeErr)
			}
			mirror.use.Unlock()
		}
	}

	return advertisement, existed, err
}

// refreshHeld is refreshMirror's work, with the mirror held for reading.
func (s *CacheService) refreshHeld(
	ctx context.Context, request gitRequest, mirror *gitMirror, refs map[string]string, head string,
) ([]byte, bool, error) {
	key := request.key()
	path := s.git.mirrorPath(key)

	mirror.fetch.Lock()
	current, err := s.mirrorRefs(ctx, path)
	existed := err == nil && len(current) > 0
	if err != nil || !sameRefs(current, refs) {
		if full, statErr := filledAbove(s.git.root, s.git.fullFraction); statErr == nil && full {
			mirror.fetch.Unlock()
			s.wakeGitReaper(ctx)

			return nil, false, errors.New("the git cache's filesystem is full")
		}
		err = s.fetchMirror(ctx, path, request,
			volumeCeiling(sessionSetting(request.session, config.CacheGit)))
	}
	if err == nil && head != "" && strings.HasPrefix(head, "refs/") {
		_, err = s.runGit(ctx, path, nil, "symbolic-ref", "HEAD", head)
	}
	mirror.fetch.Unlock()
	if err != nil {
		return nil, false, err
	}
	s.touchMirror(path)

	advertisement, err := s.runGit(ctx, path, nil, append(slices.Clone(uploadPackConfig),
		"upload-pack", "--stateless-rpc", "--advertise-refs", ".")...)

	return advertisement, existed, err
}

func sameRefs(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for name, object := range a {
		if b[name] != object {
			return false
		}
	}

	return true
}

// mirrorRefs lists a mirror's refs; a mirror that does not exist has none.
func (s *CacheService) mirrorRefs(ctx context.Context, path string) (map[string]string, error) {
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		return map[string]string{}, nil
	}
	output, err := s.runGit(ctx, path, nil, "for-each-ref", "--format=%(objectname) %(refname)")
	if err != nil {
		return nil, err
	}
	refs := make(map[string]string)
	scanner := bufio.NewScanner(bytes.NewReader(output))
	for scanner.Scan() {
		if object, name, ok := strings.Cut(scanner.Text(), " "); ok {
			refs[name] = object
		}
	}

	return refs, scanner.Err()
}

// fetchMirror makes or refreshes a mirror with the job's header, which reaches
// git through its environment and never its argv, a file or a log.
//
// BOUNDED WHILE IT RUNS, not only after: a node-wide slot caps the fetches in
// flight, and a watchdog stops one whose mirror passes the tier's ceiling or
// whose filesystem passes its fill, before either can exhaust the node's disk.
func (s *CacheService) fetchMirror(ctx context.Context, path string, request gitRequest, ceiling int64) error {
	waitCtx, cancel := context.WithTimeout(ctx, gitFetchWait)
	release, err := admitGit(waitCtx, request.session.gitFetch, s.git.fetches)
	cancel()
	if err != nil {
		return errors.New("every mirror fetch slot is busy; github.com answers this one")
	}
	defer release()
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return err
		}
		if _, err := s.runGit(ctx, filepath.Dir(path), nil, "init", "--bare", "--quiet", path); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(path, gitUsedStamp), nil, 0o600); err != nil {
			return err
		}
	}
	var secret []string
	if request.auth != "" {
		secret = []string{"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=http.extraheader",
			"GIT_CONFIG_VALUE_0=Authorization: " + string(request.auth)}
	}
	watched, stop := context.WithCancelCause(ctx)
	defer stop(nil)
	done := make(chan struct{})
	defer close(done)
	go func() {
		ticker := time.NewTicker(gitWatchEvery)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
			}
			if directorySize(path) > ceiling {
				stop(errMirrorTooLarge)

				return
			}
			if full, err := filledAbove(s.git.root, s.git.fullFraction); err == nil && full {
				stop(errors.New("the git cache's filesystem is full"))

				return
			}
		}
	}()
	_, err = s.runGit(watched, path, secret, "fetch", "--prune", "--quiet", "--no-write-fetch-head",
		s.git.upstream+"/"+request.owner+"/"+request.repo+".git", "+refs/*:refs/*")
	if cause := context.Cause(watched); cause != nil && !errors.Is(cause, context.Canceled) {
		return cause
	}
	if err == nil && directorySize(path) > ceiling {
		return errMirrorTooLarge
	}

	return err
}

// wakeGitReaper reaps unused mirrors now rather than at the next maintenance
// pass, once, when the filesystem is found full.
//
// THE REAP OUTLIVES THE REQUEST THAT FOUND THE FILESYSTEM FULL, so it keeps the
// request's values and drops its cancellation, bounded on its own.
func (s *CacheService) wakeGitReaper(ctx context.Context) {
	if !s.git.reaping.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer s.git.reaping.Store(false)
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), casRequestLife)
		defer cancel()
		if err := s.ReapGitMirrors(ctx); err != nil {
			s.log.Warn("could not reap git mirrors on a full filesystem", "error", err)
		}
	}()
}

// directorySize is the bytes of the regular files under path.
func directorySize(path string) int64 {
	var size int64
	// The callback skips what it cannot read and never fails, so the walk
	// cannot either; a tree read in part is sized by the part.
	if err := filepath.WalkDir(path, func(_ string, entry fs.DirEntry, err error) error {
		if err == nil && entry.Type().IsRegular() {
			if info, err := entry.Info(); err == nil {
				size += info.Size()
			}
		}

		return nil
	}); err != nil {
		return size
	}

	return size
}

// killGroup makes cancelling cmd end every process it started, not only git:
// fetch runs index-pack, and upload-pack pack-objects, which would otherwise go
// on writing after the slot and the lock that bounded them were released.
func killGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
}

// runGit runs git in dir with a closed environment: no system or global
// configuration, no prompt, protocol v0, and only what extra adds.
func (s *CacheService) runGit(ctx context.Context, dir string, extra []string, args ...string) ([]byte, error) {
	// #nosec G204 -- git is the node's configured binary, there is no shell,
	// and every caller builds args from constants, paths the node derived and
	// the HEAD symref GitHub advertised, admitted only when it starts with
	// refs/, so no value can be read as an option.
	cmd := exec.CommandContext(ctx, s.git.binary, args...)
	killGroup(cmd)
	cmd.Dir = dir
	cmd.Env = append([]string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + s.git.root, "GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=/bin/false",
	}, extra...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.WaitDelay = 5 * time.Second
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", args[len(args)-1], err, boundedOutput(stderr.Bytes()))
	}

	return output, nil
}

// gitUploadPack serves a fetch from the mirror, to a job GitHub authorised for
// the repository within gitAuthorisationAge; anything else goes to GitHub.
func (s *CacheService) gitUploadPack(ctx context.Context, w http.ResponseWriter, r *http.Request, request gitRequest) {
	// NOTHING IS SERVED FROM THE MIRROR BUT A FETCH WHOSE ADVERTISEMENT IT
	// ANSWERED: GitHub authorised the header then, and the mirror was brought to
	// GitHub's refs then. Every other fetch is GitHub's to answer.
	if !s.git.servesFromMirror(request, s.now()) {
		s.forwardGit(w, r, request)

		return
	}
	key := request.key()
	mirror := s.git.mirror(key)
	mirror.use.RLock()
	defer mirror.use.RUnlock()
	path := s.git.mirrorPath(key)
	if _, err := os.Stat(filepath.Join(path, "HEAD")); err != nil {
		s.forwardGit(w, r, request)

		return
	}

	raw, err := io.ReadAll(io.LimitReader(r.Body, gitRequestLimit+1))
	if err != nil || len(raw) > gitRequestLimit {
		http.Error(w, "an unreadable or oversized request", http.StatusBadRequest)

		return
	}
	negotiation := raw
	if r.Header.Get("Content-Encoding") == "gzip" {
		decompressed, err := gzip.NewReader(bytes.NewReader(raw))
		if err == nil {
			negotiation, err = io.ReadAll(io.LimitReader(decompressed, gitRequestLimit+1))
		}
		if err != nil || len(negotiation) > gitRequestLimit {
			http.Error(w, "an unreadable or oversized request", http.StatusBadRequest)

			return
		}
	}

	// A WANT THE MIRROR MAY NOT ANSWER GOES TO GITHUB, WHOLE. upload-pack's own
	// check under v0 walks commits alone, so a want for a tree or a blob no ref
	// reaches passes it (measured, git 2.51.0, 2026-09-25); the mirror answers
	// only wants that are commits, which the check does judge, or a current
	// ref's own object. Anything else, a partial clone's blob fetch included, is
	// GitHub's to answer, and GitHub authorised this header for this repository.
	wants, shallow := negotiatedWants(negotiation)
	if shallow || !s.mirrorServesWants(ctx, path, wants) {
		r.Body, r.ContentLength = io.NopCloser(bytes.NewReader(raw)), int64(len(raw))
		s.forwardGit(w, r, request)

		return
	}

	// #nosec G204 -- git is the node's configured binary and the arguments are
	// constants; the guest's request reaches it only on stdin.
	cmd := exec.CommandContext(ctx, s.git.binary, append(slices.Clone(uploadPackConfig),
		"upload-pack", "--stateless-rpc", ".")...)
	killGroup(cmd)
	cmd.Dir = path
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + s.git.root, "GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null"}
	cmd.Stdin = bytes.NewReader(negotiation)
	cmd.WaitDelay = 5 * time.Second
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
	w.Header().Set("Cache-Control", "no-cache")
	cmd.Stdout = w
	if err := cmd.Run(); err != nil {
		s.log.Warn("a fetch from the git cache failed", "repository", request.owner+"/"+request.repo,
			"error", err, "stderr", boundedOutput(stderr.Bytes()))
	}
	s.touchMirror(path)
}

// touchMirror marks a mirror used now, which is what the reaper reads. A mark
// that cannot be written leaves the mirror looking older than it is, so the
// reaper may take it sooner; nothing a job depends on.
func (s *CacheService) touchMirror(path string) {
	now := s.now()
	if err := os.Chtimes(filepath.Join(path, gitUsedStamp), now, now); err != nil {
		s.log.Debug("could not mark a git mirror used", "mirror", path, "error", err)
	}
}

// negotiatedWants reads the objects a v0 fetch negotiation wants, every
// `want <oid>` pkt-line before the first flush, and whether it names a shallow
// boundary. A client's `shallow <oid>` makes upload-pack send that commit's
// parents without judging them (git 2.51.0's send_unshallow), so a negotiation
// that names one is GitHub's to answer; `deepen` alone walks from the wants,
// which are judged.
func negotiatedWants(negotiation []byte) ([]string, bool) {
	var wants []string
	shallow := false
	for len(negotiation) >= 4 {
		var size int
		if _, err := fmt.Sscanf(string(negotiation[:4]), "%04x", &size); err != nil {
			return append(wants, ""), shallow
		}
		if size == 0 {
			break
		}
		if size < 4 || size > len(negotiation) {
			return append(wants, ""), shallow
		}
		line := strings.TrimSuffix(string(negotiation[4:size]), "\n")
		negotiation = negotiation[size:]
		if rest, ok := strings.CutPrefix(line, "want "); ok {
			object, _, _ := strings.Cut(rest, " ")
			wants = append(wants, object)
		}
		if strings.HasPrefix(line, "shallow ") {
			shallow = true
		}
	}

	return wants, shallow
}

// mirrorServesWants reports whether every want is a commit in the mirror or a
// current ref's own object. A want it cannot read, or cannot type, is not.
func (s *CacheService) mirrorServesWants(ctx context.Context, path string, wants []string) bool {
	if len(wants) == 0 {
		return true
	}
	refs, err := s.mirrorRefs(ctx, path)
	if err != nil {
		return false
	}
	tips := make(map[string]bool, len(refs))
	for _, object := range refs {
		tips[object] = true
	}
	var input strings.Builder
	for _, want := range wants {
		if len(want) != 40 && len(want) != 64 || strings.ContainsAny(want, " \n") {
			return false
		}
		input.WriteString(want + "\n")
	}
	// #nosec G204 -- git is the node's configured binary, and every object name
	// on stdin was just proved to be a 40- or 64-character hex id.
	cmd := exec.CommandContext(ctx, s.git.binary, "cat-file",
		"--batch-check=%(objectname) %(objecttype)")
	cmd.Dir = path
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + s.git.root, "GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null"}
	cmd.Stdin = strings.NewReader(input.String())
	cmd.WaitDelay = 5 * time.Second
	output, err := cmd.Output()
	if err != nil {
		return false
	}
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	if len(lines) != len(wants) {
		return false
	}
	for i, line := range lines {
		object, kind, _ := strings.Cut(line, " ")
		if object != wants[i] || kind != "commit" && !tips[object] {
			return false
		}
	}

	return true
}

// ReapGitMirrors removes the mirrors no job has used within gitMirrorRetention,
// then the least recently used while their filesystem is above gitFullFraction.
func (s *CacheService) ReapGitMirrors(ctx context.Context) error {
	type mirror struct {
		key  string
		used time.Time
	}
	var mirrors []mirror
	err := filepath.WalkDir(s.git.root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) && path == s.git.root {
				return fs.SkipAll
			}

			return err
		}
		if !entry.IsDir() || !strings.HasSuffix(path, ".git") {
			return nil
		}
		info, err := os.Stat(filepath.Join(path, gitUsedStamp))
		if err != nil {
			return fs.SkipDir
		}
		relative, err := filepath.Rel(s.git.root, strings.TrimSuffix(path, ".git"))
		if err != nil {
			return err
		}
		mirrors = append(mirrors, mirror{key: relative, used: info.ModTime()})

		return fs.SkipDir
	})
	if err != nil {
		return err
	}
	slices.SortFunc(mirrors, func(a, b mirror) int { return a.used.Compare(b.used) })

	var failures []error
	for _, m := range mirrors {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		full, err := filledAbove(s.git.root, s.git.fullFraction)
		if err != nil {
			return err
		}
		if !full && s.now().Sub(m.used) < gitMirrorRetention {
			break
		}
		// A MIRROR IN USE IS PASSED OVER, not waited for: this loop also renews
		// and cleans up every session, and a slow transfer holds a mirror for
		// minutes. It is reaped on a later pass.
		held := s.git.mirror(m.key)
		if !lockWithin(ctx, &held.use, time.Second) {
			continue
		}
		err = os.RemoveAll(s.git.mirrorPath(m.key))
		held.use.Unlock()
		if err != nil {
			failures = append(failures, err)
		}
	}

	return errors.Join(failures...)
}
