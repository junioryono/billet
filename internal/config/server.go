package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// StateBackend names the engine the control-plane ledger lives in.
type StateBackend string

const (
	// StateSQLite is the default and the recommended shape for a laptop, one
	// owned server, or the small controller ADR-001 describes. It is explicitly
	// SINGLE CONTROLLER: the exclusive lock on the state directory is what stops
	// a second one, and there is no shared-storage story, because SQLite's WAL
	// cannot work on a network filesystem.
	StateSQLite StateBackend = "sqlite"

	// StatePostgres puts the ledger in a database billet does not operate, which
	// is what makes the controller replaceable — the scheduling state outlives
	// the machine, so recovery is a managed backup rather than a directory.
	//
	// IT IS NOT HIGH AVAILABILITY ON ITS OWN. Exactly one controller may make
	// scheduling decisions either way, and a database's ability to serialize
	// writes is not proof that only one process is polling GitHub.
	StatePostgres StateBackend = "postgres"
)

// IdentityBackend names where a deployment's identity material lives.
type IdentityBackend string

const (
	// IdentityFile is the default and what every deployment has today: the
	// node-wire authority and the GitHub App private key are files in
	// identity_dir. It is the right answer for one controller, and it is the only
	// answer for a deployment with no AWS account.
	IdentityFile IdentityBackend = "file"

	// IdentitySSM puts them in AWS Systems Manager Parameter Store as
	// SecureStrings, so an active/passive pair shares one authority instead of
	// two copies somebody has to keep in step.
	//
	// PARAMETER STORE RATHER THAN SECRETS MANAGER, and the deciding fact is a
	// deletion: DeleteParameter is immediate where DeleteSecret imposes a
	// seven-day recovery window unless forced. billet already speaks this service,
	// with signing vectors in the tree, so it is one client rather than two.
	IdentitySSM IdentityBackend = "aws-ssm"
)

// IdentityConfig is where the deployment's identity material lives.
//
// SEPARATE FROM `state:` BECAUSE THE TWO ARE NOT INTERCHANGEABLE, which is the
// same sentence identity_dir already carries. A ledger is rows and can move into
// a database; a private key cannot follow it there, which is why the pairing is
// a refusal.
type IdentityConfig struct {
	Backend IdentityBackend `yaml:"backend"`

	// AWSSSM configures the store when Backend selects it, and is REFUSED
	// otherwise rather than ignored — the same rule the `state:` block follows,
	// and for the same reason: silently ignoring a block produces a deployment
	// that believes it configured something.
	AWSSSM *IdentitySSMConfig `yaml:"aws_ssm,omitempty"`
}

// IdentitySSMConfig names the Parameter Store path this deployment's identity
// lives under.
type IdentitySSMConfig struct {
	// Region is the SIGNING region, and it also selects the endpoint: there is no
	// override, because an override is a way to send a deployment's private key to
	// a host of somebody's choosing.
	Region string `yaml:"region"`

	// Prefix isolates one deployment inside an account. Everything billet stores
	// lands under it, so IAM can be scoped by path and two deployments cannot read
	// each other's authority.
	Prefix string `yaml:"prefix"`

	// KMSKeyID names the key that encrypts the SecureStrings. Empty uses the
	// account's default SSM key, which is what a deployment that has not chosen
	// one gets — and which is a real choice rather than an omission, because that
	// key's policy is what decides who else in the account can read them.
	KMSKeyID string `yaml:"kms_key_id,omitempty"`
}

// ControllerMode says how many control planes this deployment runs, and
// therefore what a controller does when it finds the claim already held.
type ControllerMode string

