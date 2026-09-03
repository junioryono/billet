package initconfig

import (
	"encoding/xml"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/junioryono/billet/deploy"
	"github.com/junioryono/billet/internal/config"
)

// asPlatform pins which service shape the generator produces, so BOTH can be
// asserted wherever the tests run.
//
// THE SERVICE PROFILE IS PLATFORM-DERIVED, and without this every test below
// would assert whichever shape the machine running it happens to have — so the
// Linux pin test would compare a systemd unit against macOS paths on a Mac, and
// the macOS one would never run in CI at all.
func asPlatform(t *testing.T, goos string) {
	t.Helper()

	prev := serviceOS
	serviceOS = goos

	t.Cleanup(func() { serviceOS = prev })
}

// mustLoad fills the App ids the way `github-app create` would and loads the
// generated body through the config package's own path.
func mustLoad(t *testing.T, body string) *config.Config {
	t.Helper()

	filled := strings.Replace(body, "app_id: 0", "app_id: 1", 1)
	filled = strings.Replace(filled, "installation_id: 0", "installation_id: 1", 1)
	cfg, err := config.Parse("generated", []byte(filled))
	if err != nil {
		t.Fatalf("the generated config does not load: %v\n\n%s", err, body)
	}

	return cfg
}

func serviceParams() Params {
	p := dockerParams()
	p.Profile = ProfileLocalService

	return p
}

// THE SERVICE SHAPE'S PATHS STAND OR FALL TOGETHER: config and key under
// /etc/billet, state under /var/lib/billet, the lock under the tmpfs directory
// the packaged node unit creates. A single stray per-user path makes a config
// the units cannot run (ProtectHome=true).
func TestGenerateLocalServicePathsAreServiceShaped(t *testing.T) {
	asPlatform(t, "linux")

	body, _, err := Generate(serviceParams())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	cfg := mustLoad(t, body)

	for name, got := range map[string]struct{ got, want string }{
		"server.state_dir":        {cfg.Server.IdentityDir, "/var/lib/billet/server"},
		"node.state_dir":          {cfg.Node.StateDir, "/var/lib/billet/node"},
		"node.lock_dir":           {cfg.Node.LockDir, "/run/billet/locks"},
		"github.private_key_path": {cfg.GitHub.PrivateKeyPath, "/etc/billet/app-private-key.pem"},
	} {
		if got.got != got.want {
			t.Errorf("%s = %q, want %q", name, got.got, got.want)
		}
	}

	if home, err := os.UserHomeDir(); err == nil && home != "" && home != "/" {
		if strings.Contains(body, home) {
			t.Errorf("a local-service config references the home directory, which "+
				"ProtectHome=true makes unreadable:\n%s", body)
		}
	}
}

