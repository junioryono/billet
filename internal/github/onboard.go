package github

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"net/url"
	"time"
)

// ErrCredentialPreserved marks a failure in which the App's private key was NOT
// lost: it exists on disk, and the error that carries this sentinel says where.
//
// It exists because the correct recovery advice is the opposite in the two
// cases. A key that was never stored means the App is unusable and should be
// deleted on GitHub; a key that was stored somewhere unexpected means the App
// must be KEPT and the file moved into place. Onboarding cannot tell the two
// apart from the error text, and the wrong instruction destroys a credential
// GitHub will not re-issue.
//
// Callers that persist the credential wrap their errors with this whenever the
// key reached durable storage.
var ErrCredentialPreserved = errors.New("the App key was preserved")

// ErrCredentialUncertain marks a failure where billet could not determine
// whether the key survived — an inspection that itself failed, rather than one
// that found nothing.
//
// It suppresses the same destructive advice ErrCredentialPreserved does, and
// promises less. Reporting an unverifiable file as preserved would send the
// operator to a path that may be empty; reporting it as lost would have them
// delete an App whose key may be sitting right there. Neither is honest, so
// there are three outcomes rather than two.
var ErrCredentialUncertain = errors.New("billet could not verify whether the App key was saved")

// codeQueueDepth is how many pending registration callbacks the loopback
// listener will hold.
//
// GitHub sends exactly one. The depth exists for the injected ones: with a
// single slot, a local process could keep the queue full across an in-flight
// exchange and the honest redirect would be dropped on arrival.
const codeQueueDepth = 32

// maxManifestCodeLen bounds what the callback will accept as a manifest code.
// GitHub's are short; this is generous enough to survive a format change and
// small enough that no request built from one can draw a 414.
const maxManifestCodeLen = 512

// Onboarding result. Credentials live here only long enough to be written to
// disk by the caller.
type Onboarding struct {
	App          *App
	Installation *Installation
}

// OnboardOptions configures the manifest flow.
type OnboardOptions struct {
	Org string
	// Name pre-fills the app name. GitHub app names are globally unique, so this
	// is a suggestion the operator edits on GitHub's own page.
	Name string
	// OpenBrowser is called with each URL the operator must visit. Returning an
	// error is not fatal: the URL is printed for manual use, which is what makes
	// the flow work over SSH.
	OpenBrowser func(context.Context, string) error
	// OnAppCreated is called the moment the app's credentials exist, BEFORE the
	// installation step. Required.
	//
	// This ordering is the whole point. GitHub registers the app during the
	// browser redirect, and the private key is returned exactly once by the
	// conversion — so if billet waited until the end of a successful onboarding
	// to persist it, every failure in the installation phase (a timeout, a
	// cancelled context, an API error, an operator who walks away) would leave a
	// real registered app whose only key had been discarded. Returning an error
	// here aborts, because continuing would produce that same orphan.
	OnAppCreated func(*App) error

	// Log receives human-facing progress. Required.
	Log func(format string, args ...any)
	// Client is optional; a sane default is used when nil.
	Client *http.Client
	// InstallPoll is how often to check whether the install finished, used when
	// the post-install redirect never arrives.
	InstallPoll time.Duration

	// Port fixes the loopback callback port. Zero picks a free one.
	//
	// It exists for the remote case: onboarding a CI host over SSH is the normal
	// way this runs, and there the callback listens on the SERVER's loopback
	// while the browser is on a laptop, where 127.0.0.1 means the laptop. That
	// needs `ssh -L`, and a forward needs a port known in advance.
	Port int

	// apiBase overrides GitHub's API host. Unexported: this exists so the flow
	// can be driven end to end against a fake in tests, not as a supported way to
	// point billet at another host.
	apiBase string
}

func (o OnboardOptions) api() string {
	if o.apiBase != "" {
		return o.apiBase
	}

	return apiBase
}

