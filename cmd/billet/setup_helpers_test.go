package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"testing"

	"github.com/junioryono/billet/internal/github"
	"github.com/junioryono/billet/internal/ops/setup"
)

// Helpers cmd/billet's tests share with internal/ops/setup's, copied rather
// than imported: a test helper is not part of any package's API.

// testKey returns a valid PEM-encoded App private key.
func testKey(t *testing.T) []byte {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	return pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
}

// stubOnboard replaces the onboarding seam for one test and reports how many
// times it was reached.
//
// A COUNTER RATHER THAN AN ERROR RETURN IS THE POINT for the refusals below.
// "githubAppCreate returned an error" is satisfied by the old behaviour too —
// which created the App, failed to record it, printed the block and returned
// NIL — so the assertion that actually distinguishes them is that GitHub was
// never reached at all.
//
// It judges nothing: when it is told to fail it fails, and otherwise it calls
// OnAppCreated with a real key and returns what it was given. A fake that
// re-implemented the production rule it is used to test would pass while the
// real one was deleted.
func stubOnboard(t *testing.T, key []byte, fail error) *int {
	t.Helper()

	calls := 0

	prev := setup.Onboard
	t.Cleanup(func() { setup.Onboard = prev })

	setup.Onboard = func(_ context.Context, opts github.OnboardOptions) (*github.Onboarding, error) {
		calls++

		if fail != nil {
			// BEFORE OnAppCreated, so this models the flow ending without an App:
			// the operator closed the browser, or the hour lapsed. Nothing is
			// written, which is what the config assertions rely on.
			return nil, fail
		}

		app := &github.App{ID: stubAppID, ClientID: "Iv1.stub", PEM: string(key)}
		if err := opts.OnAppCreated(app); err != nil {
			return nil, err
		}

		return &github.Onboarding{
			App:          app,
			Installation: &github.Installation{ID: stubInstallationID},
		}, nil
	}

	return &calls
}

// asLinux pins the generation to the systemd shape — /etc/billet, /var/lib, the
// billet group, mode 0640 — on the darwin machines billet is developed on. Not
// parallel-safe; none of these tests are parallel.
// asLinux says this host is Linux to EVERY seam that asks, not only to the
// command layer's own: the retirement's platform decides whether the global
// authority exclusion exists at all, and a test that pinned one and not the
// other proved its ordering against a host no fleet runs (found by CI, which
// runs where both are Linux for real, 2026-09-12).
func asLinux(t *testing.T) {
	t.Helper()
	useRetirementRoot(t)
}

// The App the stub mints. Distinctive values, so an assertion cannot be
// satisfied by a zero left behind from somewhere else.
const (
	stubAppID          = 4242
	stubInstallationID = 99
)
