package host

import (
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/config"
)

// aMirror is a TLS listener answering the registry API's base route with status,
// whose certificate is an authority the system pool does not hold; and that
// authority's PEM.
func aMirror(t *testing.T, status int) (string, string) {
	t.Helper()

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/" {
			t.Errorf("the probe asked for %s", r.URL.Path)
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(server.Close)

	return server.URL, string(pem.EncodeToMemory(&pem.Block{
		Type: "CERTIFICATE", Bytes: server.Certificate().Raw,
	}))
}

func firecrackerNode(mirrors *config.RegistryMirrors, authority string) *config.Config {
	return &config.Config{Node: &config.NodeConfig{
		Provider: config.ProviderFirecracker, Site: "home",
		RegistryMirrors: mirrors, RegistryMirrorsCA: authority,
	}}
}

func TestCheckReportsASiteWithNoDockerHubMirrorWithoutRefusingIt(t *testing.T) {
	t.Parallel()

	for _, mirrors := range []*config.RegistryMirrors{nil, {}} {
		lines, err := judgeRegistryMirrors(t.Context(), firecrackerNode(mirrors, ""), false)
		if err != nil {
			t.Fatalf("a site with no mirror was refused: %v", err)
		}
		report := strings.Join(lines, "\n")
		for _, want := range []string{"NO DOCKER HUB MIRROR at site home", "toomanyrequests", "billet_registry_mirrors"} {
			if !strings.Contains(report, want) {
				t.Errorf("the report does not say %q:\n%s", want, report)
			}
		}
	}
}

func TestCheckSaysNothingOfMirrorsWhereNoGuestConsumesThem(t *testing.T) {
	t.Parallel()

	for _, cfg := range []*config.Config{
		{},
		{Node: &config.NodeConfig{Provider: config.ProviderDocker}},
		{Node: &config.NodeConfig{Provider: config.ProviderEC2}},
	} {
		lines, err := judgeRegistryMirrors(t.Context(), cfg, false)
		if len(lines) != 0 || err != nil {
			t.Errorf("%+v: reported %q, %v", cfg.Node, lines, err)
		}
	}
}

func TestCheckProbesEachMirrorTrustingItsAuthority(t *testing.T) {
	t.Parallel()

	answering, authority := aMirror(t, http.StatusOK)
	wantsCredential, _ := aMirror(t, http.StatusUnauthorized)
	broken, _ := aMirror(t, http.StatusBadGateway)
	// httptest's servers share one certificate, so one authority signs all three.
	path := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(path, []byte(authority), 0o600); err != nil {
		t.Fatalf("write the authority: %v", err)
	}

	lines, err := judgeRegistryMirrors(t.Context(), firecrackerNode(&config.RegistryMirrors{
		DockerIO: answering, GHCRIO: wantsCredential, QuayIO: broken,
	}, path), false)
	if err != nil {
		t.Fatalf("a mirror that does not answer was refused: %v", err)
	}
	if len(lines) != 3 {
		t.Fatalf("want one line per mirror: %q", lines)
	}
	if want := "docker.io through " + answering + " answers"; lines[0] != want {
		t.Errorf("line = %q, want %q", lines[0], want)
	}
	if want := "ghcr.io through " + wantsCredential + " answers"; lines[1] != want {
		t.Errorf("a registry that asks for a credential answered: line = %q, want %q", lines[1], want)
	}
	if !strings.HasPrefix(lines[2], "NOT ANSWERING: quay.io through "+broken) ||
		!strings.Contains(lines[2], "502") {
		t.Errorf("line = %q, want the 502 said", lines[2])
	}
}

// WITHOUT THE AUTHORITY THE PROBE FAILS AS A GUEST WOULD, which is the proof that
// it is what the probe trusted above.
func TestCheckDoesNotTrustAMirrorWithoutItsAuthority(t *testing.T) {
	t.Parallel()

	mirror, _ := aMirror(t, http.StatusOK)
	lines, err := judgeRegistryMirrors(t.Context(), firecrackerNode(&config.RegistryMirrors{
		DockerIO: mirror, GHCRIO: mirror, QuayIO: mirror,
	}, ""), false)
	if err != nil {
		t.Fatalf("an untrusted mirror was refused: %v", err)
	}
	if len(lines) != 3 {
		t.Fatalf("want one line per mirror: %q", lines)
	}
	for _, line := range lines {
		if !strings.HasPrefix(line, "NOT ANSWERING:") || !strings.Contains(line, "certificate") {
			t.Errorf("line = %q, want the certificate named", line)
		}
	}
}

func TestCheckSaysAMaintenanceRunProbedNoMirror(t *testing.T) {
	t.Parallel()

	const nowhere = "https://mirror.invalid"
	lines, err := judgeRegistryMirrors(t.Context(), firecrackerNode(&config.RegistryMirrors{
		DockerIO: nowhere, GHCRIO: nowhere, QuayIO: nowhere,
	}, ""), true)
	if err != nil {
		t.Fatalf("judgeRegistryMirrors: %v", err)
	}
	if len(lines) != 3 {
		t.Fatalf("want one line per mirror: %q", lines)
	}
	for _, line := range lines {
		if !strings.Contains(line, "(configured, not probed)") {
			t.Errorf("line = %q, want it to say nothing was probed", line)
		}
	}
}

func TestCheckRefusesAnAuthorityItCannotHandAGuest(t *testing.T) {
	t.Parallel()

	const key = "-----BEGIN PRIVATE KEY-----\nc2VjcmV0\n-----END PRIVATE KEY-----\n"
	directory := t.TempDir()
	path := filepath.Join(directory, "key.pem")
	if err := os.WriteFile(path, []byte(key), 0o600); err != nil {
		t.Fatalf("write the file: %v", err)
	}

	for name, authority := range map[string]string{
		"a private key": path, "an absent file": filepath.Join(directory, "absent.pem"),
	} {
		lines, err := judgeRegistryMirrors(t.Context(), firecrackerNode(&config.RegistryMirrors{
			DockerIO: "https://mirror.invalid", GHCRIO: "https://mirror.invalid", QuayIO: "https://mirror.invalid",
		}, authority), true)
		if err == nil || !strings.Contains(err.Error(), "registry mirrors") {
			t.Errorf("%s: error = %v", name, err)
		}
		if report := strings.Join(lines, "\n"); !strings.Contains(report, "could not read the mirrors' authority") {
			t.Errorf("%s: report = %q", name, report)
		}
	}
}
