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
	"net"
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
// A JOB GITHUB AUTHORISED IS SERVED ANY OBJECT ITS REPOSITORY'S MIRROR HOLDS,
// because GitHub serves it the same: an object no ref reaches, a superseded pull
// request's merge commit or a fork's commit, is fetched by its name under v0 and
// v2 alike (measured, 2026-10-21, ADR-018). A mirror holds only what billet
// fetched from that repository, so it holds nothing GitHub would not give.
//
// NOTHING THAT GOES WRONG IN THE MIRROR FAILS A FETCH GITHUB WOULD ANSWER: a want
// the mirror lacks, a refusal from the mirror's own upload-pack, a refresh still
// running or one that failed, each sends the fetch to GitHub.
const (
	gitPathPrefix = "/v1/git/github.com/"
	// gitAuthorisationAge is how long GitHub's yes for one header and one
	// repository stands, so a fetch's POST is not authorised twice.
	gitAuthorisationAge = time.Minute
	// gitAdvertisementLimit bounds upstream's ref advertisement.
	gitAdvertisementLimit = 256 << 20
	// gitRequestLimit bounds the fetch negotiation the proxy reads and holds; a
	// larger one goes to GitHub as it arrives.
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
	// gitRefreshWait is how long an advertisement waits for its mirror to be
	// brought to GitHub's refs before GitHub's own advertisement is sent. The
	// refresh goes on for the jobs after it.
	gitRefreshWait = 10 * time.Second
	// gitRefreshLife bounds one refresh, which belongs to no request.
	gitRefreshLife = time.Hour
	// gitRequestLife bounds one git request through the proxy, and gitIdleLimit
	// ends one sooner once no byte has moved either way for that long. A clone
	// of a large repository on a slow line is alive while bytes move, however
	// long it takes; one that has stopped is dead whatever the clock says.
	gitRequestLife = 2 * time.Hour
	gitIdleLimit   = 3 * time.Minute
	// gitUpstreamHeaderWait is how long GitHub may take to start an answer. It
	// sends nothing before its headers, so a pack it is still computing is
	// silent until then (measured: 5.6 s, 2026-10-21); it stays below
	// gitIdleLimit so the transport says what happened before the watchdog does.
	gitUpstreamHeaderWait = 2 * time.Minute
	// gitUpstreamConnections bounds the node's connections to GitHub.
	gitUpstreamConnections = 2 * gitWork
	// gitSessionWork bounds one session's git requests at once, apart from its
	// content-addressed transfers, so neither can starve the other.
	gitSessionWork = 8
	// gitGrantMemory is how long an answer's sequence is kept after it expires:
	// longer than any request that could still be asking, so an older answer
	// finishing late always meets the newer one it must not overtake.
	gitGrantMemory = gitRequestLife + 5*time.Minute
	// gitWatchEvery is how often a fetch in progress is measured against its
	// ceiling and the filesystem's fill.
	gitWatchEvery = 500 * time.Millisecond
)

// errMirrorTooLarge says a mirror passed its tier's ceiling and is removed.
var errMirrorTooLarge = errors.New("the repository is larger than the tier's git cache")

// errRefreshPending says the mirror was still being brought to GitHub's refs
// when the advertisement could wait no longer.
var errRefreshPending = errors.New("the mirror is still being refreshed")

// errGitIdle ends a git request on which no byte has moved for gitIdleLimit.
var errGitIdle = errors.New("no byte moved for the git idle limit")

var gitSegment = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)

// gitObjectName is a full object name, the only want or shallow the proxy
// judges: anything else would be read by cat-file as a revision expression.
var gitObjectName = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

// uploadPackConfig is how the mirror is served, advertised and fetched alike:
// any object it holds may be wanted, as GitHub allows (ADR-018), and filters
// are served, so a partial clone's lazy fetch is answered from the mirror.
var uploadPackConfig = []string{"-c", "uploadpack.allowAnySHA1InWant=true",
	"-c", "uploadpack.allowFilter=true"}

// gitFetchConfig is how a mirror is fetched: no automatic gc or maintenance,
// which git would detach with the job's header in its environment, and a
// commit-graph kept, which pack-objects reads.
var gitFetchConfig = []string{"-c", "gc.auto=0", "-c", "maintenance.auto=false",
	"-c", "fetch.writeCommitGraph=true"}

