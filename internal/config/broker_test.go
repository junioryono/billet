package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	brokerClientA = "SHA256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	brokerClientB = "SHA256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	brokerA       = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	brokerB       = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func brokerFile() string {
	return `listen: :8443
cert_file: /etc/billet/broker.crt
key_file: /etc/billet/broker.key
apps:
  - app_id: 11
    private_key_path: /etc/billet/app-11.pem
deployments:
  - id: ` + brokerA + `
    clients: ["` + brokerClientA + `"]
    installations:
      - {app_id: 11, installation_id: 22, target: acme}
  - id: ` + brokerB + `
    clients: ["` + brokerClientB + `"]
    installations:
      - {app_id: 11, installation_id: 33, target: other/api}
      - {app_id: 11, installation_id: 33, target: other/web}
`
}

func TestABrokerFileLoads(t *testing.T) {
	t.Parallel()

	cfg, err := ParseBroker("broker.yaml", []byte(brokerFile()))
	if err != nil {
		t.Fatalf("ParseBroker: %v", err)
	}

	if len(cfg.Apps) != 1 || len(cfg.Deployments) != 2 || cfg.Deployments[1].Installations[1].Target != "other/web" {
		t.Fatalf("the file read as %+v", cfg)
	}

	path := filepath.Join(t.TempDir(), "broker.yaml")
	if err := os.WriteFile(path, []byte(brokerFile()), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	if _, err := LoadBroker(path); err != nil {
		t.Fatalf("LoadBroker: %v", err)
	}
}

// EACH IS A REGISTRY THAT WOULD HAND A DEPLOYMENT SOMETHING NOT ITS OWN, or could never
// answer anyone, and a mistake in this file is the expensive kind.
func TestABrokerFileRefusesWhatItCannotHonour(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct{ old, new, want string }{
		"a key it does not know": {"listen: :8443", "listen: :8443\nextra: true", "extra"},
		"no listen":              {"listen: :8443\n", "", "listen is required"},
		"no certificate":         {"cert_file: /etc/billet/broker.crt\n", "", "cert_file and key_file are required"},
		"an app with no id":      {"- app_id: 11\n    private_key_path", "- app_id: 0\n    private_key_path", "must be positive"},
		"a relative app key": {
			"private_key_path: /etc/billet/app-11.pem", "private_key_path: app-11.pem", "must be absolute",
		},
		"not a deployment's identity": {"- id: " + brokerA, "- id: zzz", "is not a deployment's identity"},
		"a deployment twice":          {"- id: " + brokerB, "- id: " + brokerA, "listed twice"},
		"not a fingerprint":           {brokerClientA, "sha256:nope", "is not a fingerprint"},
		"one key for two deployments": {brokerClientB, brokerClientA, "already proves deployment"},
		"no clients":                  {`clients: ["` + brokerClientB + `"]`, "clients: []", "nothing could ever prove"},
		"an app it does not hold":     {"{app_id: 11, installation_id: 22", "{app_id: 12, installation_id: 22", "not one this broker holds"},
		"no installation id":          {"installation_id: 22", "installation_id: 0", "must be positive"},
		"a bad target":                {"target: other/web", "target: a/b/c", "neither"},
		"no target":                   {", target: acme}", "}", "target is required"},
		"the same one twice":          {"target: other/web", "target: other/api", "listed twice"},
		"one repository, two deployments, any case": {
			"{app_id: 11, installation_id: 22, target: acme}", "{app_id: 11, installation_id: 33, target: OTHER/API}",
			"already registered to deployment",
		},

		// THE RULE THE BROKER EXISTS FOR. An organization's token reaches every
		// repository the installation does, so nobody else may hold that installation.
		"an organization's installation shared": {
			"installation_id: 33, target: other/api", "installation_id: 22, target: acme/api", "whole organization",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if !strings.Contains(brokerFile(), tc.old) {
				t.Fatalf("the base file has no %q, so this case changed nothing", tc.old)
			}

			_, err := ParseBroker("broker.yaml", []byte(strings.Replace(brokerFile(), tc.old, tc.new, 1)))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want it to say %q", err, tc.want)
			}
		})
	}
}

// TWO DEPLOYMENTS MAY SHARE AN INSTALLATION BY REPOSITORY, whose tokens reach only
// their own, and that is the shape the rule above leaves open.
func TestTwoDeploymentsMayShareAnInstallationByRepository(t *testing.T) {
	t.Parallel()

	body := strings.Replace(brokerFile(), "{app_id: 11, installation_id: 22, target: acme}",
		"{app_id: 11, installation_id: 33, target: other/billing}", 1)

	if _, err := ParseBroker("broker.yaml", []byte(body)); err != nil {
		t.Fatalf("two deployments on one installation by repository: %v", err)
	}
}

// ONE REPOSITORY IS ONE REPOSITORY, whichever App reaches it: a second App installed on the
// same owner does not make what it reaches anyone else's.
func TestARegistryComparesWhatATokenReachesAcrossApps(t *testing.T) {
	t.Parallel()

	const (
		a = "0123456789abcdef0123456789abcdef"
		b = "fedcba9876543210fedcba9876543210"
	)

	fingerprint := func(c byte) string { return "SHA256:" + strings.Repeat(string(c), 43) }

	load := func(t *testing.T, first, second BrokerInstallation) error {
		t.Helper()

		dir := t.TempDir()
		cfg := &BrokerConfig{
			Listen: "127.0.0.1:8443", CertFile: filepath.Join(dir, "c"), KeyFile: filepath.Join(dir, "k"),
			Apps: []BrokerApp{
				{AppID: 1, PrivateKeyPath: filepath.Join(dir, "a1")},
				{AppID: 2, PrivateKeyPath: filepath.Join(dir, "a2")},
			},
			Deployments: []BrokerDeployment{
				{ID: a, Clients: []string{fingerprint('A')}, Installations: []BrokerInstallation{first}},
				{ID: b, Clients: []string{fingerprint('B')}, Installations: []BrokerInstallation{second}},
			},
		}

		return cfg.Validate()
	}

	for name, tc := range map[string]struct {
		first, second BrokerInstallation
		want          string
	}{
		"a repository under two Apps": {
			BrokerInstallation{AppID: 1, InstallationID: 10, Target: "acme/api"},
			BrokerInstallation{AppID: 2, InstallationID: 20, Target: "ACME/api"}, "already registered",
		},
		"an organization, and a repository of it under another App": {
			BrokerInstallation{AppID: 1, InstallationID: 10, Target: "acme"},
			BrokerInstallation{AppID: 2, InstallationID: 20, Target: "acme/api"}, "whole organization",
		},
		"two repositories of one owner": {
			BrokerInstallation{AppID: 1, InstallationID: 10, Target: "acme/api"},
			BrokerInstallation{AppID: 1, InstallationID: 10, Target: "acme/web"}, "",
		},
		"two owners": {
			BrokerInstallation{AppID: 1, InstallationID: 10, Target: "acme"},
			BrokerInstallation{AppID: 2, InstallationID: 20, Target: "other"}, "",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			err := load(t, tc.first, tc.second)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}

				return
			}

			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want it to say %q", err, tc.want)
			}
		})
	}
}
