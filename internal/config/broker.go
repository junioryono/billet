package config

import (
	"bytes"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// TokenBrokerConfig is server.token_broker: this deployment takes its installation
// tokens from a broker over mutual TLS, and holds no App key at all.
type TokenBrokerConfig struct {
	// URL is the broker's address, https only, because a client certificate and an
	// installation token cross it.
	URL string `yaml:"url"`
	// CAFile names the PEM bundle the broker's certificate is verified against.
	// Absent, the system's roots.
	CAFile string `yaml:"ca_file,omitempty"`
	// ClientKeyPath is the key this host proves its deployment with, which `billet
	// broker client-key` creates. The broker knows it by fingerprint. It belongs to
	// this host: a standby or a restored host makes and registers its own.
	ClientKeyPath string `yaml:"client_key_path"`
}

// validateTokenBroker checks the block. Whether a target also names a key is
// validateTargetKeyPaths's to say.
func (s *ServerConfig) validateTokenBroker() []error {
	b := s.TokenBroker
	if b == nil {
		return nil
	}

	var errs []error

	u, err := url.Parse(b.URL)

	switch {
	case b.URL == "":
		errs = append(errs, errors.New("server.token_broker.url is required"))
	case err != nil || u.Host == "":
		errs = append(errs, fmt.Errorf("server.token_broker.url %q is not a URL", b.URL))
	case u.Scheme != "https":
		errs = append(errs, fmt.Errorf("server.token_broker.url %q must be https: a client certificate and "+
			"an installation token cross it", b.URL))
	case u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.TrimSuffix(u.Path, "/") != "":
		errs = append(errs, fmt.Errorf("server.token_broker.url %q must name only a scheme and a host", b.URL))
	}

	switch {
	case b.ClientKeyPath == "":
		errs = append(errs, errors.New("server.token_broker.client_key_path is required; "+
			"`billet broker client-key` creates the key there"))
	case !filepath.IsAbs(b.ClientKeyPath):
		errs = append(errs, fmt.Errorf("server.token_broker.client_key_path %q must be absolute", b.ClientKeyPath))
	case inDirectory(b.ClientKeyPath, s.IdentityDir):
		// THE IDENTITY DIRECTORY IS BILLET'S: it creates, renames and deletes files
		// there by name, and a restore replaces what it holds. A key that belongs to
		// this host and travels in no archive has no place in it.
		errs = append(errs, fmt.Errorf("server.token_broker.client_key_path %q is inside identity_dir %s; "+
			"put it outside, because that directory is restored and rotated whole", b.ClientKeyPath, s.IdentityDir))
	}

	if b.CAFile != "" && !filepath.IsAbs(b.CAFile) {
		errs = append(errs, fmt.Errorf("server.token_broker.ca_file %q must be absolute", b.CAFile))
	}

	return errs
}

func inDirectory(path, dir string) bool {
	if dir == "" {
		return false
	}

	rel, err := filepath.Rel(dir, path)

	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// BrokerConfig is the file `billet broker serve` reads. It is the broker's own and
// never a part of billet.yaml: the process holding the App keys holds nothing else,
// and a control plane's config never names one of them.
type BrokerConfig struct {
	// Listen is the address control planes dial, with the certificate they verify.
	Listen   string `yaml:"listen"`
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
	// Apps are the Apps whose keys this broker holds.
	Apps []BrokerApp `yaml:"apps"`
	// Deployments are who may ask, and for what.
	Deployments []BrokerDeployment `yaml:"deployments"`
}

// BrokerApp is one App whose key this broker holds.
type BrokerApp struct {
	AppID int64 `yaml:"app_id"`
	// PrivateKeyPath is the App's key, under the rules billet.yaml's has.
	PrivateKeyPath string `yaml:"private_key_path"`
	// APIURL is the REST base, absent meaning the real GitHub.
	APIURL string `yaml:"api_url,omitempty"`
}

// BrokerDeployment is one deployment this broker serves.
type BrokerDeployment struct {
	// ID is the deployment's identity, as `billet broker client-key` printed it.
	ID string `yaml:"id"`
	// Clients are the fingerprints of the keys that prove this deployment, one per
	// host: an active-passive pair registers two.
	Clients []string `yaml:"clients"`
	// Installations are the only tokens this deployment may ask for.
	Installations []BrokerInstallation `yaml:"installations"`
}

// BrokerInstallation is one installation a deployment may have tokens for.
type BrokerInstallation struct {
	AppID          int64 `yaml:"app_id"`
	InstallationID int64 `yaml:"installation_id"`
	// Target is the GitHub path the deployment's target has: `owner` for an
	// organization, `owner/name` for a repository, whose tokens reach it alone.
	Target string `yaml:"target"`
	// ActionsRead adds `actions: read` for a deployment publishing caches from a
	// default branch, as `tiers[].cache.publish` needs.
	ActionsRead bool `yaml:"actions_read,omitempty"`
}

// LoadBroker reads and validates the broker's file, refusing a key it does not know
// for the reason Parse does: a typo in the file deciding who may mint what should be
// loud.
func LoadBroker(path string) (*BrokerConfig, error) {
	data, err := os.ReadFile(path) //nolint:gosec // the operator names the file, as with billet.yaml
	if err != nil {
		return nil, fmt.Errorf("read broker config %s: %w", path, err)
	}

	cfg, err := ParseBroker(path, data)
	if err != nil {
		return nil, err
	}

	return cfg, nil
}

// ParseBroker is LoadBroker over bytes already read.
func ParseBroker(name string, data []byte) (*BrokerConfig, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)

	var cfg BrokerConfig
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("broker config %s: %w", name, err)
	}

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("broker config %s: %w", name, err)
	}

	return &cfg, nil
}

