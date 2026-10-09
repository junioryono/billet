package config

// GitHubConfig is one GitHub target and the App identity that manages its
// runners: the `github:` block, or one entry under `targets:`.
//
// billet requests exactly two permissions per target: metadata:read and, for an
// organization, organization_self_hosted_runners:read+write, or for a
// repository, the repository permission administration:write, which is the only
// permission GitHub offers for registering a repository's runners. It
// deliberately does not request actions:read, which would expose workflow runs,
// logs, and artifacts.
type GitHubConfig struct {
	// Name is the target's name under `targets:`, what a tier's `target` names.
	// Refused under `github:`, whose name is DefaultTargetName.
	Name string `yaml:"name,omitempty"`
	// Org is the organization this target is. Exactly one of Org and Repository.
	Org string `yaml:"org,omitempty"`
	// Repository is the repository this target is, as owner/name. Its runners
	// belong to the repository alone: a repository has no runner groups, so a
	// tier under it is untrusted only.
	Repository string `yaml:"repository,omitempty"`
	AppID      int64  `yaml:"app_id"`
	// ClientID is the App's OAuth client identifier, and it is OPTIONAL.
	//
	// GitHub's newer guidance prefers it over the numeric app id as the JWT
	// issuer, and the scale-set client accepts either — its GitHubAppAuth
	// documents ClientID as "the Client ID of the application (app id also
	// works)". So this must never become required: every config written before
	// the field existed keeps working.
	//
	// It is recorded because the manifest conversion already returns it, and
	// throwing away a value GitHub handed over means a second trip through the
	// browser to get it back. It is an identifier, not a secret — App.Forget
	// deliberately keeps it while blanking the client SECRET beside it.
	ClientID       string `yaml:"client_id,omitempty"`
	InstallationID int64  `yaml:"installation_id"`
	// PrivateKeyPath points at the App private key PEM. This file is the single
	// most sensitive thing in a billet deployment: it lives only on the control
	// plane, and nodes never hold long-lived GitHub credentials.
	PrivateKeyPath string `yaml:"private_key_path"`
	// MaxVCPU and MaxMemory are this target's share: the most its tiers may hold
	// at once between them, inside the deployment ceiling. Zero leaves that
	// dimension to the deployment ceiling alone. A share is what keeps one
	// target's burst from taking every host another target's jobs need.
	MaxVCPU   int      `yaml:"max_vcpu,omitempty"`
	MaxMemory ByteSize `yaml:"max_memory,omitempty"`
}