// Onboard runs the whole flow: register the app from a manifest, exchange the
// code for credentials, then wait for the operator to install it.
//
// Two browser steps, because GitHub genuinely has two: creating an app does NOT
// install it, and the installation is where the installation ID comes from.
// Anything claiming this is one click is describing only the first half.
func Onboard(ctx context.Context, opts OnboardOptions) (*Onboarding, error) {
	if opts.Org == "" {
		return nil, fmt.Errorf("github: organization is required")
	}

	if opts.Log == nil {
		return nil, fmt.Errorf("github: OnboardOptions.Log is required")
	}

	if opts.OnAppCreated == nil {
		return nil, fmt.Errorf("github: OnboardOptions.OnAppCreated is required — " +
			"credentials must be persisted before the installation step")
	}

	if opts.InstallPoll <= 0 {
		opts.InstallPoll = 3 * time.Second
	}

	// Bind before building any URL: the manifest must carry the real port, and
	// the port is only known once the listener exists.
	var lc net.ListenConfig

	addr := fmt.Sprintf("127.0.0.1:%d", opts.Port)

	listener, err := lc.Listen(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("github: open loopback listener on %s: %w", addr, err)
	}
	defer listener.Close()

	state, err := randomState()
	if err != nil {
		return nil, err
	}

	// Every route sits under an unguessable prefix.
	//
	// The start page carries the state in its form, and it was served from "/"
	// on loopback — so any local process could fetch it, learn the state, and
	// then present a valid-looking callback with a bogus code. Refusing invalid
	// callbacks does not help there: the state IS valid, billet accepts the code,
	// the exchange fails, and the flow dies with GitHub's App already created.
	//
	// A 256-bit path is the same secret the state is, applied one step earlier.
	//
	// Its guarantee is narrower than it looks, and worth stating exactly: it
	// defeats a process that must GUESS the path — port scanning, or walking
	// loopback for a known route. It does NOT defeat a process that can observe
	// this one, and on a shared host that is not a high bar: the URL is handed to
	// `open`/`xdg-open` as a command-line argument, and argv is readable through
	// /proc or ps. So the path and the state are both treated as knowable, and
	// what a caller can DO with them is bounded separately — a forged callback
	// cannot end the flow (handleCallback) and an injected code cannot either
	// (register).
	secretPath, err := randomState()
	if err != nil {
		return nil, err
	}

	base := "http://" + listener.Addr().String() + "/" + secretPath

	tcpAddr, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		return nil, fmt.Errorf("github: unexpected listener address type %T", listener.Addr())
	}

	flow := &onboardFlow{
		opts:   opts,
		state:  state,
		base:   base,
		prefix: secretPath,
		port:   tcpAddr.Port,
		// Deep enough that a legitimate redirect is never turned away while an
		// exchange is in flight. It does not stop a local process from filling it
		// — nothing can, since argv hands out the path — but the flow now
		// survives that as a delay bounded by ManifestTTL rather than as a lost
		// credential.
		codeCh:   make(chan string, codeQueueDepth),
		installC: make(chan struct{}, 1),
		errCh:    make(chan error, 1),
	}

	srv := &http.Server{
		Handler:           flow.routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	defer srv.Close()

	go func() {
		if err := srv.Serve(listener); err != nil && err != http.ErrServerClosed {
			flow.fail(fmt.Errorf("github: loopback server: %w", err))
		}
	}()

	// The whole handshake — register, redirect, exchange — must complete inside
	// GitHub's one-hour window, so bound the wait rather than hanging forever on
	// an operator who walked away.
	ctx, cancel := context.WithTimeout(ctx, ManifestTTL)
	defer cancel()

	app, err := flow.register(ctx)
	if err != nil {
		return nil, err
	}

	inst, err := flow.install(ctx, app)
	if err != nil {
		return nil, err
	}

	// The key is on disk and the installation is resolved, so nothing downstream
	// needs a credential. Blanking them here means a caller that logs the result
	// — the obvious thing to do with a returned struct — cannot leak one.
	app.Forget()

	return &Onboarding{App: app, Installation: inst}, nil
}

type onboardFlow struct {
	opts  OnboardOptions
	state string
	// base already includes the unguessable path prefix, so every URL derived
	// from it inherits the guard.
	base string
	// prefix is that path alone, for registering routes on the mux.
	prefix   string
	port     int
	codeCh   chan string
	installC chan struct{}
	errCh    chan error

	app *App
}

func (f *onboardFlow) routes() http.Handler {
	mux := http.NewServeMux()

	// Nothing is registered at "/", so a process that has not guessed the prefix
	// gets 404 from every path — including the start page that carries the state.
	root := "/" + f.prefix
	mux.HandleFunc(root+"/", f.handleStart)
	mux.HandleFunc(root+"/callback", f.handleCallback)
	mux.HandleFunc(root+"/installed", f.handleInstalled)

	return mux
}

func (f *onboardFlow) fail(err error) {
	select {
	case f.errCh <- err:
	default:
	}
}