var (
	brokerDeploymentID = regexp.MustCompile(`^[0-9a-f]{32}$`)
	brokerFingerprint  = regexp.MustCompile(`^SHA256:[A-Za-z0-9+/]{43}$`)
)

// Validate refuses a registry that would hand one deployment's reach to another.
//
// AN INSTALLATION WHOSE WHOLE OWNER A DEPLOYMENT REACHES BELONGS TO THAT DEPLOYMENT
// ALONE. An organization target's token reaches every repository the installation
// does, so registering it beside any other deployment gives that one the first's
// reach, which is what this broker exists to end. Two deployments may share an
// installation only by repository, whose tokens are scoped to the repository.
func (c *BrokerConfig) Validate() error {
	var errs []error

	if c.Listen == "" {
		errs = append(errs, errors.New("listen is required"))
	}
	if c.CertFile == "" || c.KeyFile == "" {
		errs = append(errs, errors.New("cert_file and key_file are required: control planes verify this "+
			"broker before sending it anything"))
	}

	apps := make(map[int64]bool, len(c.Apps))

	for i := range c.Apps {
		app := &c.Apps[i]

		switch {
		case app.AppID <= 0:
			errs = append(errs, fmt.Errorf("apps[%d].app_id must be positive", i))
		case apps[app.AppID]:
			errs = append(errs, fmt.Errorf("apps[%d]: app %d is listed twice", i, app.AppID))
		}

		apps[app.AppID] = true

		if !filepath.IsAbs(app.PrivateKeyPath) {
			errs = append(errs, fmt.Errorf("apps[%d].private_key_path %q must be absolute", i, app.PrivateKeyPath))
		}
	}

	var (
		deployments = make(map[string]bool, len(c.Deployments))
		clients     = map[string]string{}
		// holders is every deployment holding one (app, installation), and whole
		// those of them holding it as an organization.
		holders = map[[2]int64]map[string]bool{}
		whole   = map[[2]int64]bool{}
	)

	for i := range c.Deployments {
		d := &c.Deployments[i]
		where := fmt.Sprintf("deployments[%d]", i)

		switch {
		case !brokerDeploymentID.MatchString(d.ID):
			errs = append(errs, fmt.Errorf("%s.id %q is not a deployment's identity", where, d.ID))
		case deployments[d.ID]:
			errs = append(errs, fmt.Errorf("%s: deployment %s is listed twice", where, d.ID))
		}

		deployments[d.ID] = true

		if len(d.Clients) == 0 {
			errs = append(errs, fmt.Errorf("%s.clients is empty, so nothing could ever prove this deployment", where))
		}

		for _, client := range d.Clients {
			if !brokerFingerprint.MatchString(client) {
				errs = append(errs, fmt.Errorf("%s.clients: %q is not a fingerprint `billet broker client-key` prints",
					where, client))

				continue
			}

			// ONE KEY, ONE DEPLOYMENT: the key is the whole of who is calling.
			if other, dup := clients[client]; dup && other != d.ID {
				errs = append(errs, fmt.Errorf("%s.clients: %s already proves deployment %s", where, client, other))
			}

			clients[client] = d.ID
		}

		seen := map[string]bool{}

		for j := range d.Installations {
			inst := &d.Installations[j]
			at := fmt.Sprintf("%s.installations[%d]", where, j)

			if !apps[inst.AppID] {
				errs = append(errs, fmt.Errorf("%s: app %d is not one this broker holds", at, inst.AppID))
			}
			if inst.InstallationID <= 0 {
				errs = append(errs, fmt.Errorf("%s.installation_id must be positive", at))
			}

			isRepository, err := brokerTargetIsRepository(inst.Target)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", at, err))

				continue
			}

			key := fmt.Sprintf("%d/%d/%s", inst.AppID, inst.InstallationID, strings.ToLower(inst.Target))
			if seen[key] {
				errs = append(errs, fmt.Errorf("%s: this installation and target are listed twice", at))
			}

			seen[key] = true

			id := [2]int64{inst.AppID, inst.InstallationID}
			if holders[id] == nil {
				holders[id] = map[string]bool{}
			}

			holders[id][d.ID] = true

			if !isRepository {
				whole[id] = true
			}
		}
	}

	for id, who := range holders {
		if whole[id] && len(who) > 1 {
			errs = append(errs, fmt.Errorf("installation %d of app %d is registered to %d deployments and to one "+
				"of them as a whole organization, whose token would reach what the others hold; give each a "+
				"repository target, or an installation of its own", id[1], id[0], len(who)))
		}
	}

	return errors.Join(errs...)
}

// brokerTargetIsRepository reads `owner` or `owner/name`.
func brokerTargetIsRepository(target string) (bool, error) {
	if !strings.Contains(target, "/") {
		if target == "" {
			return false, errors.New("target is required: `owner` or `owner/name`")
		}

		return false, nil
	}

	if _, _, ok := SplitRepository(target); !ok {
		return false, fmt.Errorf("target %q is neither `owner` nor `owner/name`", target)
	}

	return true, nil
}