const (
	// ControllersSingle is the default and what every deployment has today: one
	// control plane, and a second one is a MISTAKE that says so loudly. It exits
	// non-zero naming the machine that holds the claim, and `Restart=on-failure`
	// repeats that refusal every RestartSec until somebody fixes it.
	ControllersSingle ControllerMode = "single"

	// ControllersActivePassive says this deployment runs more than one control
	// plane on purpose. Whichever takes the claim first is the controller; the
	// others WAIT, and one of them takes over when the incumbent's database
	// session ends.
	//
	// BOTH HOSTS WRITE THE SAME VALUE, and that symmetry is the reason it is a
	// property of the deployment rather than a flag on one process. After a
	// failover the standby IS the controller, so a per-process spelling would
	// leave a file describing a role its host no longer has.
	//
	// IT IS NOT AUTOMATIC, and the diagnostic is why. If waiting were what every
	// refused controller did, two machines misconfigured as active would stop
	// being a loud restart loop and become a deployment that looks healthy and
	// has quietly halved itself.
	ControllersActivePassive ControllerMode = "active-passive"
)

// StateConfig is the versioned form of "where does the ledger live".
type StateConfig struct {
	Backend StateBackend `yaml:"backend"`

	// Postgres configures the ledger when Backend selects it, and is REFUSED
	// otherwise rather than ignored — the same way a `node.ceph` block on a
	// non-firecracker backend is, and for the same reason: silently ignoring it
	// produces a deployment that believes it configured something.
	//
	// THERE IS NO `sqlite:` BLOCK, and its absence is deliberate. The only thing
	// it could carry is a path, the SQLite ledger is always billet.db inside
	// identity_dir, and every part of billet that reads a ledger FILE — the
	// restore planner, the writer barrier, the archive — derives it that way. A
	// key that billet accepted and then did not use would be a deployment
	// started against a freshly created empty ledger, which is not a failure
	// anybody would see until the fleet came back empty.
	Postgres *PostgresStateConfig `yaml:"postgres,omitempty"`
}

// PostgresStateConfig is the ledger in PostgreSQL.
type PostgresStateConfig struct {
	// DSNEnv names the ENVIRONMENT VARIABLE holding the connection string, and
	// the indirection is the point: a DSN carries a password, and a secret
	// written into YAML ends up in a backup, a paste buffer, and eventually a
	// support thread. It is the same rule the GitHub App private key follows.
	DSNEnv string `yaml:"dsn_env"`
}

