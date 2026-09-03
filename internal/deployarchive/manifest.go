package deployarchive

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/junioryono/billet/internal/state"
)

// Manifest describes an archive well enough that a restore can refuse a wrong
// one WITHOUT opening anything it is about to install.
type Manifest struct {
	Schema int    `json:"schema"`
	Kind   string `json:"kind"`

	CreatedAt     string `json:"created_at"`
	BilletVersion string `json:"billet_version"`

	// DeploymentID is the identity every other piece belongs to. It is repeated
	// here rather than only stored in identity/deployment-id so that a mismatch
	// between the manifest and the file is itself detectable.
	DeploymentID string `json:"deployment_id"`

	Source    Source         `json:"source"`
	GitHub    GitHubIdentity `json:"github"`
	Authority AuthorityFacts `json:"authority"`
	Ledger    LedgerFacts    `json:"ledger"`

	Files []FileRecord `json:"files"`
}

// Source is where the archive came from. INFORMATIONAL ONLY — a restore never
// acts on it, because the target's paths are the target's business.
type Source struct {
	Host       string `json:"host"`
	ConfigPath string `json:"config_path"`
	StateDir   string `json:"state_dir"`
}

// GitHubIdentity is the App this deployment's key belongs to.
//
// RECORDED SO A RESTORE CANNOT PAIR A KEY WITH UNRELATED CONFIGURATION. The key
// file itself says nothing about which App it is for, so installing it beside a
// config naming a different app_id produces a deployment that authenticates as
// nothing and reports a bare 401 on its first poll.
type GitHubIdentity struct {
	Org            string `json:"org"`
	AppID          int64  `json:"app_id"`
	ClientID       string `json:"client_id,omitempty"`
	InstallationID int64  `json:"installation_id"`
}

// Same reports whether two App identities describe the same App.
//
// ClientID IS NOT COMPARED. It is optional — every config written before the
// field existed omits it — so requiring it to match would refuse a correct
// restore on the strength of a field one side simply never recorded.
func (g GitHubIdentity) Same(other GitHubIdentity) bool {
	return g.Org == other.Org &&
		g.AppID == other.AppID &&
		g.InstallationID == other.InstallationID
}

func (g GitHubIdentity) String() string {
	return fmt.Sprintf("org %s, app %d, installation %d", g.Org, g.AppID, g.InstallationID)
}

// AuthorityFacts is what the archive holds of the node-wire CA.
type AuthorityFacts struct {
	// Fingerprint is the issuing authority's public-key fingerprint, in the
	// shape `billet ca show` prints so an operator can compare the two by eye.
	Fingerprint string `json:"fingerprint"`
	NotAfter    string `json:"not_after"`

	// Rotating says a rotation was running when this was taken, which means the
	// previous authority's key travels in the archive too — it is what signs
	// what the control plane PRESENTS until the fleet has renewed.
	Rotating               bool     `json:"rotating"`
	PreviousFingerprint    string   `json:"previous_fingerprint,omitempty"`
	PreviousNotAfter       string   `json:"previous_not_after,omitempty"`
	UnexpectedFilesPresent []string `json:"unexpected_files_present,omitempty"`
}

// LedgerFacts is what the archive says about the ledger.
//
// READ BACK FROM THE COMPLETED SNAPSHOT, never from the live database beside it.
// The live one is moving: a control plane restarted onto a newer binary can
// migrate between the snapshot being taken and the manifest being written, and
// the manifest would then describe a schema the archive does not contain.
//
// EXCEPT WHEN THE LEDGER IS EXTERNAL, where there is no snapshot to read back
// from and the live database is the only source there could be. What that costs
// is stated rather than hidden: the list is what the ledger carried at the moment
// of the backup and the database goes on migrating afterwards, so it is
// PROVENANCE and never a proof. The one thing it is still allowed to decide is
// the refusal in checkBinaryUnderstandsArchive — a stale list can only be BEHIND
// the truth, so that check may under-refuse and can never refuse a restore it
// should have allowed.
type LedgerFacts struct {
	// External says the ledger is not in this archive and never was.
	//
	// NOT DERIVED FROM THE ABSENCE OF THE ENTRY, which would make a truncated
	// archive and a deliberate one the same thing — and the truncated one would
	// then restore an identity paired with nothing, which is precisely the
	// half-deployment this package exists to refuse.
	External bool `json:"external,omitempty"`

	// Backend names the engine the external ledger lives in, e.g. "postgres".
	// Empty on a schema-1 archive and on any archive carrying its own ledger.
	Backend string `json:"backend,omitempty"`

	// DSNEnv is the ENVIRONMENT VARIABLE the source config named for the
	// connection string, and it is the variable's name rather than its value.
	// A DSN carries a password; recording one here would put it in a file that
	// travels off-site, which is the rule PostgresStateConfig already follows.
	//
	// INFORMATIONAL. A restore reports it and does not require the target to
	// agree: naming the variable differently on a replacement host is ordinary,
	// and refusing that would be refusing a correct restore over a label.
	DSNEnv string `json:"dsn_env,omitempty"`

	Migrations []state.AppliedMigration `json:"migrations"`
}

