package ec2

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/provider"
)

// THE LIBRARY IS THE FIRECRACKER AGENT'S TEXT, every region of it, and bash can parse
// the result: an image whose library does not parse would fail every job at its boot.
func TestTheGuestCachesLibraryIsTheAgentsOwnTextAndParses(t *testing.T) {
	t.Parallel()
	library, err := guestCachesLibrary()
	if err != nil {
		t.Fatalf("guestCachesLibrary: %v", err)
	}
	for _, want := range []string{
		"billet_guest_caches_prepare() {", "billet_guest_caches_start() {",
		"billet_guest_caches_relay() {", "billet_guest_caches_env() {",
		// One line from each region of the agent, by what it does.
		`printf '%s results-receiver.actions.githubusercontent.com\n'`,
		"--socket-property=ListenStream",
		`runner_env+=("GOCACHEPROG=$GUEST_BILLET cache gocacheprog")`,
		"--mode node-relay",
	} {
		if !strings.Contains(library, want) {
			t.Errorf("the library does not carry %q", want)
		}
	}

	path := filepath.Join(t.TempDir(), "library.sh")
	writeFile(t, path, library, 0o644)
	if output, err := exec.CommandContext(t.Context(), "bash", "-n", path).CombinedOutput(); err != nil {
		t.Fatalf("the library does not parse: %v\n%s", err, output)
	}
}

// runLibrary sources the library with the given environment and runs a fragment.
func runLibrary(t *testing.T, env map[string]string, fragment string) string {
	t.Helper()
	library, err := guestCachesLibrary()
	if err != nil {
		t.Fatalf("guestCachesLibrary: %v", err)
	}
	path := filepath.Join(t.TempDir(), "library.sh")
	writeFile(t, path, library, 0o644)
	command := exec.CommandContext(t.Context(), "bash", "-c", "set -euo pipefail\n. "+path+"\n"+fragment)
	command.Env = []string{"PATH=" + os.Getenv("PATH")}
	for name, value := range env {
		command.Env = append(command.Env, name+"="+value)
	}
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("the fragment failed: %v\n%s", err, output)
	}

	return string(output)
}

// WHAT THE AGENT ASKS MMDS, ANSWERED FROM WHAT THE BOOT SCRIPT EXPORTED, and a key the
// node did not send fails, as one the service does not hold does: every caller treats
// that as the ordinary case, and an empty answer would read as a value.
func TestFetchAnswersFromTheBootScriptAndFailsOnWhatItDidNotExport(t *testing.T) {
	t.Parallel()
	got := runLibrary(t, map[string]string{
		envActionsProxy: "https://billet:token@10.0.0.1:9443",
		envGuestCaches:  "go,git",
	}, `
fetch actions-proxy
fetch guest-caches
if fetch npm-proxy; then echo "npm-proxy answered"; fi
if fetch actions-ca-pem; then echo "the authority answered"; fi
if fetch something-else; then echo "an unknown key answered"; fi
`)
	if want := "https://billet:token@10.0.0.1:9443\ngo,git\n"; got != want {
		t.Fatalf("fetch answered %q, want %q", got, want)
	}
}

// THE PASSTHROUGHS CANNOT NEST A CLIENT'S TLS IN A SECOND SESSION, so on this image
// they reach the node through the relay, carrying the same credential.
func TestThePassthroughsTunnelThroughTheRelayOrNotAtAll(t *testing.T) {
	t.Parallel()
	const fragment = `
cache_endpoint=$ENDPOINT
actions_proxy=https://billet:s3cret@10.0.0.1:9443
npm_proxy=https://billet:s3cret@10.0.0.1:9443
billet_guest_caches_through_relay
printf 'actions=%s\nnpm=%s\n' "$actions_proxy" "$npm_proxy"
`
	got := runLibrary(t, map[string]string{"ENDPOINT": "http://172.17.0.1:41322"}, fragment)
	want := "actions=http://billet:s3cret@172.17.0.1:41322\nnpm=http://billet:s3cret@172.17.0.1:41322\n"
	if got != want {
		t.Fatalf("through the relay = %q, want %q", got, want)
	}

	// A RELAY THAT DID NOT START LEAVES THE NODE'S OWN ADDRESS, and nothing can tunnel
	// to it, so interception is off: the job uses GitHub's cache.
	got = runLibrary(t, map[string]string{"ENDPOINT": "https://10.0.0.1:9443"}, fragment)
	if !strings.Contains(got, "actions=\nnpm=\n") {
		t.Fatalf("with no relay = %q; interception must be off", got)
	}
}

