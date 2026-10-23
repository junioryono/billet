package node

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/durablefile"
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
	// gitFirstLineWait is how long the mirror's upload-pack may take to say its
	// first line before GitHub answers instead, well inside gitIdleLimit so the
	// request is still alive to be forwarded.
	gitFirstLineWait = time.Minute
	// gitSpoolLimit bounds a request body the proxy holds on disk, past
	// gitRequestLimit in memory, so a guest cannot fill the node's disk with one.
	gitSpoolLimit = 512 << 20
	// gitIdentityStamp names the file in a mirror that holds the id of the
	// GitHub repository it was fetched from.
	gitIdentityStamp = "billet-repository"
)

// errMirrorTooLarge says a mirror passed its tier's ceiling and is removed.
var errMirrorTooLarge = errors.New("the repository is larger than the tier's git cache")

// errRefreshPending says the mirror was still being brought to GitHub's refs
// when the advertisement could wait no longer.
var errRefreshPending = errors.New("the mirror is still being refreshed")

// errMirrorMaintaining says the mirror's refs were behind GitHub's while it was
// repacked or pruned, past what an advertisement waits.
var errMirrorMaintaining = errors.New("the mirror is being repacked")

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
// which git would detach with the job's header in its environment; a
// commit-graph kept, which pack-objects reads; and what a fetch brings kept as
// the pack it came in, never loose objects, so maintenance sees every fetch
// as the pack it rolls up.
var gitFetchConfig = []string{"-c", "gc.auto=0", "-c", "maintenance.auto=false",
	"-c", "fetch.writeCommitGraph=true", "-c", "fetch.unpackLimit=1"}

