package nodeplane_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"slices"
	"testing"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/nodeapi"
	"github.com/junioryono/billet/internal/nodeplane"
)

// registerAnswer registers n1 at exactly one wire version and returns the
// plane's answer and its raw body.
func registerAnswer(t *testing.T, base string, version int) (nodeapi.RegisterResponse, []byte) {
	t.Helper()

	body, err := json.Marshal(nodeapi.RegisterRequest{
		MinVersion: version, Version: version, Incarnation: "inc-1",
		Node: "n1", Provider: config.ProviderDocker, GuestOS: []config.GuestOS{config.GuestLinux},
		Deployment: deployment, VCPU: testNodeVCPU, Memory: testNodeMemory,
	})
	if err != nil {
		t.Fatal(err)
	}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, base+"/v1/register", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}

	req.Header.Set(nodeapi.HeaderIncarnation, "inc-1")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var raw bytes.Buffer
	if _, err := raw.ReadFrom(resp.Body); err != nil {
		t.Fatal(err)
	}

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("register at wire %d answered %d: %s", version, resp.StatusCode, raw.String())
	}

	var res nodeapi.RegisterResponse
	if err := json.Unmarshal(raw.Bytes(), &res); err != nil {
		t.Fatal(err)
	}

	return res, raw.Bytes()
}

// A REGISTRATION'S ANSWER CARRIES THE DEPLOYMENT'S IMAGE ACCEPTANCE FROM THE
// VERSION THAT DEFINES IT, and an answer below it is the bytes it always was.
func TestARegistrationCarriesTheImageAcceptanceFromItsVersion(t *testing.T) {
	t.Parallel()

	accepted := []config.ImageComponent{config.ComponentAndroidSDK, config.ComponentGoogleChrome}

	for _, tc := range []struct {
		// LITERAL VERSIONS, so moving the constant cannot move the boundary the
		// fleet already runs: 32 is the last wire without the field.
		version int
		want    []string
	}{
		{32, nil},
		{33, []string{"android-sdk", "google-chrome"}},
	} {
		_, base := serve(t, &fakeStore{}, nodeplane.WithImageAcceptance(accepted))

		res, raw := registerAnswer(t, base, tc.version)
		if !slices.Equal(res.ImagesAccept, tc.want) {
			t.Errorf("at wire %d the answer accepts %v, want %v", tc.version, res.ImagesAccept, tc.want)
		}

		if tc.want == nil && bytes.Contains(raw, []byte("images_accept")) {
			t.Errorf("at wire %d the answer names the field: %s", tc.version, raw)
		}
	}
}