// ServerConfig configures the control plane.
type ServerConfig struct {
	// Listen is the address nodes dial. Nodes always connect outbound, so on a
	// single-box deployment this stays on loopback and billet needs no open port
	// reachable from anywhere else.
	Listen string `yaml:"listen"`
	// BootstrapListen is a SECOND address serving only the two routes a machine
	// that has never enrolled needs: reading this deployment's authority, and
	// asking to join.
	//
	// ITS ABSENCE IS A REFUSAL, not a default. Without it this control plane does
	// not enroll over the network at all, and admission happens out of band —
	// `billet ca issue <node>` on the server, the bundle copied to the host.
	//
	// It exists because those two routes cannot require a certificate, and a
	// listener that admits callers who need not prove anything cannot share a
	// connection budget with the fleet: an anonymous caller that completes a
	// handshake and idles holds a slot, and once the budget is full a healthy
	// node's connection is never accepted. So `listen` demands a certificate in
	// the handshake and serves nothing else, and this address carries the rest.
	//
	// Both listeners present the same certificate, so whatever name a node dials
	// this one by has to be covered by node_tls_hosts (or by a concrete host in
	// one of the two listen addresses, which billet derives them from).
	//
	// Refused against a loopback `listen`: there are no certificates on a loopback
	// wire, so there is nothing to enroll into.
	BootstrapListen string `yaml:"bootstrap_listen,omitempty"`
	// StateDir is the SHORTHAND, and it is what most deployments write: one
	// directory holding the SQLite ledger, the process lock, the maintenance
	// fence and the mTLS CA. It MUST be on local storage, because SQLite's WAL
	// cannot work on a network filesystem and the state package fails closed if
	// it detects otherwise.
	//
	// It means exactly `identity_dir: <dir>` plus `state: {backend: sqlite}`, and
	// it stays supported. Writing it TOGETHER with `state:` is refused rather
	// than merged: two spellings of one value is a mistake internal/config has
	// already made three times, and it is silent every time.
	StateDir string `yaml:"state_dir,omitempty"`

	// IdentityDir holds what is NOT rows: the deployment identity, the node-wire
	// CA and its rotation state, the process lock, and the maintenance fence.
	//
	// SEPARATE FROM THE LEDGER BECAUSE THE TWO ARE NOT INTERCHANGEABLE. A ledger
	// can move into a database billet does not operate; a private key cannot
	// follow it there, and local process coordination has nothing to do with SQL
	// rows. The pairing is a refusal for the same reason: a subset of a
	// deployment is one that starts, looks healthy, and is not.
	//
	// Required when `state:` is written out; `state_dir` supplies it otherwise.
	IdentityDir string `yaml:"identity_dir,omitempty"`

	// State selects the backend the ledger lives in. Absent means the shorthand
	// above.
	State *StateConfig `yaml:"state,omitempty"`

	// Controllers says whether this deployment runs one control plane or an
	// active/passive pair. Absent means `single`, which is what every deployment
	// before this key had.
	//
	// REFUSED ON A SQLITE LEDGER. There is nothing to elect over: the ledger is a
	// file on local storage that a second machine cannot open at all, so a standby
	// would be a second process on one host waiting for a lock its own service
	// manager already restarts it to take.
	Controllers ControllerMode `yaml:"controllers,omitempty"`

	// Identity says where the deployment's identity material lives. Absent means
	// `file`, which is what every deployment before this key had.
	Identity *IdentityConfig `yaml:"identity,omitempty"`
	// MaxVCPU and MaxMemory bound what the allocator will ever hand out across every
	// tier combined. Required and positive: capacity is escrowed before each listener
	// advertises, so an absent ceiling lets concurrent listeners collectively
	// overcommit the machine.
	MaxVCPU   int      `yaml:"max_vcpu"`
	MaxMemory ByteSize `yaml:"max_memory"`

	// Placement decides which of several suitable machines a job is sent to.
	// Empty means pack. Only meaningful once a deployment has more than one.
	Placement PlacementPolicy `yaml:"placement,omitempty"`
	// AdmissionOrder decides which waiting tier takes the room a finished job
	// leaves, when the fleet cannot hold every tier that wants work. Empty means
	// fair. Only meaningful once a deployment has tiers of different sizes.
	AdmissionOrder AdmissionOrder `yaml:"admission_order,omitempty"`
	// NodeTLSHosts are the names and addresses nodes will dial this control plane by.
	// They become the subject names of the certificate it serves.
	//
	// REQUIRED WHEN listen IS A WILDCARD, which says which interfaces to accept on and
	// nothing about what a node types: a certificate minted for "0.0.0.0" matches
	// nothing, and the failure arrives as a handshake error on the node. A concrete
	// listen address supplies itself.
	NodeTLSHosts []string `yaml:"node_tls_hosts,omitempty"`
	// DrainTimeout bounds how long a stopping control plane waits for the jobs it is
	// already running before it destroys them. A Go duration string: "6h", "90m".
	//
	// A service manager's stop timeout must exceed this plus the teardown, or its own
	// expiry arrives first as a SIGKILL — skipping the teardown and stranding exactly
	// the compute the drain was protecting.
	DrainTimeout string `yaml:"drain_timeout,omitempty"`
	// Metrics serves the control plane's Prometheus metrics. Absent, nothing is
	// served: there is no default address.
	Metrics *MetricsConfig `yaml:"metrics,omitempty"`
	// FlightRecorder keeps the last two minutes of the controller's execution
	// trace in memory and writes them under identity_dir/flight-recorder when a
	// heartbeat pass overruns or the controller claim is lost. Off by default.
	FlightRecorder bool `yaml:"flight_recorder,omitempty"`
}