// THE SERVICE SHAPE IS PINNED TO THE PACKAGED UNITS THEMSELVES, not to a copy
// of their values: the paths here are read out of deploy/*.service, so moving a
// StateDirectory or the RuntimeDirectory breaks this test rather than shipping
// units and generator that disagree.
func TestGenerateLocalServiceMatchesThePackagedUnits(t *testing.T) {
	asPlatform(t, "linux")

	// THE EMBEDDED COPY, not a relative path: these are the exact bytes the
	// package installs and the exact bytes billet compares a host's units
	// against, so a test reading a third spelling of them could pass while the
	// two that matter disagreed.
	serverUnit, nodeUnit := deploy.ServerUnit, deploy.NodeUnit

	body, _, err := Generate(serviceParams())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	cfg := mustLoad(t, body)

	// systemd's StateDirectory/RuntimeDirectory are relative to /var/lib and /run.
	if want := "/var/lib/" + unitValue(t, serverUnit, "StateDirectory"); cfg.Server.IdentityDir != want {
		t.Errorf("server.state_dir %q does not match the server unit's StateDirectory (%q)",
			cfg.Server.IdentityDir, want)
	}
	if want := "/var/lib/" + unitValue(t, nodeUnit, "StateDirectory"); cfg.Node.StateDir != want {
		t.Errorf("node.state_dir %q does not match the node unit's StateDirectory (%q)",
			cfg.Node.StateDir, want)
	}
	if want := "/run/" + unitValue(t, nodeUnit, "RuntimeDirectory"); cfg.Node.LockDir != want {
		t.Errorf("node.lock_dir %q does not match the node unit's RuntimeDirectory (%q)",
			cfg.Node.LockDir, want)
	}

	// Both units read the config from the path their ExecStart names — parsed
	// as arguments, not a substring, which "billet.yaml.old" or a second
	// --config later in the line would satisfy. Exactly one --config, valued
	// exactly the canonical path.
	for name, unit := range map[string]string{"server": serverUnit, "node": nodeUnit} {
		args := strings.Fields(unitValue(t, unit, "ExecStart"))
		var configs []string
		for i, a := range args {
			switch {
			case a == "--config":
				// A dangling --config is a broken unit, not a non-match.
				if i+1 >= len(args) {
					t.Fatalf("the %s unit's ExecStart ends with a dangling --config", name)
				}
				configs = append(configs, args[i+1])
			case strings.HasPrefix(a, "--config="):
				configs = append(configs, strings.TrimPrefix(a, "--config="))
			}
		}
		if len(configs) != 1 || configs[0] != ServiceConfigPath() {
			t.Errorf("the %s unit's ExecStart reads --config %v, want exactly [%s]",
				name, configs, ServiceConfigPath())
		}
	}
	if dir := filepath.Dir(cfg.GitHub.PrivateKeyPath); dir != unitValue(t, serverUnit, "ReadOnlyPaths") {
		t.Errorf("the App key directory %q is not the units' read-only config directory", dir)
	}

	// The IDENTITY the whole permission story rests on: serviceOwnership chowns
	// to ServiceGroup because the server unit reads as that group. If the unit
	// changes its account, this — not a shipped config nobody can read — is
	// what fails.
	if got := unitValue(t, serverUnit, "Group"); got != ServiceGroup {
		t.Errorf("the server unit runs as group %q while the generator arranges group %q", got, ServiceGroup)
	}
	if got := unitValue(t, serverUnit, "User"); got != ServiceGroup {
		t.Errorf("the server unit runs as user %q, want %q", got, ServiceGroup)
	}
	if got := unitValue(t, nodeUnit, "User"); got != "root" {
		t.Errorf("the node unit runs as user %q; the node is a privileged host agent", got)
	}

	// The packaged config template carries the same shape; drifting from the
	// generator would ship two disagreeing sources of the service paths.
	packaged := readUnit(t, "../../deploy/billet.yaml")
	for _, want := range []string{
		"state_dir: " + cfg.Server.IdentityDir,
		"private_key_path: " + cfg.GitHub.PrivateKeyPath,
	} {
		if !strings.Contains(packaged, want) {
			t.Errorf("deploy/billet.yaml does not carry %q, which the generator emits", want)
		}
	}
}

// readUnit loads a packaged unit file, from the repo layout this test runs in.
func readUnit(t *testing.T, path string) string {
	t.Helper()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	return string(raw)
}

// unitValue extracts a directive's value from unit-file text. Directives are
// unique in these units; a duplicate would make the pin ambiguous, so it fails.
func unitValue(t *testing.T, unit, directive string) string {
	t.Helper()

	m := regexp.MustCompile(`(?m)^`+directive+`=(.+)$`).FindAllStringSubmatch(unit, -1)
	if len(m) != 1 {
		t.Fatalf("directive %s appears %d times in the unit, want exactly once", directive, len(m))
	}

	return strings.TrimSpace(m[0][1])
}

// ONE LISTEN VALUE FEEDS BOTH ENDS: the server binds it and the node dials it.
// Rendering them from two places is how they drift and the node dials a
// listener that does not exist.
func TestGenerateListenIsUsedAtBothEnds(t *testing.T) {
	p := dockerParams()
	p.Listen = "127.0.0.1:7900"

	body, _, err := Generate(p)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	cfg := mustLoad(t, body)

	if cfg.Server.Listen != "127.0.0.1:7900" {
		t.Errorf("server.listen = %q, want the given listen address", cfg.Server.Listen)
	}
	if cfg.Node.ServerAddr != cfg.Server.Listen {
		t.Errorf("node.server_addr %q does not dial server.listen %q",
			cfg.Node.ServerAddr, cfg.Server.Listen)
	}
}