// gitProxy is the node's mirrors and what it remembers about them.
type gitProxy struct {
	upstream string
	root     string
	binary   string
	client   *http.Client
	// fullFraction is gitFullFraction, refreshWait gitRefreshWait and idleLimit
	// gitIdleLimit: fields so a test is decided by neither the fill of the disk
	// it runs on nor the clock.
	fullFraction float64
	refreshWait  time.Duration
	idleLimit    time.Duration

	fetches chan struct{}
	work    chan struct{}
	reaping atomic.Bool
	// background is every refresh still running, which belongs to no request
	// and ends when stopping is closed.
	background sync.WaitGroup
	stopping   chan struct{}
	stopOnce   sync.Once

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

// gitAuthorisation is GitHub's yes to one header for one repository, or with a
// zero until, its no.
type gitAuthorisation struct {
	until    time.Time
	sequence uint64
	at       time.Time
}

type gitMirror struct {
	// mu guards refreshing, the one refresh in flight; use is held for reading
	// while a mirror is served or refreshed and for writing while it is removed.
	mu         sync.Mutex
	refreshing *gitRefresh
	use        sync.RWMutex
}

// gitRefresh is one fetch of a mirror to GitHub's refs. err is set before done
// is closed.
type gitRefresh struct {
	done chan struct{}
	err  error
}

func newGitProxy(root string) *gitProxy {
	return &gitProxy{
		upstream: "https://github.com", root: root, binary: "git", stopping: make(chan struct{}),
		client: &http.Client{
			Transport: newGitTransport(),
			Timeout:   2 * time.Minute,
			// A redirect is the client's to follow, never the node's: following
			// it would carry the job's header wherever upstream pointed.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		fullFraction: gitFullFraction,
		refreshWait:  gitRefreshWait,
		idleLimit:    gitIdleLimit,
		fetches:      make(chan struct{}, gitFetches),
		work:         make(chan struct{}, gitWork),
		mirrors:      make(map[string]*gitMirror), authorised: make(map[string]gitAuthorisation),
		tooLarge: make(map[string]time.Time),
		renamed:  make(map[string]gitRename),
	}
}

// close ends every refresh still running and waits for it.
func (g *gitProxy) close() {
	g.stopOnce.Do(func() { close(g.stopping) })
	g.background.Wait()
}

// newGitTransport is the proxy's own connections to GitHub, HTTP/1.1 only.
//
// NEVER ONE CONNECTION FOR EVERY JOB: over HTTP/2 every advertisement and every
// forwarded fetch on the node shares one connection, so one large download
// holds back the rest, and a request body starved of flow-control window stops
// arriving, which GitHub ends with 408 after ten idle seconds (measured,
// 2026-10-21).
func newGitTransport() *http.Transport {
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)

	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		Protocols:             protocols,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: gitUpstreamHeaderWait,
		ExpectContinueTimeout: time.Second,
		MaxConnsPerHost:       gitUpstreamConnections,
		MaxIdleConnsPerHost:   gitWork,
		IdleConnTimeout:       90 * time.Second,
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

func (g gitRequest) name() string { return g.owner + "/" + g.repo }

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

// gitExchange is one request's answer as it goes out: where it was answered
// and why, its status and its bytes, for the line the request ends with; and
// the moment a byte last moved either way, which the idle watchdog reads.
type gitExchange struct {
	http.ResponseWriter
	moved   *atomic.Int64
	route   string
	reason  string
	status  int
	written int64
	// upstreamErr and clientErr are how an answer already started was cut:
	// GitHub's side stopped, or the client's did.
	upstreamErr error
	clientErr   error
}

// gitUpstreamBody is GitHub's answer as it is relayed, keeping what ended it
// early.
type gitUpstreamBody struct {
	io.ReadCloser
	exchange *gitExchange
}

func (b gitUpstreamBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil && !errors.Is(err, io.EOF) && b.exchange.upstreamErr == nil {
		b.exchange.upstreamErr = err
	}

	return n, err
}

func (e *gitExchange) touch() { e.moved.Store(time.Now().UnixNano()) }

func (e *gitExchange) WriteHeader(status int) {
	if e.status == 0 {
		e.status = status
	}
	e.ResponseWriter.WriteHeader(status)
}

// Write sends p and flushes it, so upload-pack's and GitHub's keepalives reach
// the client while a pack is still being counted, rather than waiting in a
// buffer until the client gives up.
func (e *gitExchange) Write(p []byte) (int, error) {
	if e.status == 0 {
		e.status = http.StatusOK
	}
	n, err := e.ResponseWriter.Write(p)
	e.written += int64(n)
	if n > 0 {
		e.touch()
	}
	if err == nil {
		if flushErr := http.NewResponseController(e.ResponseWriter).Flush(); flushErr != nil &&
			!errors.Is(flushErr, http.ErrNotSupported) {
			err = flushErr
		}
	}
	if err != nil && e.clientErr == nil {
		e.clientErr = err
	}

	return n, err
}

// Unwrap lets http.ResponseController reach the connection's own deadlines
// and flush.
func (e *gitExchange) Unwrap() http.ResponseWriter { return e.ResponseWriter }

// answered records where the request was answered and why.
func (e *gitExchange) answered(route, reason string) { e.route, e.reason = route, reason }

// gitBody is a request body that marks progress as it is read.
type gitBody struct {
	io.ReadCloser
	exchange *gitExchange
}

func (b gitBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.exchange.touch()
	}

	return n, err
}

// serveGit answers one request under gitPathPrefix.
func (s *CacheService) serveGit(ctx context.Context, w http.ResponseWriter, r *http.Request) {
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
	ctx, cancel, err := extendTransferFor(ctx, w, gitRequestLife)
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)

		return
	}
	defer cancel()
	exchange := &gitExchange{ResponseWriter: w, moved: new(atomic.Int64)}
	exchange.touch()
	ctx, stop := s.git.watchIdle(ctx, exchange)
	defer stop()
	started := s.now()
	defer func() { s.logGit(ctx, request, r, exchange, started) }()
	// THE TRANSFER'S DEADLINE GOES WITH EVERY BRANCH, a forward included.
	r = r.WithContext(ctx)
	if r.Body != nil && r.Body != http.NoBody {
		r.Body = gitBody{ReadCloser: r.Body, exchange: exchange}
	}

	// ADMITTED LIKE ANY OTHER TRANSFER, per session and per node, because each
	// request is a subprocess or an upstream connection a guest could multiply.
	release, err := admitGit(ctx, request.session.gitAdmit, s.git.work)
	if err != nil {
		exchange.answered("refused", "the git cache is busy")
		http.Error(exchange, "the git cache is busy", http.StatusServiceUnavailable)

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
	case !s.gitAllowed(ctx, request.session):
		s.forwardGit(exchange, r, request, "the git cache is off for this tier")
	case !s.mirrorsOwner(request):
		s.forwardGit(exchange, r, request, "the repository is outside the tier's owner")
	case r.Method == http.MethodGet && request.rest == "info/refs" &&
		r.URL.Query().Get("service") == "git-upload-pack":
		s.gitAdvertise(ctx, exchange, r, request)
	case r.Method == http.MethodPost && request.rest == "git-upload-pack":
		s.gitUploadPack(ctx, exchange, r, request)
	default:
		s.forwardGit(exchange, r, request, "not a fetch")
	}
}