// MetricsConfig is one role's Prometheus endpoint: /metrics, and the Go
// profiler under /debug/pprof/ when Pprof is set.
type MetricsConfig struct {
	// Listen is the address the endpoint binds. It must be a literal loopback
	// address (127.0.0.1 or [::1], never a name the bind would resolve again)
	// unless AllowRemote is set: the endpoint has no authentication, and what it
	// reports (tier names, node names, how much of the fleet is in use) is the
	// deployment's own business.
	Listen string `yaml:"listen"`
	// AllowRemote admits a non-loopback Listen, for a scraper on another host.
	AllowRemote bool `yaml:"allow_remote,omitempty"`
	// Pprof also serves the Go profiler. A heap or goroutine profile carries
	// whatever memory held when it was taken, a credential included, so it is
	// refused on any Listen that is not a literal loopback address, AllowRemote
	// or not.
	Pprof bool `yaml:"pprof,omitempty"`
}

// DefaultServerStateDir is where a config that omits server.state_dir actually
// keeps its state — the value applyDefaults fills in. Exported for `billet
// init`'s identity refusal, which must treat an ABSENT key as this directory
// rather than as "no deployment to protect".
func DefaultServerStateDir() string { return defaultStateDir("server") }

// applyStateDefaults resolves the shorthand into the explicit form, ONCE, before
// anything validates or consumes it.
//
// THE SAME RULE THE REST OF THIS PACKAGE FOLLOWS: normalize in one place and
// write the result back, so validation and every consumer read the same value.
// Validation examining a shape the consumer does not use has now been found
// three times here, and it is silent every time.
//
// IT DOES NOT INVENT A BACKEND FOR A `state:` BLOCK THAT NAMES NONE. An absent
// backend is a config that did not say, and defaulting it to sqlite would make
// `state: {postgres: {...}}` a SQLite deployment carrying a PostgreSQL block
// nothing reads. Validation refuses it instead.
func (s *ServerConfig) applyStateDefaults() {
	if s.State == nil {
		if s.StateDir == "" {
			s.StateDir = defaultStateDir("server")
		}

		if s.IdentityDir == "" {
			s.IdentityDir = s.StateDir
		}

		return
	}

	if s.IdentityDir == "" && s.StateDir != "" {
		// Refused by validation as two spellings of one value; defaulted here
		// anyway so the diagnostic is about that rather than about a missing
		// identity_dir it would have had.
		s.IdentityDir = s.StateDir
	}
}

// LedgerBackend is the engine this config selects, resolved.
func (s *ServerConfig) LedgerBackend() StateBackend {
	if s.State == nil {
		return StateSQLite
	}

	return s.State.Backend
}

// LedgerPath is the SQLite ledger file, and is empty for any other backend.
//
// DERIVED, NEVER CONFIGURED. It is billet.db inside identity_dir, which is what
// state_dir has always meant and what every reader of a ledger file already
// assumes — the restore planner, the writer barrier and the archive among them.
func (s *ServerConfig) LedgerPath() string {
	if s.LedgerBackend() != StateSQLite {
		return ""
	}

	return filepath.Join(s.IdentityDir, "billet.db")
}

// DeploymentStateDirs are the directories that may hold this deployment's id,
// most authoritative first: the control plane mints it under server.state_dir, a
// node adopts it under node.state_dir.
func (c *Config) DeploymentStateDirs() []string {
	var dirs []string
	if c.Server != nil && c.Server.IdentityDir != "" {
		dirs = append(dirs, c.Server.IdentityDir)
	}

	if c.Node != nil && c.Node.StateDir != "" {
		dirs = append(dirs, c.Node.StateDir)
	}

	return dirs
}

// LedgerDSNEnv names the environment variable holding the PostgreSQL DSN, and is
// empty for any other backend.
func (s *ServerConfig) LedgerDSNEnv() string {
	if s.State == nil || s.State.Postgres == nil {
		return ""
	}

	return s.State.Postgres.DSNEnv
}

// envVarName is what a legal environment variable name looks like.
//
// REFUSED RATHER THAN ACCEPTED-AND-EMPTY, because os.Getenv answers "" for a
// name no shell could ever have exported, and an empty DSN is a control plane
// that fails to start with a message about the DSN rather than about the name.
var envVarName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func defaultStateDir(role string) string {
	if dir, err := os.UserConfigDir(); err == nil {
		return filepath.Join(dir, "billet", role)
	}
	return filepath.Join(".", ".billet", role)
}

