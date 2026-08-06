package github

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"net/url"
	"time"
)

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

	base := "http://" + listener.Addr().String()

	state, err := randomState()
	if err != nil {
		return nil, err
	}

	tcpAddr, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		return nil, fmt.Errorf("github: unexpected listener address type %T", listener.Addr())
	}

	flow := &onboardFlow{
		opts:     opts,
		state:    state,
		base:     base,
		port:     tcpAddr.Port,
		codeCh:   make(chan string, 1),
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

	return &Onboarding{App: app, Installation: inst}, nil
}

type onboardFlow struct {
	opts     OnboardOptions
	state    string
	base     string
	port     int
	codeCh   chan string
	installC chan struct{}
	errCh    chan error

	app *App
}

func (f *onboardFlow) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", f.handleStart)
	mux.HandleFunc("/callback", f.handleCallback)
	mux.HandleFunc("/installed", f.handleInstalled)

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

	var code string

	select {
	case code = <-f.codeCh:
	case err := <-f.errCh:
		return nil, err
	case <-ctx.Done():
		return nil, fmt.Errorf("github: timed out waiting for app registration: %w", ctx.Err())
	}

	f.opts.Log("Registration received. Exchanging it for credentials...")

	app, err := convertManifestAt(ctx, f.opts.Client, f.opts.api(), code)
	if err != nil {
		return nil, err
	}

	f.app = app
	f.opts.Log("Created app %q (id %d).", app.Name, app.ID)

	// Persist BEFORE the installation step. See OnAppCreated: from here on the
	// app is real on GitHub, and its key cannot be re-issued.
	if err := f.opts.OnAppCreated(app); err != nil {
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

	return nil, fmt.Errorf(
		"github: installation %d has %d permission mismatch(es); "+
			"correct them at %s/installations/%d and re-run `billet check`",
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
		f.opts.Log("Opened your browser. If nothing happened, visit:")
		f.opts.Log("  %s", target)
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

	select {
	case f.codeCh <- code:
	default:
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