// watchIdle is ctx ended once no byte has moved through exchange for the idle
// limit; stop ends the watch.
func (g *gitProxy) watchIdle(ctx context.Context, exchange *gitExchange) (context.Context, func()) {
	ctx, cancel := context.WithCancelCause(ctx)
	limit := g.idleLimit
	ticker := time.NewTicker(max(limit/8, time.Millisecond))
	done := make(chan struct{})
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			if time.Since(time.Unix(0, exchange.moved.Load())) > limit {
				cancel(errGitIdle)

				return
			}
		}
	}()

	return ctx, func() {
		close(done)
		cancel(nil)
	}
}

// logGit is the line every authenticated git request ends with: where it was
// answered and why, what it answered and how long it took.
func (s *CacheService) logGit(ctx context.Context, request gitRequest, r *http.Request, exchange *gitExchange, started time.Time) {
	attrs := []any{"repository", request.name(), "request", r.Method + " " + request.rest,
		"route", exchange.route, "status", exchange.status, "bytes", exchange.written,
		"took", s.now().Sub(started).Round(time.Millisecond)}
	if exchange.reason != "" {
		attrs = append(attrs, "reason", exchange.reason)
	}
	if exchange.upstreamErr != nil {
		attrs = append(attrs, "github_cut", exchange.upstreamErr.Error())
	}
	if exchange.clientErr != nil {
		attrs = append(attrs, "client_cut", exchange.clientErr.Error())
	}
	if cause := context.Cause(ctx); cause != nil && !errors.Is(cause, context.Canceled) {
		attrs = append(attrs, "ended", cause.Error())
	}
	s.log.Info("a git request", attrs...)
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

// bufferGitBody reads a request's body whole before GitHub is asked anything,
// so GitHub never waits on the guest: it ends a request whose body stops for
// ten seconds with 408 (measured, 2026-10-21). A body past gitRequestLimit goes
// on as it arrives.
func bufferGitBody(r *http.Request) error {
	if r.Body == nil || r.Body == http.NoBody {
		return nil
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, gitRequestLimit+1))
	if err != nil {
		return err
	}
	if len(raw) > gitRequestLimit {
		r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(raw), r.Body))

		return nil
	}
	setGitBody(r, raw)

	return nil
}

// setGitBody makes raw the request's whole body, sent with its length.
func setGitBody(r *http.Request, raw []byte) {
	r.Body, r.ContentLength, r.TransferEncoding = io.NopCloser(bytes.NewReader(raw)), int64(len(raw)), nil
	r.Header.Del("Transfer-Encoding")
}