// validateState refuses every way of naming the ledger that would leave a
// deployment believing it configured something it did not.
//
// Each rule below has a failure behind it, and the shape they share is that the
// alternative is SILENT: a config that loads, a control plane that starts, and a
// disagreement discovered later by an operator who has no reason to suspect this
// file.
// validateControllers refuses a controller layout this deployment cannot have.
//
// NORMALIZED RATHER THAN DEFAULTED AT EVERY READER, so `single` is written back
// and no caller has to remember what an empty value means. That is the rule this
// file already follows for a value nothing outside the process has an opinion
// about; it is not an identity, so there is nothing to compare it against
// elsewhere.
func (s *ServerConfig) validateControllers() []error {
	if s.Controllers == "" {
		s.Controllers = ControllersSingle

		return nil
	}

	switch s.Controllers {
	case ControllersSingle:
		return nil
	case ControllersActivePassive:
		// REFUSED ON A SQLITE LEDGER RATHER THAN IGNORED. There is nothing for a
		// second control plane to elect over — the ledger is a file on local
		// storage, and the state package refuses to put one anywhere a second
		// machine could reach — so a standby here would wait forever for a lock
		// that only its own host's other process could hold. Accepting the key and
		// doing nothing with it is the failure this file has already made three
		// times: a deployment that believes it configured something.
		if s.LedgerBackend() != StatePostgres {
			return []error{fmt.Errorf(
				"server.controllers: %s needs a ledger a second machine can reach, and this "+
					"deployment's is %s. A SQLite ledger is a file on local storage — billet "+
					"refuses to put one on a network filesystem at all — so there is nothing for "+
					"a second control plane to take over. Move the ledger to PostgreSQL "+
					"(server.state.backend: %s) or leave this as %s",
				ControllersActivePassive, s.LedgerBackend(), StatePostgres, ControllersSingle)}
		}

		return nil
	default:
		return []error{fmt.Errorf(
			"server.controllers: %q is not a controller layout billet knows; write %s (the "+
				"default: one control plane, and a second one is refused) or %s (a pair, where "+
				"whichever takes the claim first is the controller and the other waits)",
			s.Controllers, ControllersSingle, ControllersActivePassive)}
	}
}

// IdentityBackendKind is where this deployment's identity material lives,
// resolved. Absent means the file backend.
func (s *ServerConfig) IdentityBackendKind() IdentityBackend {
	if s == nil || s.Identity == nil || s.Identity.Backend == "" {
		return IdentityFile
	}

	return s.Identity.Backend
}

// IdentitySSM is the Parameter Store configuration, or nil.
func (s *ServerConfig) IdentitySSM() *IdentitySSMConfig {
	if s == nil || s.Identity == nil {
		return nil
	}

	return s.Identity.AWSSSM
}

// validateIdentity refuses an identity store this deployment cannot have.
func (c *Config) validateIdentity() []error {
	s := c.Server
	if s == nil || s.Identity == nil {
		return nil
	}

	switch s.Identity.Backend {
	case IdentityFile:
		if s.Identity.AWSSSM != nil {
			return []error{fmt.Errorf(
				"server.identity.aws_ssm is written but the backend is %s, so nothing would "+
					"read it. Set backend: %s, or remove the block",
				IdentityFile, IdentitySSM)}
		}

		return nil
	case IdentitySSM:
		return c.validateSSMIdentity()
	case "":
		return []error{fmt.Errorf(
			"server.identity.backend is required when server.identity is written out; "+
				"write %s (the default: the authority and the App key are files in "+
				"identity_dir) or %s", IdentityFile, IdentitySSM)}
	default:
		return []error{fmt.Errorf(
			"server.identity.backend: %q is not an identity store billet knows; write %s or %s",
			s.Identity.Backend, IdentityFile, IdentitySSM)}
	}
}