// IsExternal reports whether the ledger lives outside this archive.
func (l LedgerFacts) IsExternal() bool { return l.External }

// HighestVersion is the newest migration the snapshot carries.
func (l LedgerFacts) HighestVersion() int {
	high := 0
	for _, m := range l.Migrations {
		if m.Version > high {
			high = m.Version
		}
	}

	return high
}

// Record finds one file's manifest entry.
func (m Manifest) Record(entry string) (FileRecord, bool) {
	for _, f := range m.Files {
		if f.Path == entry {
			return f, true
		}
	}

	return FileRecord{}, false
}

// encode renders the manifest deterministically.
func (m Manifest) encode() ([]byte, error) {
	sort.Slice(m.Files, func(i, j int) bool { return m.Files[i].Path < m.Files[j].Path })

	body, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("deployarchive: encode the manifest: %w", err)
	}

	return append(body, '\n'), nil
}

// decodeManifest parses one and refuses a schema this build does not know.
//
// THE SCHEMA CHECK COMES FIRST AND IS EXACT. A future archive read by an older
// billet must fail HERE, with a sentence naming the version, rather than five
// checks later with a confusing complaint about a missing field it never had.
func decodeManifest(body []byte) (Manifest, error) {
	var m Manifest

	if err := json.Unmarshal(body, &m); err != nil {
		return Manifest{}, fmt.Errorf("%w: its %s does not parse: %w", errNotAnArchive,
			EntryManifest, err)
	}

	if m.Kind != Kind {
		return Manifest{}, fmt.Errorf("%w: %s says kind %q, and a billet backup says %q",
			errNotAnArchive, EntryManifest, m.Kind, Kind)
	}

	if !readableSchemas[m.Schema] {
		return Manifest{}, fmt.Errorf(
			"this archive is schema %d and this billet reads %s. A reader accepts "+
				"exactly the versions it knows rather than guessing at one it does not — restore "+
				"it with the billet that wrote it (%s)", m.Schema, readableSchemaList(),
			orUnrecorded(m.BilletVersion))
	}

	// A SCHEMA THAT CANNOT EXPRESS THE CLAIM MUST NOT BE READ AS MAKING IT.
	//
	// External is a schema-2 field. A schema-1 manifest that carries it was
	// written by nothing billet ships — hand-edited, or truncated and patched —
	// and honouring it would install an identity with no ledger while the archive
	// declares the version whose entry set REQUIRES one. Refusing names the
	// contradiction rather than resolving it.
	if m.Schema < 2 && m.Ledger.External {
		return Manifest{}, fmt.Errorf(
			"%w: %s declares schema %d and also says the ledger is external, which schema %d "+
				"has no way to express", errNotAnArchive, EntryManifest, m.Schema, m.Schema)
	}

	// AND AN EXTERNAL LEDGER NAMES AN ENGINE THIS BUILD CAN PAIR WITH.
	//
	// REFUSED HERE RATHER THAN AT PLANNING, because the planner's refusal for this
	// case was WRONG: with no backend to name it told an operator the control
	// plane would "create an empty ledger of its own" and advised configuring
	// `state: {backend: an unnamed engine}`. On a PostgreSQL target the first is
	// false — it would connect to the database its config names — and the second
	// is not a thing to type. The archive is malformed, which is a fact about the
	// archive and belongs where the archive is read.
	if m.Ledger.External && !externalBackends[m.Ledger.Backend] {
		return Manifest{}, fmt.Errorf(
			"%w: %s says its ledger is external and names %s, which this billet cannot pair a "+
				"deployment with", errNotAnArchive, EntryManifest, orUnnamed(m.Ledger.Backend))
	}

	// AND AN ARCHIVE CARRYING ITS LEDGER DOES NOT DESCRIBE ONE SOMEWHERE ELSE.
	// The two fields exist to say where the ledger is; a manifest setting them
	// while shipping the file is describing two different deployments.
	if !m.Ledger.External && (m.Ledger.Backend != "" || m.Ledger.DSNEnv != "") {
		return Manifest{}, fmt.Errorf(
			"%w: %s carries its own ledger and also describes an external one (%s)",
			errNotAnArchive, EntryManifest, orUnnamed(m.Ledger.Backend))
	}

	return m, nil
}

// orUnnamed renders a backend for a diagnostic, including the empty one.
func orUnnamed(backend string) string {
	if backend == "" {
		return "no engine at all"
	}

	return backend
}

// readableSchemaList renders readableSchemas for a diagnostic.
func readableSchemaList() string {
	versions := make([]int, 0, len(readableSchemas))
	for v := range readableSchemas {
		versions = append(versions, v)
	}

	sort.Ints(versions)

	parts := make([]string, 0, len(versions))
	for _, v := range versions {
		parts = append(parts, strconv.Itoa(v))
	}

	return "schema " + strings.Join(parts, " and ")
}

func orUnrecorded(v string) string {
	if v == "" {
		return "version unrecorded"
	}

	return "written by " + v
}