func TestTheBootScriptCarriesTheGuestCachesAndHoldsThemTogether(t *testing.T) {
	t.Parallel()
	p := &Provider{}
	spec := provider.Spec{
		Name: "billet-lease", JITConfig: "jit", Command: []string{"/usr/local/bin/billet-runner"},
		CacheEndpoint: "https://10.0.0.1:9443", CacheToken: "token", BuildKitCacheMountLimit: 5,
		ActionsProxy: "https://billet:token@10.0.0.1:9443", NpmProxy: "https://billet:token@10.0.0.1:9443",
		ActionsCAPEM: "-----BEGIN CERTIFICATE-----\nabc\n-----END CERTIFICATE-----",
		GuestCaches:  []provider.GuestCache{provider.GuestCacheGo, provider.GuestCacheGit},
	}
	script, err := p.userDataAt(spec, 1)
	if err != nil {
		t.Fatalf("userDataAt: %v", err)
	}

	// EXECUTED, not searched: the PEM spans lines, and the shell is what has to read it.
	printer := filepath.Join(t.TempDir(), "printer")
	writeFile(t, printer, "#!/bin/sh\nprintf '%s|%s|%s|%s\\n' \"$BILLET_ACTIONS_PROXY\" \"$BILLET_NPM_PROXY\" \"$BILLET_GUEST_CACHES\" \"$BILLET_ACTIONS_CA_PEM\"\n", 0o755)
	script = strings.ReplaceAll(script, "'/usr/local/bin/billet-runner'", "'"+printer+"'")
	output, err := exec.CommandContext(t.Context(), "/bin/sh", "-c", script).CombinedOutput()
	if err != nil {
		t.Fatalf("the boot script failed: %v\n%s", err, output)
	}
	want := "https://billet:token@10.0.0.1:9443|https://billet:token@10.0.0.1:9443|go,git|" +
		"-----BEGIN CERTIFICATE-----\nabc\n-----END CERTIFICATE-----\n"
	if string(output) != want {
		t.Fatalf("the boot script exported %q, want %q", output, want)
	}

	for name, broken := range map[string]provider.Spec{
		"a proxy with no authority": {
			Name: "x", CacheEndpoint: "e", CacheToken: "t", ActionsProxy: "p", Command: []string{"c"},
		},
		"an authority with no proxy": {
			Name: "x", CacheEndpoint: "e", CacheToken: "t", ActionsCAPEM: "pem", Command: []string{"c"},
		},
		"a proxy with no session": {
			Name: "x", ActionsProxy: "p", ActionsCAPEM: "pem", Command: []string{"c"},
		},
		"a build cache with no session": {
			Name: "x", GuestCaches: []provider.GuestCache{provider.GuestCacheGo}, Command: []string{"c"},
		},
		"a build cache billet does not know": {
			Name: "x", CacheEndpoint: "e", CacheToken: "t", GuestCaches: []provider.GuestCache{"rust"},
			Command: []string{"c"},
		},
	} {
		if _, err := p.userDataAt(broken, 1); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

// A JOB THAT HAS NO INTERCEPTION CARRIES NO PROXY AND NO AUTHORITY: its boot script
// says nothing about either, which is what keeps an untrusted pool off them.
func TestABootScriptWithoutInterceptionNamesNoProxyAndNoAuthority(t *testing.T) {
	t.Parallel()
	script, err := (&Provider{}).userDataAt(provider.Spec{
		Name: "billet-lease", JITConfig: "jit", Command: []string{"/run"},
		CacheEndpoint: "https://10.0.0.1:9443", CacheToken: "token", BuildKitCacheMountLimit: 5,
	}, 1)
	if err != nil {
		t.Fatalf("userDataAt: %v", err)
	}
	for _, name := range []string{envActionsProxy, envNpmProxy, envActionsCA, envGuestCaches} {
		if strings.Contains(script, name) {
			t.Errorf("the boot script names %s", name)
		}
	}
}

// THE LARGEST REGISTRATION BILLET HAS SEEN, WITH EVERYTHING INTERCEPTION ADDS, FITS
// EC2's 16384 BYTES, whichever form packUserData chooses.
func TestAFullBootScriptFitsInUserData(t *testing.T) {
	t.Parallel()
	// A registration is base64 of incompressible JSON, around 8 KiB in the wild.
	jit := strings.Repeat("abcdefghijklmnopqrstuvwxyz0123456789+/", 8<<10/38)
	pem := "-----BEGIN CERTIFICATE-----\n" + strings.Repeat("MIIBszCCAVmgAwIBAgIUQ7vF9oJc3n2hYkR8mVwZpT5sXlAwCgYIKoZIzj0EAwIw\n", 12) +
		"-----END CERTIFICATE-----\n"
	script, err := (&Provider{}).userDataAt(provider.Spec{
		Name: "billet-0123456789abcdef0123456789abcdef", JITConfig: jit, Command: []string{"/usr/local/bin/billet-runner"},
		CacheEndpoint: "https://10.0.0.1:9443", CacheToken: strings.Repeat("a", 64), BuildKitCacheMountLimit: 5,
		ActionsProxy: "https://billet:" + strings.Repeat("a", 64) + "@10.0.0.1:9443",
		NpmProxy:     "https://billet:" + strings.Repeat("a", 64) + "@10.0.0.1:9443",
		ActionsCAPEM: pem,
		GuestCaches: []provider.GuestCache{
			provider.GuestCacheGo, provider.GuestCacheGoTestResults, provider.GuestCacheBazel, provider.GuestCacheGit,
		},
	}, -9_000_000_000_000_000_000)
	if err != nil {
		t.Fatalf("userDataAt: %v", err)
	}
	packed, err := packUserData(script)
	if err != nil {
		t.Fatalf("packUserData: %v", err)
	}
	t.Logf("the boot script is %d bytes, %d packed, of %d", len(script), len(packed), maxUserData)
}