// validateSSMIdentity checks the Parameter Store block and refuses the second
// spelling of the App key.
func (c *Config) validateSSMIdentity() []error {
	var errs []error

	ssm := c.Server.Identity.AWSSSM
	if ssm == nil {
		return []error{fmt.Errorf(
			"server.identity.aws_ssm is required when the backend is %s; it names the region "+
				"and the parameter path this deployment's identity lives under", IdentitySSM)}
	}

	if strings.TrimSpace(ssm.Region) == "" {
		errs = append(errs, errors.New(
			"server.identity.aws_ssm.region is required; it is the SIGNING region and it "+
				"selects the endpoint, so there is no value billet could guess"))
	}

	switch {
	case strings.TrimSpace(ssm.Prefix) == "":
		errs = append(errs, errors.New(
			"server.identity.aws_ssm.prefix is required; everything billet stores lands under "+
				"it, so IAM can be scoped by path and two deployments cannot read each "+
				"other's authority"))
	case !strings.HasPrefix(ssm.Prefix, "/"):
		// A PARAMETER STORE PATH IS ABSOLUTE. A relative name is a legal parameter
		// and a DIFFERENT one, so accepting it would silently give a deployment a
		// store nobody's IAM policy covers.
		errs = append(errs, fmt.Errorf(
			"server.identity.aws_ssm.prefix must start with '/' (got %q); Parameter Store "+
				"paths are absolute, and a relative name is a different parameter rather "+
				"than the same one written informally", ssm.Prefix))
	}

	return errs
}

func (s *ServerConfig) validateState() []error {
	var errs []error

	// TWO SPELLINGS OF ONE VALUE. Merging them means guessing which the operator
	// meant when they disagree, and the guess is invisible.
	if s.StateDir != "" && s.State != nil {
		errs = append(errs, errors.New(
			"server.state_dir and server.state are two spellings of the same thing and only "+
				"one may be written. state_dir means `identity_dir: <dir>` plus "+
				"`state: {backend: sqlite}`; write that out instead if you need to name a "+
				"backend"))
	}

	if s.IdentityDir == "" {
		errs = append(errs, errors.New(
			"server.identity_dir is required when server.state is written out; it holds the "+
				"deployment identity, the node-wire CA, the process lock and the maintenance "+
				"fence, none of which move into a database"))
	}

	// AN IDENTITY IS REFUSED RATHER THAN TRIMMED, and a path is normalized. This
	// one is a path, so its padding is not an identity mistake — but it is also
	// the directory a CA lives in, and silently changing which directory a
	// config names is the failure this whole rule family exists for.
	if s.IdentityDir != "" && strings.TrimSpace(s.IdentityDir) != s.IdentityDir {
		errs = append(errs, fmt.Errorf(
			"server.identity_dir is %q, which begins or ends with whitespace; it names a "+
				"directory holding this deployment's private key, so billet will not guess "+
				"which one you meant", s.IdentityDir))
	}

	if s.State == nil {
		return errs
	}

	switch s.State.Backend {
	case StateSQLite:
		if s.State.Postgres != nil {
			errs = append(errs, errors.New(
				"server.state.postgres is set with backend: sqlite, so nothing would read it; "+
					"remove it or select the backend it configures"))
		}
	case StatePostgres:
		errs = append(errs, s.validatePostgresState()...)
	case "":
		errs = append(errs, errors.New(
			"server.state.backend is required; it is "+string(StateSQLite)+" or "+
				string(StatePostgres)))
	default:
		errs = append(errs, fmt.Errorf(
			"server.state.backend is %q; it is %s or %s",
			s.State.Backend, StateSQLite, StatePostgres))
	}

	return errs
}

func (s *ServerConfig) validatePostgresState() []error {
	var errs []error

	pg := s.State.Postgres
	if pg == nil || pg.DSNEnv == "" {
		return append(errs, errors.New(
			"server.state.postgres.dsn_env is required; it names the ENVIRONMENT VARIABLE "+
				"holding the connection string, because a DSN carries a password and a secret "+
				"written into this file ends up in a backup"))
	}

	if err := CheckDSNEnv(pg.DSNEnv); err != nil {
		errs = append(errs, fmt.Errorf("server.state.postgres.dsn_env: %w", err))
	}

	return errs
}