// A NON-LOOPBACK LISTEN IS REFUSED BY THE FLAG THAT CARRIED IT. The local
// profiles' guarantee is that nothing is exposed to the network; letting the
// value through would instead fail config self-validation with an error about
// node.tls that never mentions --listen.
func TestGenerateRefusesANonLoopbackListen(t *testing.T) {
	for _, listen := range []string{
		"0.0.0.0:7717", ":7717", "10.0.0.5:7717", "billet.example:7717",
		// Bad PORTS are --listen's to refuse too, or they surface later as
		// "the generated config is not valid" with no flag named.
		"127.0.0.1:0", "127.0.0.1:99999", "127.0.0.1:http", "127.0.0.1:",
	} {
		p := dockerParams()
		p.Listen = listen

		if _, _, err := Generate(p); err == nil {
			t.Errorf("a non-loopback listen %q was not refused", listen)
		} else if !strings.Contains(err.Error(), "--listen") {
			t.Errorf("the refusal for %q does not name --listen: %v", listen, err)
		}
	}
}

func TestGenerateRefusesAnUnknownProfile(t *testing.T) {
	p := dockerParams()
	p.Profile = "cloud"

	if _, _, err := Generate(p); err == nil {
		t.Error("an unknown profile was not refused")
	} else if !strings.Contains(err.Error(), "--profile") {
		t.Errorf("the refusal does not name --profile: %v", err)
	}
}

// THE DEFAULT PROFILE IS THE USER-SESSION SHAPE IT ALWAYS WAS: per-user paths,
// no lock_dir (the per-user default is correct there), default loopback listen.
func TestGenerateLocalKeepsTheUserSessionShape(t *testing.T) {
	body, _, err := Generate(dockerParams())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	cfg := mustLoad(t, body)

	if cfg.Node.LockDir != "" {
		t.Errorf("the user-session shape sets node.lock_dir %q; the per-user default is correct there",
			cfg.Node.LockDir)
	}
	if cfg.Server.Listen != DefaultListen {
		t.Errorf("server.listen = %q, want %s", cfg.Server.Listen, DefaultListen)
	}
	if base := stateDirBase(); !strings.HasPrefix(cfg.Server.IdentityDir, base) {
		t.Errorf("server.state_dir %q is not under the user-session base %q", cfg.Server.IdentityDir, base)
	}
}

// THE EC2 RENDER FOLLOWS THE PROFILE TOO — an orchestrator run as the packaged
// services needs the same path shape as any other local-service host.
func TestGenerateEC2LocalServiceLoads(t *testing.T) {
	asPlatform(t, "linux")

	p := Params{
		Org:      "acme",
		Provider: config.ProviderEC2,
		VCPU:     16,
		Memory:   64 * config.GiB,
		Profile:  ProfileLocalService,
		EC2: &EC2Params{
			Region:                  "us-east-1",
			SubnetID:                "subnet-0e0e0e0e0e0e0e0e0",
			SecurityGroups:          []string{"sg-0a0a0a0a0a0a0a0a0"},
			UntrustedSecurityGroups: []string{"sg-0f0f0f0f0f0f0f0f0"},
			Shapes: []config.EC2InstanceType{{
				Type: "c7i.2xlarge", VCPU: 8, Memory: 16 * config.GiB,
				PriceUSDPerHour: 340000,
			}},
		},
	}

	body, _, err := Generate(p)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	cfg := mustLoad(t, body)

	if cfg.Server.IdentityDir != "/var/lib/billet/server" || cfg.Node.LockDir != "/run/billet/locks" {
		t.Errorf("the ec2 local-service render is not service-shaped: state %q lock %q",
			cfg.Server.IdentityDir, cfg.Node.LockDir)
	}

	// AND ON A MAC IT DESCRIBES A MAC.
	//
	// An ec2 node is an ORCHESTRATOR — it calls an API and the compute appears in
	// a region — so it is the one backend a Mac can run without any of the
	// hardware the others need, and `--provider ec2 --profile local-service` on a
	// Mac is an ordinary invocation. This renderer kept its own copy of the lock
	// explanation after the docker one was made platform-derived, so it went on
	// calling /usr/local/var/run/billet/locks a tmpfs directory a systemd
	// RuntimeDirectory creates per boot — in a file whose own header says the
	// services are launch agents.
	t.Run("darwin", func(t *testing.T) {
		asPlatform(t, "darwin")

		body, _, err := Generate(p)
		if err != nil {
			t.Fatalf("Generate: %v", err)
		}

		cfg := mustLoad(t, body)
		if cfg.Node.LockDir != macServiceLockDir {
			t.Errorf("node.lock_dir is %q, want the macOS %q", cfg.Node.LockDir, macServiceLockDir)
		}

		for _, absent := range []string{"systemd", "systemctl", "RuntimeDirectory"} {
			if strings.Contains(body, absent) {
				t.Errorf("the macOS ec2 config explains itself with %q:\n%s", absent, body)
			}
		}
	})
}