// register drives step one: serve the self-submitting form, wait for GitHub's
// redirect, exchange the code.
func (f *onboardFlow) register(ctx context.Context) (*App, error) {
	startURL := f.base + "/"

	f.opts.Log("Creating the GitHub App for %s.", f.opts.Org)
	f.opts.Log("")
	f.openOrPrint(ctx, startURL)

	// A code that does not redeem is DISCARDED, and the flow keeps waiting.
	//
	// The state stops a process that has to guess it, but billet passes the
	// callback URL to `open`/`xdg-open` as a command-line argument, and argv is
	// readable by other processes on this machine — so both the path and the
	// state have to be assumed known. Treating the first code to arrive as final
	// then handed any local process a kill switch: inject a worthless code, watch
	// the exchange fail, and onboarding ends with GitHub's App already created
	// and its one-time private key unrecoverable.
	//
	// Only GitHub's own rejection of a code is retried. A request that could not
	// be completed at all is reported immediately, because no second redirect is
	// coming to fix it.
	var lastRejection error

	for {
		var code string

		select {
		case code = <-f.codeCh:
		case err := <-f.errCh:
			return nil, err
		case <-ctx.Done():
			if lastRejection != nil {
				// The honest failure — an operator who took longer than GitHub's
				// window — must not be reported as a bare timeout, or the one
				// message that explains it is the one that gets dropped.
				return nil, fmt.Errorf("github: no usable registration arrived: %w", lastRejection)
			}

			return nil, fmt.Errorf("github: timed out waiting for app registration: %w", ctx.Err())
		}

		f.opts.Log("Registration received. Exchanging it for credentials...")

		app, err := convertManifestAt(ctx, f.opts.Client, f.opts.api(), code)
		if err == nil {
			return f.persist(app)
		}

		if !errors.Is(err, errCodeRejected) {
			return nil, err
		}

		lastRejection = err

		f.opts.Log("GitHub rejected that registration (%v). Still waiting for the redirect.", err)
	}
}

// persist hands the credentials to the caller before the installation step.
func (f *onboardFlow) persist(app *App) (*App, error) {
	f.app = app
	f.opts.Log("Created app %q (id %d).", app.Name, app.ID)

	// Persist BEFORE the installation step. See OnAppCreated: from here on the
	// app is real on GitHub, and its key cannot be re-issued.
	if err := f.opts.OnAppCreated(app); err != nil {
		// The advice depends on whether the key survived, and getting it wrong
		// destroys things. Wrapping EVERY failure with "delete it and try again"
		// contradicted the callback's own message — which names the path the key
		// was preserved at — and an operator who followed the outer advice would
		// delete the App that preserved key belongs to.
		if errors.Is(err, ErrCredentialPreserved) || errors.Is(err, ErrCredentialUncertain) {
			return nil, fmt.Errorf(
				"github: app %d was created on GitHub and its key was preserved, but onboarding "+
					"could not finish (%w). Do NOT delete the app: follow the instruction above, "+
					"then run `billet check`", app.ID, err)
		}

		return nil, fmt.Errorf(
			"github: app %d was created on GitHub but its credentials could not be saved (%w); "+
				"delete it at %s and try again", app.ID, err, app.HTMLURL)
	}

	return app, nil
}

// install drives step two. The post-install redirect is the fast path; polling
// is what makes the flow survive a closed tab or an install finished elsewhere.
func (f *onboardFlow) install(ctx context.Context, app *App) (*Installation, error) {
	f.opts.Log("")
	f.opts.Log("Creating an app does not install it. Installing it on %s now.", f.opts.Org)
	f.opts.Log("")
	f.openOrPrint(ctx, app.InstallURL())

	pollCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	polled := make(chan *Installation, 1)
	pollErr := make(chan error, 1)

	go func() {
		inst, err := waitForOrgInstallationAt(
			pollCtx, f.opts.Client, f.opts.api(), app.ID, []byte(app.PEM), f.opts.Org, f.opts.InstallPoll)
		if err != nil {
			pollErr <- err
			return
		}

		polled <- inst
	}()

	// The setup-URL callback is a WAKE-UP SIGNAL ONLY, never a source of truth.
	//
	// GitHub documents that the installation_id on a setup URL is spoofable —
	// anything on this host can call the loopback endpoint with any number. The
	// installation id ends up in billet.yaml and decides which installation
	// billet registers runners against, so a value that never crossed an
	// authenticated API is a value that must not be written down. The callback
	// only shortens the wait; the poller below is what actually answers.
	select {
	case <-f.installC:
		f.opts.Log("Installation reported. Confirming with GitHub...")
	case inst := <-polled:
		return f.verify(inst)
	case err := <-pollErr:
		return nil, err
	case err := <-f.errCh:
		return nil, err
	case <-ctx.Done():
		return nil, fmt.Errorf("github: timed out waiting for installation: %w", ctx.Err())
	}

	// Keep waiting on the same authenticated poll. A 404 immediately after the
	// callback is ordinary — GitHub's own redirect can beat its API's
	// consistency — so this continues rather than concluding anything.
	select {
	case inst := <-polled:
		return f.verify(inst)
	case err := <-pollErr:
		return nil, err
	case <-ctx.Done():
		return nil, fmt.Errorf(
			"github: the browser reported an installation but GitHub's API never confirmed it: %w", ctx.Err())
	}
}