// CheckDSNEnv is the one rule for what may name the PostgreSQL connection
// string's environment variable.
//
// EXPORTED BECAUSE THERE ARE TWO ENTRY POINTS. Config validation reaches it for
// a file on disk, and `billet init` reaches it for a flag — and a rule enforced
// at only one of two entry points is an entry point that does not enforce it.
// Without this, `--state-dsn-env 9-lives` was accepted by the generator, written
// into the file, and then refused by Parse with a message blaming a generated
// block the operator never typed.
func CheckDSNEnv(name string) error {
	// THE MOST LIKELY MISTAKE IS PUTTING THE DSN ITSELF HERE, and it is worth its
	// own sentence: os.Getenv would answer "" for it, and the deployment would
	// fail to start complaining about an empty data source rather than about the
	// value that is wrong. Checked first, so the more specific diagnostic wins.
	if strings.ContainsAny(name, ":/@ ") || strings.HasPrefix(name, "postgres") {
		return fmt.Errorf(
			"%q looks like a connection string rather than the NAME of an environment "+
				"variable holding one. Write the name, and keep the DSN out of the config", name)
	}

	if !envVarName.MatchString(name) {
		return fmt.Errorf(
			"%q is not a legal environment variable name; nothing could export it, so the "+
				"DSN would always read as empty", name)
	}

	return nil
}

func (c *Config) validateServer() []error {
	if c.Server == nil {
		return nil
	}
	var errs []error

	errs = append(errs, c.Server.validateState()...)
	// AFTER validateState, because what a controller layout is allowed to be
	// depends on which backend the ledger is on and LedgerBackend answers from
	// the block that one has just checked.
	errs = append(errs, c.Server.validateControllers()...)
	errs = append(errs, c.validateIdentity()...)
	if err := validateHostPort("server.listen", c.Server.Listen); err != nil {
		errs = append(errs, err)
	}

	errs = append(errs, c.validateBootstrapListen()...)

	if err := c.Server.Placement.Validate(); err != nil {
		errs = append(errs, err)
	}

	if err := c.Server.AdmissionOrder.Validate(); err != nil {
		errs = append(errs, err)
	}
	// Required, not optional. Without a ceiling the allocator has nothing to
	// escrow against and concurrent tier listeners can overcommit the machine.
	if c.Server.MaxVCPU <= 0 {
		errs = append(errs, errors.New(
			"server.max_vcpu must be positive; it is the ceiling the allocator escrows against"))
	}
	if c.Server.MaxMemory <= 0 {
		errs = append(errs, errors.New(
			"server.max_memory must be positive; it is the ceiling the allocator escrows against"))
	}
	// Parsed here so a typo is reported when the file is READ, rather than at the
	// shutdown that needed it — by which point the operator is watching a restart
	// that will not finish and has no reason to suspect the config.
	if _, err := c.Server.DrainTimeoutDuration(); err != nil {
		errs = append(errs, err)
	}
	return errs
}

