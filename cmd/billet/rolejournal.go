package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/junioryono/billet/internal/hostupgrade"
	"github.com/junioryono/billet/internal/regularfile"
)

// THE ROLE'S JOURNAL. The host role's binary transaction journals itself in a
// recovery directory under the upgrade root as `manifest.yml`, the way this
// program journals its own in `journal.json`. A converge guard's takeover
// (`hold --recover-from`) must prove the transaction behind a guard's pointer
// is complete before it re-labels the guard, whichever program wrote it, so
// the manifest is judged here: its shape and nothing more. What the role's
// recovery then needs of the host (the backups' presence, the previous
// binary, the ledger snapshot, the mount) is the role's to check when it
// resumes; this program never resumes a role transaction and never loads its
// journal as one of its own.

// roleJournalName is the file inside a recovery directory the role's
// transaction writes (`upgrade-inspect.yml` reads exactly this shape).
const roleJournalName = "manifest.yml"

// maxRoleJournalBytes bounds a manifest: three inputs and a dozen scalars are
// under a kilobyte, and a manifest past this is not one the role wrote.
const maxRoleJournalBytes = 64 << 10

// errRoleJournal is a recovery directory that holds the role's journal and
// not this program's: a converge's transaction, which the role resumes.
var errRoleJournal = errors.New("a converge's transaction, not a Go one; the role resumes it")

// recoveryKind is which program's journal a recovery directory holds.
type recoveryKind int

const (
	recoveryNone recoveryKind = iota
	recoveryGo
	recoveryRole
)

// roleJournal is the manifest's shape, exactly the members the role writes
// and `upgrade-inspect.yml` requires.
type roleJournal struct {
	Version               int                `yaml:"version"`
	ServerStateDir        string             `yaml:"server_state_dir"`
	NodeStateDir          string             `yaml:"node_state_dir"`
	ServiceUser           string             `yaml:"service_user"`
	ServiceGroup          string             `yaml:"service_group"`
	BinaryExisted         bool               `yaml:"binary_existed"`
	ServerWasActive       bool               `yaml:"server_was_active"`
	NodeWasActive         bool               `yaml:"node_was_active"`
	CandidateServerActive bool               `yaml:"candidate_server_active"`
	CandidateNodeActive   bool               `yaml:"candidate_node_active"`
	ServerEnablement      string             `yaml:"server_enablement"`
	NodeEnablement        string             `yaml:"node_enablement"`
	Inputs                []roleJournalInput `yaml:"inputs"`
}

type roleJournalInput struct {
	Path    string `yaml:"path"`
	Backup  string `yaml:"backup"`
	Existed bool   `yaml:"existed"`
	Owner   string `yaml:"owner"`
	Group   string `yaml:"group"`
	Mode    string `yaml:"mode"`
}

var (
	roleJournalMembers = []string{"version", "server_state_dir", "node_state_dir", "service_user", "service_group",
		"binary_existed", "server_was_active", "node_was_active", "candidate_server_active", "candidate_node_active",
		"server_enablement", "node_enablement", "inputs"}
	roleJournalInputMembers = []string{"path", "backup", "existed", "owner", "group", "mode"}
	// roleJournalInputs are the three inputs, in the order the role records
	// them, with the backup names it gives them.
	roleJournalInputs = [][2]string{
		{"/etc/billet/billet.yaml", "billet.yaml.previous"},
		{"/etc/systemd/system/billet-server.service", "billet-server.service.previous"},
		{"/etc/systemd/system/billet-node.service", "billet-node.service.previous"},
	}
	roleJournalEnablements = map[string]bool{"enabled": true, "enabled-runtime": true, "disabled": true,
		"static": true, "indirect": true, "generated": true, "transient": true, "not-found": true}
	roleAccountPattern = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
	roleModePattern    = regexp.MustCompile(`^[0-7]{4}$`)
	roleStateRoot      = "/var/lib/billet/"
	// roleUpgradeRootPath is the role's own rule, the packaged host's path
	// spelled in the manifest, whatever root this process was pointed at.
	roleUpgradeRootPath = "/var/lib/billet/upgrades"
)