// verify fails onboarding when the installation's permissions are not exactly
// what billet asked for.
//
// Both directions are fatal, for different reasons. TOO FEW and runner
// registration fails later with an error that never mentions permissions. TOO
// MANY and billet holds access it advertises that it does not have — an app
// edited to add `contents` or `actions` between creation and installation would
// otherwise sail through and make the README's central claim false.
//
// The app key has already been written by this point, so failing here is
// recoverable: fix the permissions on GitHub and re-run `billet check`.
func (f *onboardFlow) verify(inst *Installation) (*Installation, error) {
	problems := inst.PermissionMismatches()
	if len(problems) == 0 {
		return inst, nil
	}

	f.opts.Log("")
	f.opts.Log("The installation's permissions do not match what billet requested:")

	for _, p := range problems {
		f.opts.Log("  - %s", p)
	}

	// %s/%d, not %s/installations/%d: orgSettingsURL already ends in
	// /installations, so the old form produced
	// .../settings/installations/installations/<id> — a 404 handed to an
	// operator who is already stuck.
	return nil, fmt.Errorf(
		"github: installation %d has %d permission mismatch(es); "+
			"correct them at %s/%d and re-run `billet check`",
		inst.ID, len(problems), f.orgSettingsURL(), inst.ID)
}

func (f *onboardFlow) orgSettingsURL() string {
	return fmt.Sprintf("%s/organizations/%s/settings/installations", webBase, url.PathEscape(f.opts.Org))
}

func (f *onboardFlow) openOrPrint(ctx context.Context, target string) {
	opened := false

	if f.opts.OpenBrowser != nil {
		if err := f.opts.OpenBrowser(ctx, target); err == nil {
			opened = true
		}
	}

	if opened {
		// The URL is deliberately NOT repeated here. It carries the unguessable
		// path, and printing it again puts that in the terminal scrollback and in
		// any journal or CI log capturing this output — for no benefit, since the
		// browser already has it. An operator whose browser did not appear gets a
		// route that does not widen the exposure.
		f.opts.Log("Opened your browser.")
		f.opts.Log("If nothing appeared, press Ctrl-C and re-run with --no-browser to get the URL.")
		f.opts.Log("")

		return
	}

	// The headless path, and the normal one: a CI host is usually onboarded over
	// SSH. Saying "open this URL" alone is wrong there — the callback listens on
	// THIS machine's loopback, so 127.0.0.1 in a laptop browser is the laptop.
	// Without the forward the browser lands on a connection error with nothing
	// explaining why, so the instruction has to come with the URL, not after it.
	f.opts.Log("Open this URL in a browser:")
	f.opts.Log("  %s", target)
	f.opts.Log("")
	f.opts.Log("If billet is running on a remote host, first forward the callback port")
	f.opts.Log("from the machine with the browser:")
	f.opts.Log("  ssh -L %d:127.0.0.1:%d %s", f.port, f.port, "<this-host>")
	f.opts.Log("")
	f.opts.Log("Re-run with --port to pin the port across attempts.")
	f.opts.Log("")
}