// AND THE macOS SHAPE IS PINNED TO THE LAUNCH AGENTS, the same way.
//
// The agents are the only authority for two of these paths: launchd performs no
// variable substitution, so what a plist names is a literal, and a generated
// config that disagrees produces two services reading different files with
// nothing to say so. The rest of the family has no plist to be pinned to — a
// launch agent declares no StateDirectory the way a systemd unit does — so what
// this asserts about those is that they are consistent with each other and with
// the root the agents already use.
func TestGenerateLocalServiceMatchesTheLaunchAgents(t *testing.T) {
	asPlatform(t, "darwin")

	// THE EMBEDDED COPIES, which are the exact bytes an operator installs.
	serverAgent, nodeAgent := deploy.ServerAgent, deploy.NodeAgent

	body, _, err := Generate(serviceParams())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	cfg := mustLoad(t, body)

	// EXACTLY ONE --config PER AGENT, valued exactly the canonical path. Parsed
	// out of ProgramArguments rather than matched as a substring: these plists
	// are mostly comments, and the path appears in the prose of both.
	for name, agent := range map[string]string{"server": serverAgent, "node": nodeAgent} {
		got := agentConfigArgs(t, name, agent)

		if len(got) != 1 || got[0] != ServiceConfigPath() {
			t.Errorf("the %s agent reads --config %v, want exactly [%s]",
				name, got, ServiceConfigPath())
		}
	}

	// THE LOG DIRECTORY THE AGENTS NAME IS UNDER THE SAME ROOT as everything the
	// profile writes, so an operator makes one directory tree rather than two in
	// different places. launchd creates neither.
	//
	// READ OUT OF THE PLISTS, because that is the only place it exists: no config
	// field names it, and billet's launchd backend carries its own constant. An
	// earlier version of this test had this comment over a loop that checked
	// everything EXCEPT the log directory — and the log directory going unchecked
	// is how a `logDir` that was never initialised reached a real Mac.
	for name, agent := range map[string]string{
		deploy.ServerAgentName: deploy.ServerAgent,
		deploy.NodeAgentName:   deploy.NodeAgent,
	} {
		for _, key := range []string{"StandardOutPath", "StandardErrorPath"} {
			path := agentStringValue(t, name, agent, key)

			if !strings.HasPrefix(path, "/usr/local/var/log/billet/") {
				t.Errorf("%s writes %s to %q, which is not under the log directory the rest of "+
					"the profile assumes", name, key, path)
			}
		}
	}

	root := "/usr/local/"
	for _, path := range []string{
		cfg.Server.IdentityDir, cfg.Node.StateDir, cfg.Node.LockDir, cfg.GitHub.PrivateKeyPath,
		ServiceConfigPath(),
	} {
		if !strings.HasPrefix(path, root) {
			t.Errorf("%q is not under %s, where the launch agents put everything else", path, root)
		}
	}

	// AND NOT UNDER A HOME DIRECTORY, which is the constraint that decided the
	// root: a plist is a shipped constant billet compares against, and launchd
	// would not expand a ~ or a $HOME in one.
	if home, err := os.UserHomeDir(); err == nil && home != "/" {
		if strings.Contains(body, home) {
			t.Errorf("a macOS service config references the home directory, which launchd "+
				"cannot expand in a plist:\n%s", body)
		}
	}

	// THE SERVER AND NODE KEEP SEPARATE STATE, which is the invariant the whole
	// profile exists to preserve on either platform.
	if cfg.Server.IdentityDir == cfg.Node.StateDir {
		t.Errorf("the server and node share a state directory (%s)", cfg.Server.IdentityDir)
	}

	// NO SERVICE ACCOUNT. A launch agent runs as the operator, so there is
	// nothing for the generator's ownership guidance to chown to — and prose
	// that told somebody to chown to a `billet` account that does not exist is
	// how the macOS path would fail on its first instruction.
	if got := ServiceAccount(); got != "" {
		t.Errorf("ServiceAccount() = %q on macOS, want empty: launch agents run as the "+
			"operator and there is no such account", got)
	}
}