// forwardGit sends the request to GitHub as the job would have, with the job's
// own header and nothing of billet's, and says why in the request's line.
func (s *CacheService) forwardGit(exchange *gitExchange, r *http.Request, request gitRequest, reason string) {
	exchange.answered("github", reason)
	target, err := url.Parse(s.git.upstream)
	if err != nil {
		http.Error(exchange, "the git upstream is misconfigured", http.StatusBadGateway)

		return
	}
	if err := bufferGitBody(r); err != nil {
		http.Error(exchange, "an unreadable request", http.StatusBadRequest)

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
		// STREAMED AS IT ARRIVES, every keepalive included.
		FlushInterval: -1,
		// A FORWARD CUT AFTER ITS HEADERS can only end the connection, so what
		// cut it is kept for the request's line.
		ModifyResponse: func(resp *http.Response) error {
			resp.Body = gitUpstreamBody{ReadCloser: resp.Body, exchange: exchange}

			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			exchange.answered("github", reason+"; github.com did not answer: "+err.Error())
			http.Error(w, "github.com is unreachable", http.StatusBadGateway)
		},
	}
	proxy.ServeHTTP(exchange, r)
}

// gitAnswer is what GitHub answered an advertisement, read whole, so nothing of
// the response outlives the request that fetched it.
type gitAnswer struct {
	StatusCode int
	Header     http.Header
	Body       []byte
}

// upstreamRefs asks GitHub for the repository's v0 advertisement with the job's
// header. A status other than 200 is GitHub's answer to this job, relayed.
func (s *CacheService) upstreamRefs(ctx context.Context, request gitRequest) (*gitAnswer, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		s.git.upstream+"/"+request.owner+"/"+request.repo+".git/info/refs?service=git-upload-pack", http.NoBody)
	if err != nil {
		return nil, err
	}
	if request.auth != "" {
		req.Header.Set("Authorization", string(request.auth))
	}
	req.Header.Set("User-Agent", "git/2 billet")
	resp, err := s.git.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, gitAdvertisementLimit+1))
	if err != nil {
		return nil, err
	}
	if len(body) > gitAdvertisementLimit {
		return nil, errors.New("the advertisement is larger than the proxy reads")
	}

	return &gitAnswer{StatusCode: resp.StatusCode, Header: resp.Header, Body: body}, nil
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
	request gitRequest, resp *gitAnswer, now time.Time, sequence uint64,
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
func (g *gitProxy) renameTarget(request gitRequest, resp *gitAnswer) (gitRequest, bool) {
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
func (g *gitProxy) remember(request gitRequest, now time.Time, sequence uint64, granted bool) {
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
		held.until = now.Add(gitAuthorisationAge)
	}
	g.authorised[key] = held
}