// handleStart serves the self-submitting form. A plain redirect cannot work:
// GitHub requires the manifest in a POST body.
func (f *onboardFlow) handleStart(w http.ResponseWriter, _ *http.Request) {
	manifest := NewManifest(f.opts.Name, f.base+"/callback", f.base+"/installed")

	encoded, err := json.Marshal(manifest)
	if err != nil {
		f.fail(fmt.Errorf("github: encode manifest: %w", err))
		http.Error(w, "failed to build manifest", http.StatusInternalServerError)

		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")

	data := struct {
		Action   string
		Manifest string
	}{
		Action:   RegistrationURL(f.opts.Org, f.state),
		Manifest: string(encoded),
	}

	if err := startPage.Execute(w, data); err != nil {
		f.fail(fmt.Errorf("github: render start page: %w", err))
	}
}

func (f *onboardFlow) handleCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	// A bad request is REFUSED; it does not end the flow.
	//
	// This listens on loopback, so any unprivileged process on the machine can
	// reach it — and with --port, at an address it can guess. Treating a wrong
	// state or a missing code as a fatal error handed that process a kill
	// switch: one request ended onboarding, and if it landed after GitHub had
	// created the App but before billet exchanged the one-time code, the App and
	// its private key were orphaned with no way to recover them. A local denial
	// of service became credential loss.
	//
	// Continuing to wait costs nothing. The legitimate redirect still arrives,
	// and the caller's deadline still bounds the flow.
	if subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(f.state)) != 1 {
		// Constant-time, and checked before anything else is read.
		http.Error(w, "state mismatch", http.StatusBadRequest)

		return
	}

	code := q.Get("code")
	if code == "" {
		http.Error(w, "no code in callback", http.StatusBadRequest)

		return
	}

	// Bounded before it is queued. An absurdly long code costs a round trip and
	// draws a 414 rather than GitHub's own rejection, which is a different
	// failure classification for what is obviously not a real code — cheaper to
	// refuse it here than to reason about every status a proxy might invent.
	// GitHub's codes are far shorter than this.
	if len(code) > maxManifestCodeLen {
		http.Error(w, "code is too long to be a GitHub manifest code", http.StatusBadRequest)

		return
	}

	// A dropped code must not be acknowledged as accepted.
	//
	// The channel held one element and a full one was silently discarded, so two
	// injected codes timed around an in-flight exchange could crowd out GitHub's
	// honest redirect — which was then told "App created" while its code had
	// been thrown away, leaving onboarding waiting for a redirect that had
	// already come and gone. The queue is deep enough that ordinary use never
	// reaches it, and a caller that does not fit is told so rather than lied to.
	select {
	case f.codeCh <- code:
	default:
		http.Error(w, "billet is still working through earlier registrations; retry in a moment",
			http.StatusServiceUnavailable)

		return
	}

	writePage(w, "App created", "Now install it on your organization. You can close this tab when the CLI says it is done.")
}

// handleInstalled is a wake-up signal and nothing more.
//
// The installation_id GitHub puts on a setup URL is spoofable — GitHub says so
// — and anything running on this host can call this endpoint with any number.
// That id would end up in billet.yaml deciding which installation billet
// registers runners against, so it is deliberately NOT read. The authenticated
// poll is what answers; this only saves the operator a few seconds of waiting.
func (f *onboardFlow) handleInstalled(w http.ResponseWriter, _ *http.Request) {
	select {
	case f.installC <- struct{}{}:
	default:
	}

	writePage(w, "Installed", "Confirming with GitHub. You can close this tab once the CLI finishes.")
}

func randomState() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("github: generate state: %w", err)
	}

	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func urlPathEscape(s string) string  { return url.PathEscape(s) }
func urlQueryEscape(s string) string { return url.QueryEscape(s) }

var startPage = template.Must(template.New("start").Parse(`<!doctype html>
<html><head><meta charset="utf-8"><title>billet — create GitHub App</title></head>
<body style="font-family:system-ui,sans-serif;margin:4rem auto;max-width:34rem">
<h1>Creating your GitHub App</h1>
<p>Redirecting to GitHub. If nothing happens, press the button.</p>
<form id="f" method="post" action="{{.Action}}">
  <input type="hidden" name="manifest" value="{{.Manifest}}">
  <button type="submit">Continue to GitHub</button>
</form>
<script>document.getElementById("f").submit()</script>
</body></html>`))

func writePage(w http.ResponseWriter, title, body string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")

	// A render failure here is cosmetic: the operator's browser shows a blank
	// page while the CLI carries on with the credentials it already holds. There
	// is nowhere useful to report it, and failing the flow over it would be worse.
	if err := donePage.Execute(w, struct{ Title, Body string }{title, body}); err != nil {
		http.Error(w, title, http.StatusInternalServerError)
	}
}

var donePage = template.Must(template.New("done").Parse(`<!doctype html>
<html><head><meta charset="utf-8"><title>billet — {{.Title}}</title></head>
<body style="font-family:system-ui,sans-serif;margin:4rem auto;max-width:34rem">
<h1>{{.Title}}</h1><p>{{.Body}}</p>
</body></html>`))