// gitProxy is the node's mirrors and what it remembers about them.
type gitProxy struct {
	upstream string
	// api is GitHub's REST API, asked which repository a name is.
	api    string
	root   string
	binary string
	client *http.Client
	// fullFraction is gitFullFraction, refreshWait gitRefreshWait, idleLimit
	// gitIdleLimit, firstLineWait gitFirstLineWait, requestLimit
	// gitRequestLimit, narrowMost gitNarrowMost, maxPacks gitMaxPacks and
	// fullRepackEvery gitFullRepackEvery: fields so a test is decided by
	// neither the fill of the disk it runs on nor the clock nor its memory,
	// and can make a refresh whole or a roll-up due.
	fullFraction    float64
	refreshWait     time.Duration
	idleLimit       time.Duration
	firstLineWait   time.Duration
	requestLimit    int64
	narrowMost      int
	maxPacks        int
	fullRepackEvery time.Duration

	fetches chan struct{}
	work    chan struct{}
	// maintaining is the node's one maintenance pass at a time: a full repack
	// holds the mirror's every object in memory (800 MB for a 476 MB mirror,
	// measured 2026-10-22), and a pass that finds it taken is skipped.
	maintaining chan struct{}
	// beforeRepackWait runs as an advertisement starts to wait for a repack to
	// free its mirror's refresh slot; a seam so a test can end the repack only
	// once a request waits for it. Nil in production.
	beforeRepackWait func()
	reaping          atomic.Bool
	// observer is what the proxy reports to (SetGitObserver).
	observer atomic.Pointer[GitObserver]
	// background is every refresh still running, which belongs to no request
	// and ends when stopping is closed. lifecycle orders a refresh's start
	// against close, so none starts once close has begun to wait.
	lifecycle  sync.Mutex
	closed     bool
	background sync.WaitGroup
	stopping   chan struct{}

	mu         sync.Mutex
	mirrors    map[string]*gitMirror
	authorised map[string]gitAuthorisation
	tooLarge   map[string]time.Time
	// held is the mirrors a failed serve could not mark on disk, by path.
	held map[string]bool
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
// zero until, its no. repository is the repository GitHub said the name is when
// it said yes, or zero when that could not be learned, which a mirror is never
// served for.
type gitAuthorisation struct {
	until      time.Time
	repository int64
	sequence   uint64
	at         time.Time
}

type gitMirror struct {
	// mu guards refreshing, the one refresh in flight, and fetchedAt, the last
	// question to GitHub asked when a refresh last finished; use is held for
	// reading while a mirror is served or refreshed and for writing while it is
	// removed.
	mu         sync.Mutex
	refreshing *gitRefresh
	fetchedAt  uint64
	use        sync.RWMutex
	// pruning sends every new request to GitHub while a prune waits for or
	// holds use alone.
	pruning atomic.Bool
}

// gitRefresh is one fetch of a mirror to GitHub's refs. err is set before done
// is closed.
type gitRefresh struct {
	done chan struct{}
	err  error
	// maintaining says the mirror is being repacked after the fetch, with the
	// refresh slot still held, set before done is closed; idle is closed once
	// the slot is free.
	maintaining bool
	idle        chan struct{}
}

func newGitProxy(root string) *gitProxy {
	return &gitProxy{
		upstream: "https://github.com", api: "https://api.github.com", root: root, binary: "git",
		stopping: make(chan struct{}),
		client: &http.Client{
			Transport: newGitTransport(),
			Timeout:   2 * time.Minute,
			// A redirect is the client's to follow, never the node's: following
			// it would carry the job's header wherever upstream pointed.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		fullFraction:  gitFullFraction,
		refreshWait:   gitRefreshWait,
		idleLimit:     gitIdleLimit,
		firstLineWait: gitFirstLineWait,
		requestLimit:  gitRequestLimit,
		narrowMost:    gitNarrowMost,
		maxPacks:      gitMaxPacks, fullRepackEvery: gitFullRepackEvery,
		maintaining: make(chan struct{}, 1),
		fetches:     make(chan struct{}, gitFetches),
		work:        make(chan struct{}, gitWork),
		mirrors:     make(map[string]*gitMirror), authorised: make(map[string]gitAuthorisation),
		tooLarge: make(map[string]time.Time),
		held:     make(map[string]bool),
		renamed:  make(map[string]gitRename),
	}
}

// close ends every refresh still running and waits for it.
func (g *gitProxy) close() {
	g.lifecycle.Lock()
	if !g.closed {
		g.closed = true
		close(g.stopping)
	}
	g.lifecycle.Unlock()
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
	reason  gitReason
	detail  string
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

// WriteHeader sends status, and records the first final one: an interim 1xx a
// forward relays is not what the request answered.
func (e *gitExchange) WriteHeader(status int) {
	if e.status == 0 && status >= http.StatusOK {
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
func (e *gitExchange) answered(route string, reason gitReason, detail string) {
	e.route, e.reason, e.detail = route, reason, detail
}

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
	// EVERY REQUEST IS COUNTED ONCE, these three once answered, apart from the
	// line the rest end with; none of them reached a repository's grant.
	entered := s.now()
	refused := func(reason gitReason, status int) {
		s.git.observe().GitRequest(gitRouteRefused, reason.label, gitStatusClass(status), s.now().Sub(entered))
	}
	request, ok, authenticated := s.parseGitRequest(r)
	if !ok {
		http.NotFound(w, r)
		refused(gitNotAGitPath, http.StatusNotFound)

		return
	}
	// ASKED FOR CREDENTIALS, which makes git run the guest's helper: the first
	// request of every fetch carries none, because the rewrite dropped them.
	if !authenticated {
		w.Header().Set("WWW-Authenticate", `Basic realm="billet"`)
		http.Error(w, "credentials required", http.StatusUnauthorized)
		refused(gitCredentialsRequired, http.StatusUnauthorized)

		return
	}
	ctx, cancel, err := extendTransferFor(ctx, w, gitRequestLife)
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		refused(gitTransferRefused, http.StatusServiceUnavailable)

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
		exchange.answered(gitRouteRefused, gitBusy, "")
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
		s.forwardGit(exchange, r, request, gitCacheOff, "")
	case !s.mirrorsOwner(request):
		s.forwardGit(exchange, r, request, gitOutsideOwner, "")
	case r.Method == http.MethodGet && request.rest == "info/refs" &&
		r.URL.Query().Get("service") == "git-upload-pack":
		s.gitAdvertise(ctx, exchange, r, request)
	case r.Method == http.MethodPost && request.rest == "git-upload-pack":
		s.gitUploadPack(ctx, exchange, r, request)
	default:
		s.forwardGit(exchange, r, request, gitNotAFetch, "")
	}
}

// watchIdle is ctx ended once no byte has moved through exchange for the idle
// limit; stop ends the watch and returns once it has.
//
// THE CONNECTION'S DEADLINES END WITH IT, not only the context: a read of the
// guest's body or a write to it blocked on a guest that stopped is not woken by
// a context, and would hold the request's slots until the deadline
// extendTransferFor set. The watch is joined before the handler returns, so it
// never moves the deadlines of a connection already serving another request.
func (g *gitProxy) watchIdle(ctx context.Context, exchange *gitExchange) (context.Context, func()) {
	ctx, cancel := context.WithCancelCause(ctx)
	limit := g.idleLimit
	ticker := time.NewTicker(max(limit/8, time.Millisecond))
	done := make(chan struct{})
	var watching sync.WaitGroup
	watching.Go(func() {
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
				controller := http.NewResponseController(exchange.ResponseWriter)
				now := time.Now()
				// A WRITER THAT CANNOT MOVE ITS DEADLINES (a test's recorder) is
				// ended by the context alone.
				_ = controller.SetReadDeadline(now)  //nolint:errcheck // best effort: the context is ended whatever this says
				_ = controller.SetWriteDeadline(now) //nolint:errcheck // best effort: the context is ended whatever this says

				return
			}
		}
	})

	return ctx, func() {
		close(done)
		watching.Wait()
		cancel(nil)
	}
}

// logGit is the line every authenticated git request ends with: where it was
// answered and why, what it answered and how long it took.
func (s *CacheService) logGit(ctx context.Context, request gitRequest, r *http.Request, exchange *gitExchange, started time.Time) {
	// A REQUEST THAT ENDED BEFORE ANY ROUTE WAS CHOSEN is counted as refused,
	// unanswered, so every request is counted once.
	if exchange.route == "" {
		exchange.answered(gitRouteRefused, gitUnanswered, "")
	}
	took := s.now().Sub(started)
	s.git.observe().GitRequest(exchange.route, exchange.reason.label, gitStatusClass(exchange.status), took)
	attrs := []any{"repository", request.name(), "request", r.Method + " " + request.rest,
		"route", exchange.route, "status", exchange.status, "bytes", exchange.written,
		"took", took.Round(time.Millisecond)}
	if reason := exchange.reason.text; reason != "" {
		if exchange.detail != "" {
			reason += ": " + exchange.detail
		}
		attrs = append(attrs, "reason", reason)
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

// errGitBodyTooLarge refuses a request body past gitSpoolLimit.
var errGitBodyTooLarge = errors.New("a request larger than the git cache carries")

// bufferGitBody reads a request's body whole before GitHub is asked anything,
// so GitHub never waits on the guest: it ends a request whose body stops for
// ten seconds with 408 (measured, 2026-10-21). A body past requestLimit is
// held on disk instead, up to gitSpoolLimit; the returned function removes it.
func (g *gitProxy) bufferGitBody(r *http.Request) (func(), error) {
	if r.Body == nil || r.Body == http.NoBody {
		return func() {}, nil
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, g.requestLimit+1))
	if err != nil {
		return func() {}, err
	}
	if int64(len(raw)) <= g.requestLimit {
		setGitBody(r, raw)

		return func() {}, nil
	}
	if err := os.MkdirAll(g.root, 0o700); err != nil {
		return func() {}, err
	}
	spool, err := os.CreateTemp(g.root, ".request-*")
	if err != nil {
		return func() {}, err
	}
	// UNLINKED AT ONCE and used through its descriptor alone, so not even a
	// node killed mid-request leaves it on the disk.
	if err := os.Remove(spool.Name()); err != nil {
		_ = spool.Close()

		return func() {}, err
	}
	remove := func() { _ = spool.Close() }
	size, err := io.Copy(spool, io.LimitReader(io.MultiReader(bytes.NewReader(raw), r.Body), gitSpoolLimit+1))
	if err == nil && size > gitSpoolLimit {
		err = errGitBodyTooLarge
	}
	if err == nil {
		_, err = spool.Seek(0, io.SeekStart)
	}
	if err != nil {
		remove()

		return func() {}, err
	}
	r.Body, r.ContentLength, r.TransferEncoding = io.NopCloser(spool), size, nil
	r.Header.Del("Transfer-Encoding")

	return remove, nil
}

// setGitBody makes raw the request's whole body, sent with its length.
func setGitBody(r *http.Request, raw []byte) {
	r.Body, r.ContentLength, r.TransferEncoding = io.NopCloser(bytes.NewReader(raw)), int64(len(raw)), nil
	r.Header.Del("Transfer-Encoding")
}

// forwardGit sends the request to GitHub as the job would have, with the job's
// own header and nothing of billet's, and says why in the request's line.
func (s *CacheService) forwardGit(exchange *gitExchange, r *http.Request, request gitRequest, reason gitReason, detail string) {
	exchange.answered(gitRouteGitHub, reason, detail)
	target, err := url.Parse(s.git.upstream)
	if err != nil {
		http.Error(exchange, "the git upstream is misconfigured", http.StatusBadGateway)

		return
	}
	remove, err := s.git.bufferGitBody(r)
	defer remove()
	switch {
	case errors.Is(err, errGitBodyTooLarge):
		exchange.answered(gitRouteRefused, gitRequestTooLarge, "")
		http.Error(exchange, err.Error(), http.StatusRequestEntityTooLarge)

		return
	case err != nil:
		exchange.answered(gitRouteRefused, gitUnreadableRequest, err.Error())
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
			why := reason.text
			if detail != "" {
				why += ": " + detail
			}
			exchange.answered(gitRouteGitHub, gitGitHubUnreachable, why+"; github.com did not answer: "+err.Error())
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

// asked is the number of the last question asked so far: every question asked
// after this call is numbered above it.
func (g *gitProxy) asked() uint64 {
	g.mu.Lock()
	defer g.mu.Unlock()

	return g.sequence
}

// remember records GitHub's answer to question sequence: a grant for
// gitAuthorisationAge for the repository GitHub said the name is (zero when
// that could not be learned), or, with granted false, the end of any earlier
// grant. An answer to an older question than the one recorded changes nothing,
// so a refusal is never overtaken by a grant asked for before it.
func (g *gitProxy) remember(request gitRequest, now time.Time, sequence uint64, granted bool, repository int64) {
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
		held.until, held.repository = now.Add(gitAuthorisationAge), repository
	}
	g.authorised[key] = held
}

// granted is the repository GitHub authorised this header for under this name
// within gitAuthorisationAge, and whether it did and has not refused it since.
// A grant whose repository could not be learned is zero, and serves nothing
// from a mirror.
func (g *gitProxy) granted(request gitRequest, now time.Time) (int64, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	held := g.authorised[g.authorisationKey(request)]
	if !now.Before(held.until) {
		return 0, false
	}

	return held.repository, true
}

// repositoryID asks GitHub's REST API, with the job's own header, which
// repository the name is now: an id GitHub never reuses, where a name can be
// given to another repository once its owner renames or deletes the first.
// The header actions/checkout writes for git is accepted there too (measured,
// 2026-10-21).
func (s *CacheService) repositoryID(ctx context.Context, request gitRequest) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		s.git.api+"/repos/"+request.owner+"/"+request.repo, http.NoBody)
	if err != nil {
		return 0, err
	}
	if request.auth != "" {
		req.Header.Set("Authorization", string(request.auth))
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "billet")
	resp, err := s.git.client.Do(req)
	if err != nil {
		s.git.observe().GitLookup(gitLookupFailed)

		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		s.git.observe().GitLookup(gitLookupOutcome(resp.StatusCode, resp.Header.Get("X-Ratelimit-Remaining")))

		return 0, fmt.Errorf("github.com's API answered %d", resp.StatusCode)
	}
	var repository struct {
		ID int64 `json:"id"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&repository); err != nil {
		s.git.observe().GitLookup(gitLookupFailed)

		return 0, fmt.Errorf("github.com's API answered no repository: %w", err)
	}
	if repository.ID <= 0 {
		s.git.observe().GitLookup(gitLookupFailed)

		return 0, errors.New("github.com's API answered no repository id")
	}
	s.git.observe().GitLookup(gitLookupOK)

	return repository.ID, nil
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
			s.git.remember(moved, s.now(), sequence, false, 0)
			exchange.answered(gitRouteGitHub, gitRenamedOutsideOwner, "")
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
		s.git.remember(request, s.now(), sequence, false, 0)
		s.forwardGit(exchange, r, request, gitGitHubUnreachable, err.Error())

		return
	}
	if resp.StatusCode != http.StatusOK {
		s.git.remember(request, s.now(), sequence, false, 0)
		switch {
		case resp.StatusCode >= 300 && resp.StatusCode < 400:
			exchange.answered(gitRouteGitHub, gitRedirected, "")
		case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden,
			resp.StatusCode == http.StatusNotFound:
			exchange.answered(gitRouteGitHub, gitNotAuthorised, "")
		default:
			exchange.answered(gitRouteGitHub, gitGitHubFailed, strconv.Itoa(resp.StatusCode))
		}
		s.relayGit(exchange, r, resp)

		return
	}
	// WHICH REPOSITORY THE NAME IS, asked with every advertisement and never
	// carried from an earlier grant: a name can pass to another repository while
	// every advertisement answers 200, a mirror is served only to the repository
	// it was fetched from, and a name GitHub could not tie to one is GitHub's to
	// answer.
	repository, err := s.repositoryID(ctx, request)
	if err != nil {
		s.git.remember(request, s.now(), sequence, true, 0)
		exchange.answered(gitRouteGitHub, gitNoRepositoryID, err.Error())
		s.relayGit(exchange, r, resp)

		return
	}
	s.git.remember(request, s.now(), sequence, true, repository)

	s.gitBegin(request.session)
	advertisement, existed, err := s.refreshMirror(ctx, request, repository, sequence, resp.Body)
	s.noteGit(request.session, err, existed)
	if err != nil {
		reason := gitMirrorUnavailable
		if errors.Is(err, errMirrorMaintaining) {
			reason = gitMirrorMaintaining
		}
		exchange.answered(gitRouteGitHub, reason, err.Error())
		s.relayGit(exchange, r, resp)

		return
	}
	// THE GRANT STANDS WHEN THE MIRROR ANSWERS, not only when it was asked: a
	// refusal GitHub gave this header meanwhile ends it.
	if granted, ok := s.git.granted(request, s.now()); !ok || granted != repository {
		exchange.answered(gitRouteGitHub, gitGrantEnded, "while the mirror was refreshed")
		s.relayGit(exchange, r, resp)

		return
	}
	exchange.answered(gitRouteMirror, gitServed, "")
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
	ctx context.Context, request gitRequest, repository int64, sequence uint64, upstream []byte,
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
	if mirror.pruning.Load() {
		return nil, false, errMirrorMaintaining
	}
	// ONE DEADLINE FOR ALL OF IT: the lock waits, reading the mirror's refs, the
	// refresh and the advertisement, so no part of a slow disk or a removal in
	// progress holds the job past refreshWait.
	ctx, cancel := context.WithTimeoutCause(ctx, s.git.refreshWait, errRefreshPending)
	defer cancel()
	if err := s.claimMirror(ctx, request, mirror, path, repository); err != nil {
		return nil, false, err
	}
	existed := false
	var last *gitRefresh
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
		// A REPACK HOLDS THE SLOT the next refresh needs, and is waited for
		// within the same deadline as a refresh is.
		if last != nil && last.maintaining {
			if s.git.beforeRepackWait != nil {
				s.git.beforeRepackWait()
			}
			select {
			case <-last.idle:
			case <-ctx.Done():
				return nil, existed, errMirrorMaintaining
			}
		}
		if refreshes == 2 {
			return nil, existed, errors.New("the mirror's refs still differ from github.com's after a refresh")
		}
		refresh := s.startRefresh(ctx, request, repository, mirror, head, refs, sequence)
		last = refresh
		select {
		case <-refresh.done:
			if refresh.err != nil {
				return nil, existed, refresh.err
			}
		case <-ctx.Done():
			return nil, existed, context.Cause(ctx)
		}
	}

	if !rlockWithin(ctx, &mirror.use) {
		return nil, existed, context.Cause(ctx)
	}
	defer mirror.use.RUnlock()
	// THE STAMP AGAIN, under the hold the advertisement is read in: the mirror
	// answered for is the repository GitHub said the name is.
	if stamped, _, err := mirrorRepository(path); err != nil || stamped != repository {
		return nil, existed, errors.New("the mirror is not the repository github.com named")
	}
	advertisement, err := s.runGit(ctx, path, nil, append(slices.Clone(uploadPackConfig),
		"upload-pack", "--stateless-rpc", "--advertise-refs", ".")...)
	if err != nil && ctx.Err() != nil {
		err = context.Cause(ctx)
	}
	// AND ONCE MORE AFTER IT: a refresh shares this hold, and one that began
	// while the refs were read leaves the mirror marked, so refs it fetched are
	// never advertised from a mirror GitHub has not vouched for.
	if stamped, _, stampErr := mirrorRepository(path); err == nil &&
		(stampErr != nil || stamped != repository || s.git.quarantined(path)) {
		err = errors.Join(errors.New("the mirror was marked while it was advertised"), stampErr)
	}

	return advertisement, existed, err
}

// claimMirror makes the mirror at path the repository GitHub said the name is.
// A mirror stamped with another repository, or with none it can read, was
// fetched from a repository the name no longer is (one renamed or deleted, and
// another given its name) and holds objects this one's jobs may not have, so
// it is removed before anything of it is served. So is one marked unverified
// by a fetch that did not end in a matching id, or by a serve that failed
// mid-answer, unless that fetch is still running, which this request joins.
// One in use is left, and GitHub answers until it is free.
func (s *CacheService) claimMirror(
	ctx context.Context, request gitRequest, mirror *gitMirror, path string, repository int64,
) error {
	keep := func(stamped int64, exists bool, err error) bool {
		switch {
		case !exists:
			return true
		case s.git.quarantined(path):
			return false
		case errors.Is(err, errMirrorUnverified):
			mirror.mu.Lock()
			refreshing := mirror.refreshing != nil
			mirror.mu.Unlock()

			return refreshing && stamped == repository
		default:
			return err == nil && stamped == repository
		}
	}
	if !rlockWithin(ctx, &mirror.use) {
		return context.Cause(ctx)
	}
	stamped, exists, err := mirrorRepository(path)
	mirror.use.RUnlock()
	if keep(stamped, exists, err) {
		return nil
	}
	if !lockWithin(ctx, &mirror.use, s.git.refreshWait) {
		return errors.New("the mirror must be replaced and is in use")
	}
	defer mirror.use.Unlock()
	stamped, exists, err = mirrorRepository(path)
	if keep(stamped, exists, err) {
		return nil
	}
	s.log.Info("removed a git mirror it could not serve", "repository", request.name(),
		"stamped", stamped, "repository_id", repository, "error", err)
	// RENAMED ASIDE AT ONCE, DELETED AFTER: deleting a large mirror takes as
	// long as its files, which no advertisement waits for.
	discarded, err := discardGitMirror(s.git.root, path)
	if err != nil {
		return err
	}
	s.git.release(path)
	s.git.goBackground(func() {
		if err := os.RemoveAll(discarded); err != nil {
			s.log.Warn("could not delete a discarded git mirror; the reaper will", "error", err)
		}
	})

	return nil
}

// gitDiscardDirectory holds mirrors renamed aside to be deleted, under the
// mirrors' root: no repository's mirror can be named so.
const gitDiscardDirectory = ".discarded"

// discardGitMirror renames the mirror at path aside under root, where no
// request finds it, and returns the directory to delete. It goes under a name
// no repository can have (gitSegment refuses a leading dot), and one whose
// deletion never finished is the reaper's.
func discardGitMirror(root, path string) (string, error) {
	discards := filepath.Join(root, gitDiscardDirectory)
	if err := os.MkdirAll(discards, 0o700); err != nil {
		return "", err
	}
	discarded, err := os.MkdirTemp(discards, "mirror-")
	if err != nil {
		return "", err
	}
	if err := os.Rename(path, filepath.Join(discarded, "mirror.git")); err != nil {
		return "", errors.Join(err, os.Remove(discarded))
	}

	return discarded, nil
}

// gitUnverifiedStamp marks a mirror whose objects may not all be its
// repository's: written before a fetch begins and removed only once GitHub
// names the same repository after it, and written by a serve that failed
// mid-answer. A mirror carrying it is never served, never fetched into, and
// replaced, on disk so a restart forgets nothing of it.
const gitUnverifiedStamp = "billet-unverified"

// gitBrokenStamp marks a mirror a serve failed in after its answer began. A
// refresh that verifies its fetch leaves it, since fetching new refs repairs
// nothing that broke a serve; only replacing the mirror clears it.
const gitBrokenStamp = "billet-broken"

// errMirrorUnverified says a mirror carries gitUnverifiedStamp, and
// errMirrorBroken gitBrokenStamp.
var (
	errMirrorUnverified = errors.New("the mirror's last fetch was not verified")
	errMirrorBroken     = errors.New("a serve from the mirror failed mid-answer")
)

// quarantine holds the mirror at path out of service in this process, for a
// failed serve whose mark could not be written: a disk that refused the mark
// must not leave the mirror trusted.
func (g *gitProxy) quarantine(path string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.held[path] = true
}

func (g *gitProxy) quarantined(path string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()

	return g.held[path]
}

// holdBroken holds a broken mirror the disk would not mark out of service in
// this process, and takes away its repository stamp, which needs no space, so
// a restart that forgets the quarantine still finds a mirror it cannot serve.
func (g *gitProxy) holdBroken(path string, markErr error) error {
	g.quarantine(path)
	err := fmt.Errorf("could not mark the mirror broken; held out of service: %w", markErr)
	root, openErr := os.OpenRoot(path)
	if openErr != nil {
		return errors.Join(err, openErr)
	}
	defer root.Close()
	if removeErr := root.Remove(gitIdentityStamp); removeErr != nil && !errors.Is(removeErr, fs.ErrNotExist) {
		return errors.Join(err, removeErr)
	}
	dir, openErr := root.Open(".")
	if openErr != nil {
		return errors.Join(err, openErr)
	}

	return errors.Join(err, dir.Sync(), dir.Close())
}

// killedBySIGKILL reports whether a process ended on SIGKILL, the signal
// killGroup sends when its request ends.
func killedBySIGKILL(exit *exec.ExitError) bool {
	status, ok := exit.Sys().(syscall.WaitStatus)

	return ok && status.Signaled() && status.Signal() == syscall.SIGKILL
}

// release ends a quarantine once its mirror is gone.
func (g *gitProxy) release(path string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.held, path)
}

// markUnverified writes gitUnverifiedStamp into the mirror at path, durably:
// the fetch it precedes must not begin until a crash would still find it.
func markUnverified(path string) error { return stampMirror(path, gitUnverifiedStamp) }

// markBroken writes gitBrokenStamp into the mirror at path, durably.
func markBroken(path string) error { return stampMirror(path, gitBrokenStamp) }

// stampMirror installs an empty file named name in the mirror at path, synced
// with its directory before it returns.
func stampMirror(path, name string) error {
	_, err := durablefile.Installer{}.Install(path, name, 0o600, func(io.Writer) error { return nil })

	return err
}

// markVerified removes gitUnverifiedStamp from the mirror at path.
func markVerified(path string) error {
	root, err := os.OpenRoot(path)
	if err != nil {
		return err
	}
	defer root.Close()
	if err := root.Remove(gitUnverifiedStamp); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}

	return nil
}

// goBackground runs work on the proxy's background, which close joins; once
// close has begun, the work is not started and its leftovers are the reaper's.
func (g *gitProxy) goBackground(work func()) {
	g.lifecycle.Lock()
	defer g.lifecycle.Unlock()
	if g.closed {
		return
	}
	g.background.Go(work)
}

// mirrorRepository is the repository a mirror at path was fetched from, and
// whether a mirror is there at all. A mirror whose stamp is missing or cannot
// be read is one billet cannot say is any repository's; one marked unverified
// answers its stamp with errMirrorUnverified.
func mirrorRepository(path string) (int64, bool, error) {
	root, err := os.OpenRoot(path)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, true, err
	}
	defer root.Close()
	raw, err := root.ReadFile(gitIdentityStamp)
	if err != nil {
		return 0, true, fmt.Errorf("the mirror's repository stamp: %w", err)
	}
	id, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
	if err != nil || id <= 0 {
		return 0, true, fmt.Errorf("the mirror's repository stamp %q is unreadable", boundedOutput(raw))
	}
	// COULD NOT TELL IS NOT ABSENT: only a mark proved missing leaves the
	// mirror servable.
	// THE REMOVABLE MARK FIRST: a refresh may take away billet-unverified
	// between the two reads, and a serve may add billet-broken, but none takes
	// billet-broken away, so reading it second never finds a mirror clean that
	// was marked throughout.
	if _, err := root.Stat(gitUnverifiedStamp); !errors.Is(err, fs.ErrNotExist) {
		return id, true, errors.Join(errMirrorUnverified, err)
	}
	if _, err := root.Stat(gitBrokenStamp); !errors.Is(err, fs.ErrNotExist) {
		return id, true, errors.Join(errMirrorBroken, err)
	}

	return id, true, nil
}

// rlockWithin takes l for reading, giving up when ctx ends: a writer removing
// the mirror holds it for as long as the removal takes.
func rlockWithin(ctx context.Context, l *sync.RWMutex) bool {
	for !l.TryRLock() {
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()

			return false
		case <-timer.C:
		}
	}

	return true
}

// heldRefs is the mirror's refs and what its HEAD names, read while the mirror
// is held; a mirror that does not exist has none.
func (s *CacheService) heldRefs(ctx context.Context, mirror *gitMirror, path string) (map[string]string, string, error) {
	if !rlockWithin(ctx, &mirror.use) {
		return nil, "", context.Cause(ctx)
	}
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

// errGitClosed refuses a refresh once the proxy is closing.
var errGitClosed = errors.New("the git cache is closing")

// startRefresh is the mirror's refresh in flight, or a new one started for
// request: a fetch to GitHub's refs of the repository GitHub said the name is,
// with request's header, which no client's departure ends; only gitRefreshLife
// or the proxy's close does. refs is GitHub's advertisement to request, which
// says what moved; nil fetches every ref.
func (s *CacheService) startRefresh(
	ctx context.Context, request gitRequest, repository int64, mirror *gitMirror, head string,
	refs map[string]string, sequence uint64,
) *gitRefresh {
	// NONE STARTS ONCE close HAS BEGUN TO WAIT, which would let it outlive the
	// state directory it writes.
	s.git.lifecycle.Lock()
	defer s.git.lifecycle.Unlock()
	if s.git.closed {
		s.git.observe().GitRefresh(gitRefreshClosed, 0)
		refresh := &gitRefresh{done: make(chan struct{}), idle: make(chan struct{}), err: errGitClosed}
		close(refresh.done)
		close(refresh.idle)

		return refresh
	}
	mirror.mu.Lock()
	defer mirror.mu.Unlock()
	if mirror.refreshing != nil {
		return mirror.refreshing
	}
	refresh := &gitRefresh{done: make(chan struct{}), idle: make(chan struct{})}
	mirror.refreshing = refresh
	s.git.background.Add(1)
	go func() {
		defer s.git.background.Done()
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), gitRefreshLife)
		defer cancel()
		// ENDED BY close WITH errGitClosed AS ITS CAUSE, so the refresh is
		// counted as closed rather than as whatever its cut fetch looked like.
		ctx, end := context.WithCancelCause(ctx)
		defer end(nil)
		go func() {
			select {
			case <-s.git.stopping:
				end(errGitClosed)
			case <-ctx.Done():
			}
		}()
		err := s.refresh(ctx, request, repository, mirror, head, refs, sequence)
		// MAINTENANCE FOLLOWS A REFRESH THAT WORKED, when one is due and the
		// node's maintenance slot is free; a busy slot skips it to a later
		// refresh.
		kind, release := "", func() {}
		if err == nil {
			kind = s.maintenanceDue(s.git.mirrorPath(request.key()))
		}
		if kind != "" {
			select {
			case s.git.maintaining <- struct{}{}:
				release = func() { <-s.git.maintaining }
			default:
				kind = ""
			}
		}
		// CLEARED BEFORE IT IS ANNOUNCED, so a waiter that finds the refs still
		// short starts the next refresh rather than joining this finished one;
		// unless the mirror is to be repacked, when the slot stays held, so no
		// fetch runs beside the repack, and such a waiter waits for idle.
		free := func() {
			mirror.mu.Lock()
			mirror.refreshing = nil
			mirror.mu.Unlock()
			close(refresh.idle)
		}
		refresh.maintaining = kind != ""
		refresh.err = err
		if kind == "" {
			free()
			close(refresh.done)

			return
		}
		close(refresh.done)
		s.maintain(ctx, request, repository, mirror, kind)
		release()
		free()
	}()

	return refresh
}

// refresh fetches the mirror to GitHub's refs with request's header, and
// removes it, marked, once it passes the tier's ceiling. sequence orders refs
// among GitHub's answers.
//
// AN ADVERTISEMENT ASKED FOR BEFORE THE MIRROR'S LAST REFRESH FINISHED is
// fetched whole, to GitHub's refs as they are now: that refresh may have
// brought refs GitHub made after the advertisement was answered, and read
// narrowly each would look dropped and be removed. Only an advertisement asked
// for after it says what the mirror lacks.
func (s *CacheService) refresh(
	ctx context.Context, request gitRequest, repository int64, mirror *gitMirror, head string,
	refs map[string]string, sequence uint64,
) error {
	key := request.key()
	path := s.git.mirrorPath(key)
	started := s.now()
	mirror.mu.Lock()
	if sequence <= mirror.fetchedAt {
		refs = nil
	}
	mirror.mu.Unlock()
	mirror.use.RLock()
	plan, err := s.refreshHeld(ctx, request, repository, path, head, refs)
	mirror.use.RUnlock()
	// RECORDED WHATEVER THE OUTCOME: a failed fetch may have moved refs too.
	mirror.mu.Lock()
	mirror.fetchedAt = s.git.asked()
	mirror.mu.Unlock()
	// REMOVED UNDER THE EXCLUSIVE LOCK, once this refresh's own hold is gone,
	// so no fetch being served loses its repository mid-pack.
	if errors.Is(err, errMirrorTooLarge) {
		s.git.mu.Lock()
		s.git.tooLarge[key] = s.now().Add(gitTooLargeFor)
		s.git.mu.Unlock()
		if lockWithin(ctx, &mirror.use, casReleaseWait) {
			if removeErr := os.RemoveAll(path); removeErr != nil {
				err = errors.Join(err, removeErr)
			} else {
				s.git.release(path)
			}
			mirror.use.Unlock()
		}
	}
	if err != nil && errors.Is(context.Cause(ctx), errGitClosed) {
		err = errors.Join(errGitClosed, err)
	}
	took := s.now().Sub(started)
	s.git.observe().GitRefresh(gitRefreshOutcome(err), took)
	took = took.Round(time.Millisecond)
	if err != nil {
		s.log.Info("could not refresh a git mirror; github.com serves what it lacks",
			"repository", request.name(), "took", took, "fetched", plan, "error", err)

		return err
	}
	s.log.Info("refreshed a git mirror", "repository", request.name(), "took", took, "fetched", plan)

	return nil
}

// refreshHeld is refresh's work, with the mirror held for reading, and what it
// fetched.
func (s *CacheService) refreshHeld(
	ctx context.Context, request gitRequest, repository int64, path, head string, refs map[string]string,
) (gitFetchPlan, error) {
	plan := gitFetchPlan{}
	if full, statErr := filledAbove(s.git.root, s.git.fullFraction); statErr == nil && full {
		s.wakeGitReaper(ctx)

		return plan, errGitDiskFull
	}
	// NEVER FETCHED INTO ANOTHER REPOSITORY'S MIRROR, nor one left unverified:
	// the claim removes either, and a refresh that finds one meanwhile leaves
	// it to be removed.
	if stamped, exists, err := mirrorRepository(path); exists && (err != nil || stamped != repository) {
		return plan, errors.Join(errors.New("the mirror is another repository's or unverified"), err)
	}
	// THE ONLY WRITER IS THIS REFRESH, one per mirror at a time, so a lock file
	// or a half-written pack found now was left by one that was killed, and
	// would refuse every fetch after it.
	if err := clearStaleGitWrites(path); err != nil {
		return plan, err
	}
	// THE MIRROR'S REFS ARE READ HERE, under the refresh that alone writes them,
	// so what moved is measured against what the mirror holds now. A mirror
	// whose refs cannot be read is fetched whole.
	if current, err := s.mirrorRefs(ctx, path); err == nil && refs != nil {
		plan = planGitFetch(current, refs, head, s.git.narrowMost)
	}
	plan, err := s.fetchMirror(ctx, path, request, repository,
		volumeCeiling(sessionSetting(request.session, config.CacheGit)), plan)
	if err == nil && head != "" && strings.HasPrefix(head, "refs/") {
		_, err = s.runGit(ctx, path, nil, "symbolic-ref", "HEAD", head)
	}
	if err != nil {
		return plan, err
	}
	// THE NAME WAS FETCHED, NOT THE ID: GitHub serves git by name alone
	// (`/repositories/<id>` is not found over git, 2026-10-21). The mirror
	// stays unverified, never served and replaced at the next claim, unless
	// GitHub names the same repository after the fetch: a name that passed to
	// another repository meanwhile brought that one's objects, and could not
	// tell is not the same.
	after, err := s.repositoryID(ctx, request)
	switch {
	case err != nil:
		return plan, fmt.Errorf("%w: could not verify which repository was fetched: %w", errFetchNotVerified, err)
	case after != repository:
		return plan, fmt.Errorf("%w: the name became repository %d while the mirror of %d was fetched",
			errFetchNotVerified, after, repository)
	}
	if err := markVerified(path); err != nil {
		return plan, err
	}
	s.markMirrorUsed(path)

	return plan, nil
}

// gitNarrowMost is the most refs a refresh fetches or removes by name; a mirror
// further from GitHub's refs than that is fetched whole.
const gitNarrowMost = 256

// gitFetchPlan is what one refresh fetches: every ref, pruning what GitHub no
// longer has; or, when narrow, only the refs GitHub's advertisement moved or
// added, negotiated from tips, and the refs it dropped removed by name.
type gitFetchPlan struct {
	narrow bool
	// moved is the refs to fetch, each by its own name.
	moved []string
	// tips is what the mirror held of the moved refs and of the default
	// branch, the objects git offers GitHub as what it has.
	tips []string
	// gone is the refs GitHub no longer has, with the object each named.
	gone map[string]string
}

// String is what the refresh's line says it fetched.
func (p gitFetchPlan) String() string {
	if !p.narrow {
		return "every ref"
	}

	return fmt.Sprintf("%d refs moved, %d gone", len(p.moved), len(p.gone))
}

// planGitFetch is the fetch that brings a mirror holding current to GitHub's
// refs, whose HEAD names head: only what moved, unless the mirror is empty, a
// name is not one billet puts on a command line, or more than most refs differ.
//
// THE DEFAULT BRANCH IS ALWAYS A TIP, with the old value of every moved ref: a
// new branch has no old value, and a rebased one shares no history with its
// old value past where it left the default branch, so without the default
// branch's tip git offers nothing GitHub can build on and the fetch brings the
// whole history again.
func planGitFetch(current, upstream map[string]string, head string, most int) gitFetchPlan {
	if len(current) == 0 {
		return gitFetchPlan{}
	}
	plan := gitFetchPlan{narrow: true, gone: make(map[string]string)}
	tips := make(map[string]bool)
	for name, object := range upstream {
		if current[name] == object {
			continue
		}
		if !narrowRefName(name) {
			return gitFetchPlan{}
		}
		plan.moved = append(plan.moved, name)
		if old, ok := current[name]; ok {
			tips[old] = true
		}
	}
	for name, object := range current {
		if _, ok := upstream[name]; !ok {
			plan.gone[name] = object
		}
	}
	if len(plan.moved)+len(plan.gone) > most {
		return gitFetchPlan{}
	}
	if old, ok := current[head]; ok {
		tips[old] = true
	}
	slices.Sort(plan.moved)
	plan.tips = slices.Sorted(maps.Keys(tips))

	return plan
}

// narrowRefName is whether name is a ref billet fetches by name: a refspec on
// git's command line, so nothing git would read as an option, a pattern, a
// revision expression or the refspec's own separator.
func narrowRefName(name string) bool {
	rest, ok := strings.CutPrefix(name, "refs/")
	if !ok || rest == "" || strings.HasSuffix(name, "/") || strings.HasSuffix(name, ".") ||
		strings.HasSuffix(name, ".lock") || strings.Contains(name, "..") || strings.Contains(name, "@{") ||
		strings.Contains(name, "//") {
		return false
	}
	for _, r := range name {
		if r <= ' ' || r == 0x7f || strings.ContainsRune(":~^?*[\\", r) {
			return false
		}
	}

	return true
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
	for _, pattern := range []string{"objects/pack/tmp_*", "objects/pack/.tmp-*", "objects/incoming-*",
		"objects/info/*.lock", "objects/info/commit-graphs/*.lock", "objects/info/commit-graphs/tmp_graph_*"} {
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
//
// A NARROW FETCH THAT FAILS IS DONE AGAIN WHOLE, which is what every refresh
// did before there was a narrow one: a ref GitHub dropped after it advertised
// it refuses a fetch that names it. The plan returned is what was fetched.
func (s *CacheService) fetchMirror(
	ctx context.Context, path string, request gitRequest, repository int64, ceiling int64, plan gitFetchPlan,
) (gitFetchPlan, error) {
	waitCtx, cancel := context.WithTimeout(ctx, gitFetchWait)
	release, err := admitGit(waitCtx, request.session.gitFetch, s.git.fetches)
	cancel()
	if err != nil {
		return plan, errFetchSlotsBusy
	}
	defer release()
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		plan = gitFetchPlan{}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return plan, err
		}
		if _, err := s.runGit(ctx, filepath.Dir(path), nil, "init", "--bare", "--quiet", path); err != nil {
			return plan, err
		}
		if err := os.WriteFile(filepath.Join(path, gitUsedStamp), nil, 0o600); err != nil {
			return plan, err
		}
		// STAMPED BEFORE ANY OBJECT ARRIVES, so a mirror is never without the
		// repository it is; one a crash left unstamped is removed by the next claim.
		if err := os.WriteFile(filepath.Join(path, gitIdentityStamp),
			[]byte(strconv.FormatInt(repository, 10)+"\n"), 0o600); err != nil {
			return plan, err
		}
	}
	// UNVERIFIED BEFORE A BYTE ARRIVES, verified by the caller only once GitHub
	// names the same repository after the fetch.
	if err := markUnverified(path); err != nil {
		return plan, err
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
	// READ ONCE, before the watchdog starts: the watchdog is not joined, so a
	// tick that began before done closed may still read after this returns.
	root, fraction := s.git.root, s.git.fullFraction
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
			if full, err := filledAbove(root, fraction); err == nil && full {
				stop(errGitDiskFull)

				return
			}
		}
	}()
	remote := s.git.upstream + "/" + request.owner + "/" + request.repo + ".git"
	err = s.fetchRefs(watched, path, secret, remote, plan)
	if err != nil && plan.narrow && context.Cause(watched) == nil {
		s.log.Info("a narrow git fetch failed; fetching every ref", "repository", request.name(), "error", err)
		plan = gitFetchPlan{}
		err = s.fetchRefs(watched, path, secret, remote, plan)
	}
	if cause := context.Cause(watched); cause != nil && !errors.Is(cause, context.Canceled) {
		return plan, cause
	}
	if err != nil {
		return plan, err
	}
	if size, err := directorySize(path); err != nil || size > ceiling {
		return plan, errors.Join(errMirrorTooLarge, err)
	}

	return plan, nil
}

// fetchRefs runs plan's fetch from remote into the mirror at path.
//
// A NARROW PLAN REMOVES THE REFS GITHUB DROPPED FIRST, each only while it still
// names what the plan saw, so a new ref can take the place of one dropped
// (refs/heads/a/b where refs/heads/a was), as a pruning fetch's would.
func (s *CacheService) fetchRefs(ctx context.Context, path string, secret []string, remote string, plan gitFetchPlan) error {
	if !plan.narrow {
		_, err := s.runGit(ctx, path, secret, append(slices.Clone(gitFetchConfig),
			"fetch", "--prune", "--quiet", "--no-write-fetch-head", "--no-show-forced-updates",
			remote, "+refs/*:refs/*")...)

		return err
	}
	if len(plan.gone) > 0 {
		var deletes bytes.Buffer
		for _, name := range slices.Sorted(maps.Keys(plan.gone)) {
			fmt.Fprintf(&deletes, "delete %s %s\n", name, plan.gone[name])
		}
		if _, err := s.runGitInput(ctx, path, nil, deletes.Bytes(), "update-ref", "--stdin"); err != nil {
			return err
		}
	}
	if len(plan.moved) == 0 {
		return nil
	}
	args := append(slices.Clone(gitFetchConfig),
		"fetch", "--quiet", "--no-tags", "--no-write-fetch-head", "--no-show-forced-updates")
	for _, tip := range plan.tips {
		args = append(args, "--negotiation-tip="+tip)
	}
	args = append(args, "--", remote)
	for _, name := range plan.moved {
		args = append(args, "+"+name+":"+name)
	}
	_, err := s.runGit(ctx, path, secret, args...)

	return err
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
	return s.runGitInput(ctx, dir, extra, nil, args...)
}

// runGitInput is runGit with input on git's standard input.
func (s *CacheService) runGitInput(ctx context.Context, dir string, extra []string, input []byte, args ...string) ([]byte, error) {
	//nolint:gosec // G204: the node's configured git, with billet's own arguments, owner/repo segments gitSegment admitted and ref names narrowRefName admitted
	cmd := exec.CommandContext(ctx, s.git.binary, args...)
	killGroup(cmd)
	if input != nil {
		cmd.Stdin = bytes.NewReader(input)
	}
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
	repository, ok := s.git.granted(request, s.now())
	switch {
	case !ok:
		s.forwardGit(exchange, r, request, gitNoGrant, "")

		return
	case repository == 0:
		s.forwardGit(exchange, r, request, gitNoRepositoryID, "")

		return
	}
	// THE MIRROR SPEAKS v0. A client asking for v2 got GitHub's advertisement
	// and talks to GitHub.
	if strings.Contains(r.Header.Get("Git-Protocol"), "version=2") {
		s.forwardGit(exchange, r, request, gitProtocolV2, "")

		return
	}
	key := request.key()
	mirror := s.git.mirror(key)
	if mirror.pruning.Load() {
		s.forwardGit(exchange, r, request, gitMirrorMaintaining, "")

		return
	}
	held, cancel := context.WithTimeout(ctx, s.git.refreshWait)
	ok = rlockWithin(held, &mirror.use)
	cancel()
	if !ok {
		s.forwardGit(exchange, r, request, gitMirrorBeingRemoved, "")

		return
	}
	defer mirror.use.RUnlock()
	path := s.git.mirrorPath(key)
	stamped, exists, err := mirrorRepository(path)
	switch {
	case !exists:
		s.forwardGit(exchange, r, request, gitNoMirror, "")

		return
	case err != nil || stamped != repository || s.git.quarantined(path):
		s.forwardGit(exchange, r, request, gitMirrorNotServable, "")

		return
	}

	raw, err := io.ReadAll(io.LimitReader(r.Body, s.git.requestLimit+1))
	if err != nil {
		exchange.answered(gitRouteRefused, gitUnreadableRequest, "")
		http.Error(exchange, "an unreadable request", http.StatusBadRequest)

		return
	}
	if int64(len(raw)) > s.git.requestLimit {
		r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(raw), r.Body))
		s.forwardGit(exchange, r, request, gitNegotiationTooLarge, "")

		return
	}
	setGitBody(r, raw)
	negotiation := raw
	if r.Header.Get("Content-Encoding") == "gzip" {
		decompressed, err := gzip.NewReader(bytes.NewReader(raw))
		if err == nil {
			negotiation, err = io.ReadAll(io.LimitReader(decompressed, s.git.requestLimit+1))
		}
		if err != nil || int64(len(negotiation)) > s.git.requestLimit {
			s.forwardGit(exchange, r, request, gitNegotiationUnreadable, "")

			return
		}
	}

	objects, ok := negotiatedObjects(negotiation)
	if !ok {
		s.forwardGit(exchange, r, request, gitNegotiationUnreadable, "")

		return
	}
	// THE PROBE HAS A DEADLINE OF ITS OWN, so a stalled one leaves the request
	// alive for GitHub to answer.
	probe, cancelProbe := context.WithTimeout(ctx, s.git.firstLineWait)
	holds := s.mirrorHolds(probe, path, objects)
	cancelProbe()
	if !holds {
		s.forwardGit(exchange, r, request, gitMirrorLacksObject, "")

		return
	}
	// THE GRANT STANDS WHEN THE MIRROR ANSWERS, after the body was read, not only
	// when the request arrived: a refusal GitHub gave this header since ends it.
	if again, ok := s.git.granted(request, s.now()); !ok || again != repository {
		s.forwardGit(exchange, r, request, gitGrantEnded, "")

		return
	}
	// STILL GRANTED AND STILL VERIFIED, read at the last moment: a refresh that
	// shares this request's hold on the mirror can leave it unverified while
	// the request waits.
	servable := func() gitReason {
		if again, ok := s.git.granted(request, s.now()); !ok || again != repository {
			return gitGrantEnded
		}
		if stamped, _, err := mirrorRepository(path); err != nil || stamped != repository || s.git.quarantined(path) {
			return gitMirrorNotServable
		}

		return gitReason{}
	}
	if served, reason, detail := s.serveUploadPack(ctx, exchange, path, negotiation, servable); !served {
		s.forwardGit(exchange, r, request, reason, detail)

		return
	}
	exchange.answered(gitRouteMirror, gitServed, "")
	s.markMirrorUsed(path)
}

// serveUploadPack answers a negotiation from the mirror's upload-pack, unless
// its first line is a refusal, or it says nothing within firstLineWait or at
// all: nothing reaches the client before that line is read, so GitHub can
// still answer instead, on the request's own context, which the attempt here
// never ends.
//
// A SERVE THAT FAILS AFTER ITS ANSWER BEGAN cannot be taken back, and the
// client sees the failure; the mirror is marked unverified so the next
// advertisement removes it and every fetch after goes to GitHub until it is
// fetched again.
func (s *CacheService) serveUploadPack(
	ctx context.Context, exchange *gitExchange, path string, negotiation []byte, servable func() gitReason,
) (bool, gitReason, string) {
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
		return false, gitMirrorDidNotAnswer, "could not run the mirror's upload-pack: " + err.Error()
	}
	if err := cmd.Start(); err != nil {
		return false, gitMirrorDidNotAnswer, "could not run the mirror's upload-pack: " + err.Error()
	}
	quiet := time.AfterFunc(s.git.firstLineWait, cancel)
	first, err := readPktLine(stdout)
	// A TIMER THAT FIRED HAS KILLED upload-pack, even one whose line arrived as
	// it fired, so that answer is not served.
	answeredInTime := quiet.Stop()
	// THE GRANT AND THE MIRROR ONCE MORE, at the last moment before an answer
	// commits: the wait for the first line can outlast the grant, meet a
	// refusal GitHub gave this header meanwhile, or meet a refresh that marked
	// the mirror.
	var unservable gitReason
	if err == nil && answeredInTime {
		unservable = servable()
	}
	if err != nil || !answeredInTime || unservable != (gitReason{}) ||
		bytes.HasPrefix(first[min(len(first), 4):], []byte("ERR ")) {
		cancel()
		waitErr := cmd.Wait()
		reason, detail := gitMirrorDidNotAnswer, "the mirror's upload-pack said nothing"
		switch {
		case !answeredInTime:
			detail = fmt.Sprintf("the mirror's upload-pack said nothing within %s", s.git.firstLineWait)
		case unservable != (gitReason{}):
			reason, detail = unservable, ""
		case err == nil:
			detail = "the mirror refused: " + strings.TrimSpace(string(first[4:]))
		}
		s.log.Info("the git cache's upload-pack did not answer; github.com does", "reason", reason.text,
			"detail", detail, "error", errors.Join(err, waitErr), "stderr", boundedOutput(stderr.Bytes()))

		return false, reason, detail
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
	waitErr := cmd.Wait()
	// AN upload-pack THAT FAILED OF ITS OWN ACCORD says the mirror is broken,
	// whatever the client did after, a crash on a signal included; only the
	// SIGKILL this request's end sends says nothing of it.
	if exit, ok := errors.AsType[*exec.ExitError](waitErr); ok && (ctx.Err() == nil || !killedBySIGKILL(exit)) {
		if err := markBroken(path); err != nil {
			waitErr = errors.Join(waitErr, s.git.holdBroken(path, err))
		}
	}
	if waitErr != nil || copyErr != nil {
		s.log.Warn("a fetch from the git cache failed", "error", errors.Join(waitErr, copyErr),
			"stderr", boundedOutput(stderr.Bytes()))
	}

	return true, gitServed, ""
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

// reapBrokenMirror removes the mirror of key if it is still marked broken once
// no fetch is being served from it, and leaves it for a later pass if one is.
//
// THE MARK IS READ AGAIN UNDER THE HOLD: the walk that chose the mirror held
// nothing, and a claim may have replaced it with a healthy one since. The
// hold is the one requests take, under the key in its own case, whatever the
// case path was found in.
func (s *CacheService) reapBrokenMirror(ctx context.Context, key, path string) error {
	held := s.git.mirror(key)
	if !lockWithin(ctx, &held.use, time.Second) {
		return nil
	}
	defer held.use.Unlock()
	if _, err := os.Stat(filepath.Join(path, gitBrokenStamp)); err != nil {
		return nil
	}
	aside, err := discardGitMirror(s.git.root, path)
	if err != nil {
		return err
	}
	s.git.release(s.git.mirrorPath(key))

	return os.RemoveAll(aside)
}

// ReapGitMirrors removes the mirrors marked broken, a forgotten one among them,
// whenever they were used; then the mirrors no job has used within
// gitMirrorRetention, then the least recently used while their filesystem is
// above gitFullFraction.
func (s *CacheService) ReapGitMirrors(ctx context.Context) error {
	type mirror struct {
		key, path string
		used      time.Time
	}
	var mirrors, broken []mirror
	var discarded []string
	err := filepath.WalkDir(s.git.root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) && path == s.git.root {
				return fs.SkipAll
			}

			return err
		}
		// A MIRROR RENAMED ASIDE whose deletion never finished, a restart's
		// leftover, is no mirror. One renamed in the last ten minutes is still
		// a claim's to delete.
		if entry.IsDir() && filepath.Dir(path) == filepath.Join(s.git.root, gitDiscardDirectory) {
			if info, err := entry.Info(); err == nil && s.now().Sub(info.ModTime()) > 10*time.Minute {
				discarded = append(discarded, path)
			}

			return fs.SkipDir
		}
		// A MIRROR IS FOUND IN ANY CASE AND HELD BY ITS KEY'S: requests hold the
		// key as they spell it, lower case, and on a filesystem that ignores case
		// a directory spelled otherwise is that mirror.
		name := entry.Name()
		if !entry.IsDir() || len(name) <= len(".git") || !strings.EqualFold(name[len(name)-len(".git"):], ".git") {
			return nil
		}
		relative, err := filepath.Rel(s.git.root, path[:len(path)-len(".git")])
		if err != nil {
			return err
		}
		key := strings.ToLower(relative)
		// THE BROKEN MARK IS READ ON ITS OWN, before any other stamp: a mirror
		// forgotten after a failed refresh carries the unverified mark too, and
		// one may lack its used stamp.
		if _, err := os.Stat(filepath.Join(path, gitBrokenStamp)); err == nil {
			broken = append(broken, mirror{key: key, path: path})

			return fs.SkipDir
		}
		info, err := os.Stat(filepath.Join(path, gitUsedStamp))
		if err != nil {
			return fs.SkipDir
		}
		mirrors = append(mirrors, mirror{key: key, path: path, used: info.ModTime()})

		return fs.SkipDir
	})
	if err != nil {
		return err
	}
	slices.SortFunc(mirrors, func(a, b mirror) int { return a.used.Compare(b.used) })

	var failures []error
	for _, path := range discarded {
		if err := os.RemoveAll(path); err != nil {
			failures = append(failures, err)
		}
	}
	// A BROKEN MIRROR GOES HOWEVER RECENTLY IT WAS USED, once no fetch is being
	// served from it; one in use is taken on a later pass.
	for _, m := range broken {
		if err := s.reapBrokenMirror(ctx, m.key, m.path); err != nil {
			failures = append(failures, err)
		}
	}
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
		err = os.RemoveAll(m.path)
		if err == nil {
			s.git.release(s.git.mirrorPath(m.key))
		}
		held.use.Unlock()
		if err != nil {
			failures = append(failures, err)
		}
	}

	return errors.Join(failures...)
}
