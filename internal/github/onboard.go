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
	"strconv"
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
	// Log receives human-facing progress. Required.
	Log func(format string, args ...any)
	// Client is optional; a sane default is used when nil.
	Client *http.Client
	// InstallPoll is how often to check whether the install finished, used when
	// the post-install redirect never arrives.
	InstallPoll time.Duration

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

	if opts.InstallPoll <= 0 {
		opts.InstallPoll = 3 * time.Second
	}

	// Bind before building any URL: the manifest must carry the real port, and
	// the port is only known once the listener exists.
	var lc net.ListenConfig

	listener, err := lc.Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("github: open loopback listener: %w", err)
	}
	defer listener.Close()

	base := "http://" + listener.Addr().String()

	state, err := randomState()
	if err != nil {
		return nil, err
	}

	flow := &onboardFlow{
		opts:     opts,
		state:    state,
		base:     base,
		codeCh:   make(chan string, 1),
		installC: make(chan int64, 1),
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
	codeCh   chan string
	installC chan int64
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

	var installationID int64

	select {
	case installationID = <-f.installC:
		// The redirect arrived. Still resolve through the API so the permission
		// check below has a real installation to inspect.
	case inst := <-polled:
		return f.verify(inst), nil
	case err := <-pollErr:
		return nil, err
	case err := <-f.errCh:
		return nil, err
	case <-ctx.Done():
		return nil, fmt.Errorf("github: timed out waiting for installation: %w", ctx.Err())
	}

	inst, err := getOrgInstallationAt(ctx, f.opts.Client, f.opts.api(), app.ID, []byte(app.PEM), f.opts.Org)
	if err != nil {
		// The redirect told us the id, so report that rather than failing outright.
		f.opts.Log("Installed (id %d), but reading it back failed: %v", installationID, err)
		return &Installation{ID: installationID}, nil
	}

	return f.verify(inst), nil
}

// verify warns about a permission the operator removed during installation. Not
// fatal — billet should still write the config — but silence here becomes an
// inscrutable failure at job time.
func (f *onboardFlow) verify(inst *Installation) *Installation {
	if missing := inst.MissingPermissions(); len(missing) > 0 {
		f.opts.Log("")
		f.opts.Log("WARNING: the installation is missing permissions billet needs:")

		for _, m := range missing {
			f.opts.Log("  - %s", m)
		}

		f.opts.Log("Runner registration will fail until these are granted.")
	}

	return inst
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
	} else {
		// The headless path. A server being onboarded over SSH is the normal case,
		// not the exception, so this must read as an instruction rather than a
		// fallback apology.
		f.opts.Log("Open this URL in a browser:")
	}

	f.opts.Log("  %s", target)
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

	// Constant-time, and checked before anything else is read from the request.
	if subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(f.state)) != 1 {
		http.Error(w, "state mismatch — restart `billet github-app create`", http.StatusBadRequest)
		f.fail(fmt.Errorf("github: state mismatch on callback (possible CSRF)"))

		return
	}

	code := q.Get("code")
	if code == "" {
		http.Error(w, "no code in callback", http.StatusBadRequest)
		f.fail(fmt.Errorf("github: callback carried no code"))

		return
	}

	select {
	case f.codeCh <- code:
	default:
	}

	writePage(w, "App created", "Now install it on your organization. You can close this tab when the CLI says it is done.")
}

func (f *onboardFlow) handleInstalled(w http.ResponseWriter, r *http.Request) {
	raw := r.URL.Query().Get("installation_id")

	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		// Not fatal: the poller resolves the installation independently. Say so
		// rather than showing the operator an error for a flow that is working.
		writePage(w, "Installed", "Finishing up in the CLI.")
		return
	}

	select {
	case f.installC <- id:
	default:
	}

	writePage(w, "Installed", "billet is configured. You can close this tab.")
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