// agentConfigArgs reads the --config values out of a plist's ProgramArguments.
func agentConfigArgs(t *testing.T, name, agent string) []string {
	t.Helper()

	args := agentProgramArguments(t, name, agent)

	var configs []string

	for i, a := range args {
		switch {
		case a == "--config":
			if i+1 >= len(args) {
				t.Fatalf("the %s agent's ProgramArguments end with a dangling --config", name)
			}

			configs = append(configs, args[i+1])
		case strings.HasPrefix(a, "--config="):
			configs = append(configs, strings.TrimPrefix(a, "--config="))
		}
	}

	return configs
}

// agentProgramArguments reads a plist's top-level ProgramArguments array by
// PARSING rather than by searching.
//
// These files are mostly comments, because the reasoning is the point of them,
// and every path they name appears in that prose as well as in the array that
// matters. A substring search is satisfied by the explanation — so it would pass
// with the real ProgramArguments deleted, which is precisely the shape of test
// this repo has been bitten by before.
func agentProgramArguments(t *testing.T, name, body string) []string {
	t.Helper()

	decoder := xml.NewDecoder(strings.NewReader(body))

	var (
		depth   int
		inKey   bool
		keyText string
		found   bool
		args    []string
	)

	for {
		tok, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}

		if err != nil {
			t.Fatalf("the %s agent is not well-formed XML: %v", name, err)
		}

		switch el := tok.(type) {
		case xml.StartElement:
			switch {
			case el.Name.Local == "dict":
				depth++

			case depth != 1:
				// Nested deeper than the top-level settings.

			case found && args == nil:
				if el.Name.Local != "array" {
					t.Fatalf("the %s agent declares ProgramArguments as <%s>, want <array>",
						name, el.Name.Local)
				}

				var content struct {
					Items []string `xml:"string"`
				}

				if err := decoder.DecodeElement(&content, &el); err != nil {
					t.Fatalf("the %s agent's ProgramArguments cannot be read: %v", name, err)
				}

				args = content.Items
				if args == nil {
					// An empty array is a real answer; nil would send this back
					// round looking for the NEXT key's value.
					args = []string{}
				}

			case el.Name.Local == "key":
				inKey = true
				keyText = ""
			}

		case xml.CharData:
			if inKey {
				keyText += string(el)
			}

		case xml.EndElement:
			switch {
			case el.Name.Local == "dict":
				depth--

			case el.Name.Local == "key" && inKey:
				inKey = false

				if depth == 1 && strings.TrimSpace(keyText) == "ProgramArguments" {
					if found {
						t.Fatalf("the %s agent declares ProgramArguments more than once; "+
							"which one launchd honours is not for this test to decide", name)
					}

					found = true
				}
			}
		}
	}

	if !found {
		t.Fatalf("the %s agent declares no ProgramArguments, so it runs nothing", name)
	}

	return args
}

// agentStringValue reads one top-level <string> setting out of a plist.
//
// PARSED, for the reason agentProgramArguments is: these files are mostly
// comments and every path in them appears in that prose too, so a search is
// satisfied by the explanation.
func agentStringValue(t *testing.T, name, body, key string) string {
	t.Helper()

	decoder := xml.NewDecoder(strings.NewReader(body))

	var (
		depth   int
		inKey   bool
		keyText string
		found   bool
	)

	for {
		tok, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}

		if err != nil {
			t.Fatalf("%s is not well-formed XML: %v", name, err)
		}

		switch el := tok.(type) {
		case xml.StartElement:
			switch {
			case el.Name.Local == "dict":
				depth++

			case depth != 1:
				// Nested deeper than the top-level settings.

			case found:
				var content struct {
					Text string `xml:",chardata"`
				}

				if err := decoder.DecodeElement(&content, &el); err != nil {
					t.Fatalf("%s: cannot read %s: %v", name, key, err)
				}

				return strings.TrimSpace(content.Text)

			case el.Name.Local == "key":
				inKey = true
				keyText = ""
			}

		case xml.CharData:
			if inKey {
				keyText += string(el)
			}

		case xml.EndElement:
			switch {
			case el.Name.Local == "dict":
				depth--

			case el.Name.Local == "key" && inKey:
				inKey = false

				if depth == 1 && strings.TrimSpace(keyText) == key {
					found = true
				}
			}
		}
	}

	t.Fatalf("%s declares no %s", name, key)

	return ""
}