// validateMetrics checks each role's metrics endpoint: loopback unless remote
// scraping was asked for, the profiler only on loopback, and never the socket of
// another listener in the file. The two roles share one file on a single host,
// so the server's and the node's endpoints are checked against each other too.
func (c *Config) validateMetrics() []error {
	type listener struct{ key, addr string }

	var (
		errs   []error
		others []listener
		ends   []listener
	)

	if c.Server != nil {
		others = append(others, listener{"server.listen", c.Server.Listen},
			listener{"server.bootstrap_listen", c.Server.BootstrapListen})
		if c.Server.Metrics != nil {
			ends = append(ends, listener{"server.metrics", c.Server.Metrics.Listen})
		}
	}

	if c.Node != nil {
		if c.Node.Cache != nil {
			others = append(others, listener{"node.cache.listen", c.Node.Cache.Listen})
		}
		if c.Node.Metrics != nil {
			ends = append(ends, listener{"node.metrics", c.Node.Metrics.Listen})
		}
	}

	metricsOf := func(key string) *MetricsConfig {
		if key == "server.metrics" {
			return c.Server.Metrics
		}

		return c.Node.Metrics
	}

	for i, end := range ends {
		m := metricsOf(end.key)
		// AS WRITTEN, NOT TRIMMED: the bind uses this string, and a padded
		// address that validated here would fail at startup instead, whether or
		// not allow_remote waves the loopback rule aside.
		addr := m.Listen

		if addr != strings.TrimSpace(addr) {
			errs = append(errs, fmt.Errorf("%s.listen %q has whitespace around it, which the bind "+
				"would not accept; write the address alone", end.key, addr))

			continue
		}

		if err := validateHostPort(end.key+".listen", addr); err != nil {
			errs = append(errs, err)

			continue
		}

		// A LITERAL LOOPBACK ADDRESS, NOT A NAME. `localhost` is what
		// LoopbackAddr accepts elsewhere, but the bind resolves it again, and a
		// resolver that maps it to another interface would serve an
		// unauthenticated endpoint, the profiler included, off this machine.
		loopback := literalLoopback(addr)

		if !loopback && !m.AllowRemote {
			errs = append(errs, fmt.Errorf("%s.listen is %q, which is not a loopback address "+
				"(127.0.0.1 or [::1]; a name such as localhost is not accepted, because the bind "+
				"resolves it again). The endpoint has no authentication, so it binds loopback "+
				"unless %s.allow_remote is true; set it only on a network where whoever can "+
				"reach the port may read the fleet's state", end.key, addr, end.key))
		}

		if m.Pprof && !loopback {
			errs = append(errs, fmt.Errorf("%s.pprof is set on %q, which is not a loopback address. "+
				"A heap or goroutine profile carries whatever memory held when it was taken, a "+
				"credential included, so the profiler is served on 127.0.0.1 or [::1] only, "+
				"allow_remote or not", end.key, addr))
		}

		for _, o := range append(others, ends[:i]...) {
			other := strings.TrimSpace(o.addr)
			if other != "" && addressesOverlap(addr, other) {
				errs = append(errs, fmt.Errorf("%s.listen is %q and %s is %q, which are the same "+
					"socket; give the metrics endpoint a port of its own", end.key, addr, o.key, other))
			}
		}
	}

	return errs
}

// validateBootstrapListen checks the enrollment listener against the wire it
// exists to keep anonymous traffic off.
func (c *Config) validateBootstrapListen() []error {
	addr := strings.TrimSpace(c.Server.BootstrapListen)
	if addr == "" {
		return nil
	}

	// A LOOPBACK WIRE HAS NO CERTIFICATES AT ALL, so it has nothing to enroll a
	// machine into — and nothing off this host could reach the operational wire
	// anyway. The same rule refuses node.tls against a loopback server.
	if LoopbackAddr(c.Server.Listen) {
		return []error{fmt.Errorf(
			"server.bootstrap_listen is set, but server.listen is %q — a control plane that "+
				"accepts only on loopback serves plain HTTP and issues no certificates, so there "+
				"is nothing for a node to enroll into. Remove server.bootstrap_listen, or bind "+
				"server.listen to the address this deployment publishes to its fleet",
			c.Server.Listen)}
	}

	var errs []error

	if err := validateHostPort("server.bootstrap_listen", addr); err != nil {
		errs = append(errs, err)
	}

	// THE POINT IS THAT THEY ARE NOT THE SAME LISTENER, and equality of the two
	// strings is not the question — "0.0.0.0:7717" and "127.0.0.1:7717" are
	// different strings and the same socket. Leaving that to bind time was a
	// worse answer than it looked: whichever listener binds second reports the
	// collision, so the operator is sent to the setting that is fine.
	if addressesOverlap(addr, strings.TrimSpace(c.Server.Listen)) {
		errs = append(errs, fmt.Errorf(
			"server.bootstrap_listen is %q and server.listen is %q, which are the same socket. "+
				"Separating them is the whole point: the node wire demands a client certificate "+
				"in the handshake, and enrollment cannot, so one address cannot serve both",
			addr, strings.TrimSpace(c.Server.Listen)))
	}

	return errs
}
