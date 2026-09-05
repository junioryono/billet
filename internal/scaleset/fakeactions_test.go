package scaleset

import (
	"net/http"
	"testing"

	"github.com/junioryono/billet/internal/fakeactions"
	billetgithub "github.com/junioryono/billet/internal/github"
)

// fakeActions is the shared fake service, wrapped so these tests can keep
// naming the scale set they provision without repeating it in every call.
//
// The fake itself moved to internal/fakeactions when a second suite needed it:
// the end-to-end test runs the control plane, the node and a real container
// against the same service, and two copies of a wire fake would drift.
type fakeActions struct {
	*fakeactions.Server
}

// testSetName and testGroup are the tier every test here provisions. Constants
// rather than parameters because varying them proved nothing and only invited a
// helper that could disagree with the tests using it.
const (
	testSetName = "billet-4vcpu"
	testGroup   = "billet"
)

func newFakeActions(t *testing.T, handler http.HandlerFunc) *fakeActions {
	t.Helper()

	return &fakeActions{Server: fakeactions.New(t, handler)}
}

// config points billet's client at the fake, with a throwaway App key.
func (f *fakeActions) config(t *testing.T) Config {
	t.Helper()

	return Config{
		Target:         billetgithub.OrganizationTarget("acme"),
		GitHubURL:      f.URL,
		ClientID:       "12345",
		InstallationID: 67890,
		PrivateKey:     f.PrivateKeyPEM(),
	}
}

// calls forwards to the shared recorder.
func (f *fakeActions) calls(fragment string) []fakeactions.Request {
	return f.Calls(fragment)
}

func writeJSON(t *testing.T, w http.ResponseWriter, v any) {
	t.Helper()

	fakeactions.WriteJSON(t, w, v)
}

// scaleSetJSON is the shape the service returns for a scale set, fixed to the
// tier these tests provision.
func scaleSetJSON(id int, labels ...string) map[string]any {
	return fakeactions.ScaleSetJSON(id, testSetName, testGroup, labels...)
}

// listJSON wraps values the way the service returns collections.
func listJSON(values ...map[string]any) map[string]any {
	return fakeactions.ListJSON(values...)
}