// granted reports whether GitHub authorised this header for this repository
// within gitAuthorisationAge, and has not refused it since.
func (g *gitProxy) granted(request gitRequest, now time.Time) bool {
	g.mu.Lock()
	defer g.mu.Unlock()

	return now.Before(g.authorised[g.authorisationKey(request)].until)
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
// GitHub advertised, and answers with the mirror's own advertisement. A mirror
// that cannot be brought there in time sends GitHub's answer instead; the fetch
// after it is still served from the mirror when the mirror holds what it wants.
func (s *CacheService) gitAdvertise(ctx context.Context, exchange *gitExchange, r *http.Request, request gitRequest) {
	sequence := s.git.ask()
	resp, err := s.upstreamRefs(ctx, request)
	// ONE HOP, followed here for the reason serveGit follows a known one. Could
	// not tell withdraws an earlier rename as any other answer does.
	if moved, ok := s.git.learnRename(request, resp, s.now(), sequence); ok && err == nil {
		// A RENAME OUT OF THE SCOPE'S OWNER IS NOT MIRRORED, as a request for
		// that repository by its own name would not be: GitHub's redirect goes
		// back to the client, whose fetch then follows it to GitHub.
		if !s.mirrorsOwner(moved) {
			s.git.remember(moved, s.now(), sequence, false)
			exchange.answered("github", "renamed outside the tier's owner")
			s.relayGit(exchange, r, resp)

			return
		}
		// THE FOLLOWED NAME'S ANSWER IS A QUESTION OF ITS OWN, numbered when it
		// is asked and recorded against that name's rename as well as its grant,
		// so a refusal of it outranks whatever was said about it before. A second
		// redirect is recorded and relayed, never followed.
		request, sequence = moved, s.git.ask()
		resp, err = s.upstreamRefs(ctx, request)
		s.git.learnRename(request, resp, s.now(), sequence)
	}
	if err != nil {
		// COULD NOT TELL ENDS WHATEVER WAS GRANTED BEFORE, as a refusal does.
		s.git.remember(request, s.now(), sequence, false)
		s.forwardGit(exchange, r, request, "could not ask github.com: "+err.Error())

		return
	}
	if resp.StatusCode != http.StatusOK {
		s.git.remember(request, s.now(), sequence, false)
		exchange.answered("github", "github.com did not authorise the request")
		s.relayGit(exchange, r, resp)

		return
	}
	s.git.remember(request, s.now(), sequence, true)

	s.gitBegin(request.session)
	advertisement, existed, err := s.refreshMirror(ctx, request, resp.Body)
	s.noteGit(request.session, err, existed)
	if err != nil {
		exchange.answered("github", "the mirror could not answer: "+err.Error())
		s.relayGit(exchange, r, resp)

		return
	}
	exchange.answered("mirror", "")
	exchange.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
	exchange.Header().Set("Cache-Control", "no-cache")
	s.writeGit(exchange, slices.Concat(pktLine("# service=git-upload-pack\n"), []byte("0000"), advertisement))
}

// writeGit sends an answer's body. One that cannot be written is a client that
// went away, with nothing left to tell it.
func (s *CacheService) writeGit(w http.ResponseWriter, body []byte) {
	if _, err := w.Write(body); err != nil {
		s.log.Debug("a git client went away before its answer was written", "error", err)
	}
}

// relayGit answers with what GitHub answered. A redirect keeps its
// destination, and one within github.com is sent back through the proxy, so the
// client's next request carries its credentials here rather than going to
// GitHub without the header the rewrite dropped.
func (s *CacheService) relayGit(w http.ResponseWriter, r *http.Request, resp *gitAnswer) {
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
	s.writeGit(w, resp.Body)
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
func (s *CacheService) noteGit(session *cacheSession, err error, existed bool) {
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

// refreshMirror brings the repository's mirror to the refs upstream advertised
// and returns the mirror's advertisement, and whether the mirror existed before
// this request.
//
// IT WAITS AT MOST refreshWait, and never runs the fetch itself: one refresh at
// a time per mirror, owned by no request, so a client that gives up does not
// end the fetch every later job needs, and no job waits behind another's fetch
// for longer than its own bound. A refresh this request joined that began
// before GitHub answered it may leave refs GitHub has since moved, so the refs
// are compared again after it and another is started while time remains.
func (s *CacheService) refreshMirror(
	ctx context.Context, request gitRequest, upstream []byte,
) ([]byte, bool, error) {
	key := request.key()
	s.git.mu.Lock()
	until := s.git.tooLarge[key]
	s.git.mu.Unlock()
	if s.now().Before(until) {
		return nil, false, errMirrorTooLarge
	}
	refs, head, err := advertisedRefs(upstream)
	if err != nil {
		return nil, false, err
	}
	if len(refs) == 0 {
		return nil, false, errors.New("upstream advertised no refs")
	}

	mirror := s.git.mirror(key)
	path := s.git.mirrorPath(key)
	wait := time.NewTimer(s.git.refreshWait)
	defer wait.Stop()
	existed := false
	// TWO REFRESHES AT MOST: one already running may have begun before GitHub
	// answered this request, the next began after it, so refs that still differ
	// then are refs a fetch does not bring, and waiting longer changes nothing.
	for refreshes := 0; ; refreshes++ {
		current, currentHead, err := s.heldRefs(ctx, mirror, path)
		if refreshes == 0 {
			existed = err == nil && len(current) > 0
		}
		if err == nil && sameRefs(current, refs) && (head == "" || currentHead == head) {
			break
		}
		if refreshes == 2 {
			return nil, existed, errors.New("the mirror's refs still differ from github.com's after a refresh")
		}
		refresh := s.startRefresh(ctx, request, mirror, head)
		select {
		case <-refresh.done:
			if refresh.err != nil {
				return nil, existed, refresh.err
			}
		case <-wait.C:
			return nil, existed, errRefreshPending
		case <-ctx.Done():
			return nil, existed, context.Cause(ctx)
		}
	}

	mirror.use.RLock()
	defer mirror.use.RUnlock()
	advertisement, err := s.runGit(ctx, path, nil, append(slices.Clone(uploadPackConfig),
		"upload-pack", "--stateless-rpc", "--advertise-refs", ".")...)

	return advertisement, existed, err
}

// heldRefs is the mirror's refs and what its HEAD names, read while the mirror
// is held; a mirror that does not exist has none.
func (s *CacheService) heldRefs(ctx context.Context, mirror *gitMirror, path string) (map[string]string, string, error) {
	mirror.use.RLock()
	defer mirror.use.RUnlock()
	refs, err := s.mirrorRefs(ctx, path)
	if err != nil || len(refs) == 0 {
		return refs, "", err
	}
	// A HEAD THAT NAMES NO BRANCH is one the refresh sets, not a mirror unread.
	head, headErr := s.runGit(ctx, path, nil, "symbolic-ref", "-q", "HEAD")
	if headErr != nil {
		head = nil
	}

	return refs, strings.TrimSpace(string(head)), nil
}

// startRefresh is the mirror's refresh in flight, or a new one started for
// request: a fetch to GitHub's refs with request's header, which no client's
// departure ends; only gitRefreshLife or the proxy's close does.
func (s *CacheService) startRefresh(ctx context.Context, request gitRequest, mirror *gitMirror, head string) *gitRefresh {
	mirror.mu.Lock()
	defer mirror.mu.Unlock()
	if mirror.refreshing != nil {
		return mirror.refreshing
	}
	refresh := &gitRefresh{done: make(chan struct{})}
	mirror.refreshing = refresh
	s.git.background.Add(1)
	go func() {
		defer s.git.background.Done()
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), gitRefreshLife)
		defer cancel()
		go func() {
			select {
			case <-s.git.stopping:
				cancel()
			case <-ctx.Done():
			}
		}()
		err := s.refresh(ctx, request, mirror, head)
		// CLEARED BEFORE IT IS ANNOUNCED, so a waiter that finds the refs still
		// short starts the next refresh rather than joining this finished one.
		mirror.mu.Lock()
		mirror.refreshing = nil
		mirror.mu.Unlock()
		refresh.err = err
		close(refresh.done)
	}()

	return refresh
}

// refresh fetches the mirror to GitHub's refs with request's header, and
// removes it, marked, once it passes the tier's ceiling.
func (s *CacheService) refresh(ctx context.Context, request gitRequest, mirror *gitMirror, head string) error {
	key := request.key()
	path := s.git.mirrorPath(key)
	started := s.now()
	mirror.use.RLock()
	err := s.refreshHeld(ctx, request, path, head)
	mirror.use.RUnlock()
	// REMOVED UNDER THE EXCLUSIVE LOCK, once this refresh's own hold is gone,
	// so no fetch being served loses its repository mid-pack.
	if errors.Is(err, errMirrorTooLarge) {
		s.git.mu.Lock()
		s.git.tooLarge[key] = s.now().Add(gitTooLargeFor)
		s.git.mu.Unlock()
		if lockWithin(ctx, &mirror.use, casReleaseWait) {
			if removeErr := os.RemoveAll(path); removeErr != nil {
				err = errors.Join(err, removeErr)
			}
			mirror.use.Unlock()
		}
	}
	took := s.now().Sub(started).Round(time.Millisecond)
	if err != nil {
		s.log.Info("could not refresh a git mirror; github.com serves what it lacks",
			"repository", request.name(), "took", took, "error", err)

		return err
	}
	s.log.Info("refreshed a git mirror", "repository", request.name(), "took", took)

	return nil
}

// refreshHeld is refresh's work, with the mirror held for reading.
func (s *CacheService) refreshHeld(ctx context.Context, request gitRequest, path, head string) error {
	if full, statErr := filledAbove(s.git.root, s.git.fullFraction); statErr == nil && full {
		s.wakeGitReaper(ctx)

		return errors.New("the git cache's filesystem is full")
	}
	// THE ONLY WRITER IS THIS REFRESH, one per mirror at a time, so a lock file
	// or a half-written pack found now was left by one that was killed, and
	// would refuse every fetch after it.
	if err := clearStaleGitWrites(path); err != nil {
		return err
	}
	err := s.fetchMirror(ctx, path, request, volumeCeiling(sessionSetting(request.session, config.CacheGit)))
	if err == nil && head != "" && strings.HasPrefix(head, "refs/") {
		_, err = s.runGit(ctx, path, nil, "symbolic-ref", "HEAD", head)
	}
	if err != nil {
		return err
	}
	s.markMirrorUsed(path)

	return nil
}

// clearStaleGitWrites removes what a killed git fetch leaves in a mirror: lock
// files, which make every later ref update fail, and temporary packs. It works
// within the mirror's root, so no symlink in it reaches outside. A mirror not
// made yet has nothing to clear.
func clearStaleGitWrites(path string) error {
	root, err := os.OpenRoot(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer root.Close()
	for _, name := range []string{"HEAD.lock", "config.lock", "packed-refs.lock", "shallow.lock"} {
		if err := root.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	var locks []string
	err = fs.WalkDir(root.FS(), "refs", func(file string, entry fs.DirEntry, err error) error {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if entry.Type().IsRegular() && strings.HasSuffix(file, ".lock") {
			locks = append(locks, file)
		}

		return nil
	})
	if err != nil {
		return err
	}
	for _, pattern := range []string{"objects/pack/tmp_*", "objects/incoming-*"} {
		matches, err := fs.Glob(root.FS(), pattern)
		if err != nil {
			return err
		}
		locks = append(locks, matches...)
	}
	for _, name := range locks {
		if err := root.RemoveAll(name); err != nil {
			return err
		}
	}

	return nil
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
			// A MIRROR WHOSE SIZE CANNOT BE READ IS STOPPED as one past its
			// ceiling is: the watchdog cannot vouch for it.
			if size, err := directorySize(path); err != nil || size > ceiling {
				stop(errors.Join(errMirrorTooLarge, err))

				return
			}
			if full, err := filledAbove(s.git.root, s.git.fullFraction); err == nil && full {
				stop(errors.New("the git cache's filesystem is full"))

				return
			}
		}
	}()
	_, err = s.runGit(watched, path, secret, append(slices.Clone(gitFetchConfig),
		"fetch", "--prune", "--quiet", "--no-write-fetch-head", "--no-show-forced-updates",
		s.git.upstream+"/"+request.owner+"/"+request.repo+".git", "+refs/*:refs/*")...)
	if cause := context.Cause(watched); cause != nil && !errors.Is(cause, context.Canceled) {
		return cause
	}
	if err != nil {
		return err
	}
	if size, err := directorySize(path); err != nil || size > ceiling {
		return errors.Join(errMirrorTooLarge, err)
	}

	return nil
}

// wakeGitReaper reaps unused mirrors now rather than at the next maintenance
// pass, once, when the filesystem is found full. The pass outlives the request
// that found it full, so it keeps the request's values and not its end.
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

// directorySize is the bytes of the regular files under path. A file that
// vanishes mid-walk is one git removed and no longer counts; anything else
// unreadable leaves the size unknown.
func directorySize(path string) (int64, error) {
	var size int64
	err := filepath.WalkDir(path, func(_ string, entry fs.DirEntry, err error) error {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		info, err := entry.Info()
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		size += info.Size()

		return nil
	})

	return size, err
}

// markMirrorUsed stamps a mirror as served now, which is what the reaper reads.
// A stamp that cannot be written costs only an earlier reap and a refetch.
func (s *CacheService) markMirrorUsed(path string) {
	now := s.now()
	if err := os.Chtimes(filepath.Join(path, gitUsedStamp), now, now); err != nil {
		s.log.Warn("could not mark a git mirror used; the reaper may take it sooner",
			"mirror", filepath.Base(path), "error", err)
	}
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
	//nolint:gosec // G204: the node's configured git, with billet's own arguments and owner/repo segments gitSegment admitted
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
// the repository within gitAuthorisationAge, when the mirror holds every object
// it names; anything else goes to GitHub.
func (s *CacheService) gitUploadPack(ctx context.Context, exchange *gitExchange, r *http.Request, request gitRequest) {
	if !s.git.granted(request, s.now()) {
		s.forwardGit(exchange, r, request, "github.com has not authorised this header within the minute")

		return
	}
	// THE MIRROR SPEAKS v0. A client asking for v2 got GitHub's advertisement
	// and talks to GitHub.
	if strings.Contains(r.Header.Get("Git-Protocol"), "version=2") {
		s.forwardGit(exchange, r, request, "protocol v2")

		return
	}
	key := request.key()
	mirror := s.git.mirror(key)
	mirror.use.RLock()
	defer mirror.use.RUnlock()
	path := s.git.mirrorPath(key)
	if _, err := os.Stat(filepath.Join(path, "HEAD")); err != nil {
		s.forwardGit(exchange, r, request, "no mirror yet")

		return
	}

	raw, err := io.ReadAll(io.LimitReader(r.Body, gitRequestLimit+1))
	if err != nil {
		exchange.answered("refused", "an unreadable request")
		http.Error(exchange, "an unreadable request", http.StatusBadRequest)

		return
	}
	if len(raw) > gitRequestLimit {
		r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(raw), r.Body))
		s.forwardGit(exchange, r, request, "a negotiation larger than the proxy reads")

		return
	}
	setGitBody(r, raw)
	negotiation := raw
	if r.Header.Get("Content-Encoding") == "gzip" {
		decompressed, err := gzip.NewReader(bytes.NewReader(raw))
		if err == nil {
			negotiation, err = io.ReadAll(io.LimitReader(decompressed, gitRequestLimit+1))
		}
		if err != nil || len(negotiation) > gitRequestLimit {
			s.forwardGit(exchange, r, request, "a negotiation the proxy cannot read")

			return
		}
	}

	objects, ok := negotiatedObjects(negotiation)
	if !ok {
		s.forwardGit(exchange, r, request, "a negotiation the proxy cannot read")

		return
	}
	if !s.mirrorHolds(ctx, path, objects) {
		s.forwardGit(exchange, r, request, "the mirror lacks an object the fetch names")

		return
	}
	if served, reason := s.serveUploadPack(ctx, exchange, path, negotiation); !served {
		s.forwardGit(exchange, r, request, reason)

		return
	}
	exchange.answered("mirror", "")
	s.markMirrorUsed(path)
}