// readRoleJournalAt reads and judges the role's manifest inside a recovery
// directory already opened and judged, through descriptors: the file must be
// a regular owned one, within the bound, YAML, and exactly the shape the role
// writes; anything else refuses naming the member.
func readRoleJournalAt(recovery *os.File, dir string) (*roleJournal, error) {
	path := filepath.Join(dir, roleJournalName)

	if err := guardObserve("open", path, nil); err != nil {
		return nil, err
	}

	f, info, err := regularfile.OpenAt(recovery, roleJournalName)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s holds neither journal.json nor %s", errNoJournalOfEitherKind, dir, roleJournalName)
		}

		return nil, fmt.Errorf("open the role's journal %s: %w", path, err)
	}

	defer func() { _ = f.Close() }()

	if err := requireTrustedFile(path, info); err != nil {
		return nil, err
	}

	body, err := regularfile.ReadAllLimited(f, path, maxRoleJournalBytes)
	if err != nil {
		if errors.Is(err, regularfile.ErrTooLarge) {
			return nil, fmt.Errorf("the role's journal %s is larger than %d bytes, which is not a manifest the role wrote",
				path, maxRoleJournalBytes)
		}

		return nil, fmt.Errorf("read the role's journal %s: %w", path, err)
	}

	return parseRoleJournal(body, path)
}

// parseRoleJournal is the one reading of a manifest's bytes.
func parseRoleJournal(body []byte, path string) (*roleJournal, error) {
	// THE MEMBER SET FIRST, by name: a member the role does not write, or one
	// it writes that is missing, refuses before any value is read.
	var top map[string]any
	if err := yaml.Unmarshal(body, &top); err != nil {
		return nil, fmt.Errorf("the role's journal %s is not YAML: %w", path, err)
	}

	if top == nil {
		return nil, fmt.Errorf("the role's journal %s is empty", path)
	}

	if err := exactMembers(top, roleJournalMembers, "the role's journal "+path); err != nil {
		return nil, err
	}

	rawInputs, ok := top["inputs"].([]any)
	if !ok {
		return nil, fmt.Errorf("the role's journal %s: inputs is not a list", path)
	}

	if len(rawInputs) != len(roleJournalInputs) {
		return nil, fmt.Errorf("the role's journal %s: inputs has %d entries, want %d", path, len(rawInputs), len(roleJournalInputs))
	}

	for i, raw := range rawInputs {
		entry, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("the role's journal %s: inputs[%d] is not a mapping", path, i)
		}

		if err := exactMembers(entry, roleJournalInputMembers, fmt.Sprintf("the role's journal %s: inputs[%d]", path, i)); err != nil {
			return nil, err
		}
	}

	// THE TYPES, judged on the untyped reading BEFORE the typed decode, which
	// would otherwise report a string written where a boolean belongs as its
	// own conversion failure: a boolean written as a string, a version written
	// as a string, each named.
	for _, name := range []string{"binary_existed", "server_was_active", "node_was_active", "candidate_server_active", "candidate_node_active"} {
		if _, ok := top[name].(bool); !ok {
			return nil, fmt.Errorf("the role's journal %s: %s is not a boolean", path, name)
		}
	}

	if _, ok := top["version"].(int); !ok {
		return nil, fmt.Errorf("the role's journal %s: version is not a number", path)
	}

	for _, name := range []string{"server_state_dir", "node_state_dir", "service_user", "service_group", "server_enablement", "node_enablement"} {
		if _, ok := top[name].(string); !ok {
			return nil, fmt.Errorf("the role's journal %s: %s is not a string", path, name)
		}
	}

	for i, raw := range rawInputs {
		entry, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("the role's journal %s: inputs[%d] is not a mapping", path, i)
		}

		if _, ok := entry["existed"].(bool); !ok {
			return nil, fmt.Errorf("the role's journal %s: inputs[%d].existed is not a boolean", path, i)
		}

		for _, name := range []string{"path", "backup", "owner", "group", "mode"} {
			if _, ok := entry[name].(string); !ok {
				return nil, fmt.Errorf("the role's journal %s: inputs[%d].%s is not a string", path, i, name)
			}
		}
	}

	var j roleJournal

	dec := yaml.NewDecoder(strings.NewReader(string(body)))
	dec.KnownFields(true)

	if err := dec.Decode(&j); err != nil {
		return nil, fmt.Errorf("the role's journal %s: %w", path, err)
	}

	if j.Version != 2 {
		return nil, fmt.Errorf("the role's journal %s: version is %d, want 2", path, j.Version)
	}

	for name, dir := range map[string]string{"server_state_dir": j.ServerStateDir, "node_state_dir": j.NodeStateDir} {
		if err := checkRoleStateDir(dir); err != nil {
			return nil, fmt.Errorf("the role's journal %s: %s %w", path, name, err)
		}
	}

	if j.ServerStateDir == j.NodeStateDir {
		return nil, fmt.Errorf("the role's journal %s: server_state_dir and node_state_dir are the same directory", path)
	}

	if strings.HasPrefix(j.ServerStateDir, j.NodeStateDir+"/") || strings.HasPrefix(j.NodeStateDir, j.ServerStateDir+"/") {
		return nil, fmt.Errorf("the role's journal %s: server_state_dir and node_state_dir nest", path)
	}

	for name, value := range map[string]string{"service_user": j.ServiceUser, "service_group": j.ServiceGroup} {
		if !roleAccountPattern.MatchString(value) {
			return nil, fmt.Errorf("the role's journal %s: %s %q is not an account name", path, name, value)
		}
	}

	for name, value := range map[string]string{"server_enablement": j.ServerEnablement, "node_enablement": j.NodeEnablement} {
		if !roleJournalEnablements[value] {
			return nil, fmt.Errorf("the role's journal %s: %s %q is not an enablement state", path, name, value)
		}
	}

	for i, in := range j.Inputs {
		want := roleJournalInputs[i]
		if in.Path != want[0] || in.Backup != want[1] {
			return nil, fmt.Errorf("the role's journal %s: inputs[%d] is %s (%s), want %s (%s)", path, i, in.Path, in.Backup, want[0], want[1])
		}

		for name, value := range map[string]string{"owner": in.Owner, "group": in.Group} {
			if !roleAccountPattern.MatchString(value) {
				return nil, fmt.Errorf("the role's journal %s: inputs[%d].%s %q is not an account name", path, i, name, value)
			}
		}

		if !roleModePattern.MatchString(in.Mode) {
			return nil, fmt.Errorf("the role's journal %s: inputs[%d].mode %q is not four octal digits", path, i, in.Mode)
		}
	}

	return &j, nil
}