// serveUploadPack answers a negotiation from the mirror's upload-pack, unless
// its first line is a refusal or it says nothing at all: nothing reaches the
// client before that line is read, so GitHub can still answer instead.
func (s *CacheService) serveUploadPack(
	ctx context.Context, exchange *gitExchange, path string, negotiation []byte,
) (bool, string) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	//nolint:gosec // G204: the node's configured git, with billet's own arguments
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
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return false, "could not run the mirror's upload-pack: " + err.Error()
	}
	if err := cmd.Start(); err != nil {
		return false, "could not run the mirror's upload-pack: " + err.Error()
	}
	first, err := readPktLine(stdout)
	if err != nil || bytes.HasPrefix(first[min(len(first), 4):], []byte("ERR ")) {
		cancel()
		waitErr := cmd.Wait()
		reason := "the mirror's upload-pack said nothing"
		if err == nil {
			reason = "the mirror refused: " + strings.TrimSpace(string(first[4:]))
		}
		s.log.Info("the git cache's upload-pack did not answer; github.com does",
			"reason", reason, "error", errors.Join(err, waitErr), "stderr", boundedOutput(stderr.Bytes()))

		return false, reason
	}
	exchange.Header().Set("Content-Type", "application/x-git-upload-pack-result")
	exchange.Header().Set("Cache-Control", "no-cache")
	_, copyErr := exchange.Write(first)
	if copyErr == nil {
		_, copyErr = io.Copy(exchange, stdout)
	}
	if copyErr != nil {
		cancel()
	}
	if err := cmd.Wait(); err != nil || copyErr != nil {
		s.log.Warn("a fetch from the git cache failed", "error", errors.Join(err, copyErr),
			"stderr", boundedOutput(stderr.Bytes()))
	}

	return true, ""
}

// readPktLine reads one pkt-line from r, its four length bytes included.
func readPktLine(r io.Reader) ([]byte, error) {
	header := make([]byte, 4)
	if _, err := io.ReadFull(r, header); err != nil {
		return nil, err
	}
	var size int
	if _, err := fmt.Sscanf(string(header), "%04x", &size); err != nil {
		return nil, fmt.Errorf("a malformed pkt-line: %w", err)
	}
	if size < 4 {
		return header, nil
	}
	line := make([]byte, size)
	copy(line, header)
	if _, err := io.ReadFull(r, line[4:]); err != nil {
		return nil, err
	}

	return line, nil
}

// negotiatedObjects reads the objects a v0 fetch negotiation names before its
// first flush, every `want` and every `shallow`, and whether each is a full
// object name the proxy can look up. Anything else, a v2 command among them,
// is not a negotiation the proxy judges.
func negotiatedObjects(negotiation []byte) ([]string, bool) {
	var objects []string
	for len(negotiation) >= 4 {
		var size int
		if _, err := fmt.Sscanf(string(negotiation[:4]), "%04x", &size); err != nil {
			return nil, false
		}
		if size == 0 {
			return objects, true
		}
		if size < 4 || size > len(negotiation) {
			return nil, false
		}
		line := strings.TrimSuffix(string(negotiation[4:size]), "\n")
		negotiation = negotiation[size:]
		for _, prefix := range []string{"want ", "shallow "} {
			if rest, ok := strings.CutPrefix(line, prefix); ok {
				object, _, _ := strings.Cut(rest, " ")
				if !gitObjectName.MatchString(object) {
					return nil, false
				}
				objects = append(objects, object)
			}
		}
	}

	return nil, false
}

// mirrorHolds reports whether the mirror has every object; one it cannot look
// up is one it does not have.
func (s *CacheService) mirrorHolds(ctx context.Context, path string, objects []string) bool {
	if len(objects) == 0 {
		return true
	}
	var input strings.Builder
	for _, object := range objects {
		input.WriteString(object + "\n")
	}
	//nolint:gosec // G204: the node's configured git; literal arguments, and the objects go on stdin
	cmd := exec.CommandContext(ctx, s.git.binary, "cat-file", "--batch-check=%(objectname)")
	killGroup(cmd)
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
	if len(lines) != len(objects) {
		return false
	}
	for i, line := range lines {
		if line != objects[i] {
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