// errNoJournalOfEitherKind is a recovery directory holding neither program's
// journal; it wraps hostupgrade's ErrNoJournal so the callers that release a
// journal-less claim keep their answer.
var errNoJournalOfEitherKind = fmt.Errorf("%w: neither journal", hostupgrade.ErrNoJournal)

// checkRoleStateDir holds a state directory to what the role admits: under
// /var/lib/billet, outside the upgrade root, no dot segment.
func checkRoleStateDir(dir string) error {
	switch {
	case !strings.HasPrefix(dir, roleStateRoot):
		return fmt.Errorf("%q is not under %s", dir, roleStateRoot)
	case dir == roleUpgradeRootPath || strings.HasPrefix(dir, roleUpgradeRootPath+"/"):
		return fmt.Errorf("%q lies under the upgrade root", dir)
	case strings.Contains(dir, "/./"), strings.Contains(dir, "/../"), strings.HasSuffix(dir, "/."), strings.HasSuffix(dir, "/.."):
		return fmt.Errorf("%q carries a dot segment", dir)
	}

	return nil
}

// exactMembers requires a mapping to carry exactly the named members.
func exactMembers(m map[string]any, members []string, what string) error {
	for _, name := range members {
		if _, ok := m[name]; !ok {
			return fmt.Errorf("%s: %s is missing", what, name)
		}
	}

	for name := range m {
		known := false

		for _, member := range members {
			if name == member {
				known = true

				break
			}
		}

		if !known {
			return fmt.Errorf("%s: %s is not a member the role writes", what, name)
		}
	}

	return nil
}
