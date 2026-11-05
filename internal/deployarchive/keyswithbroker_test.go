package deployarchive

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// brokerBackupTo is a backup of a deployment whose App keys a broker holds.
func brokerBackupTo(t *testing.T, d deployment, dest string) Manifest {
	t.Helper()

	m, err := Write(t.Context(), BackupRequest{
		Dest:           dest,
		StateDir:       d.stateDir,
		ConfigPath:     d.configPath,
		DeploymentID:   d.id,
		GitHub:         d.github,
		KeysWithBroker: true,
		ConfigBody:     []byte("server: {}\n"),
		ExternalLedger: &ExternalLedger{Backend: "postgres", DSNEnv: "BILLET_STATE_DSN"},
		Now:            nowStub,
		Hostname:       "test-host",
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	return m
}

// AN ARCHIVE WITH NO KEY IS WHOLE WHEN IT SAYS A BROKER HOLDS THEM, and written at the
// schema that can say so, which an older billet refuses by version rather than as
// truncated.
func TestAnArchiveOfADeploymentOnABrokerCarriesNoKey(t *testing.T) {
	d := newDeployment(t)
	dest := filepath.Join(t.TempDir(), "archive")

	m := brokerBackupTo(t, d, dest)

	if m.Schema != 5 || !m.KeysWithBroker {
		t.Fatalf("schema %d, keys with broker %v; want 5 and true", m.Schema, m.KeysWithBroker)
	}

	a, err := Open(t.Context(), dest)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	if _, held := a.Entry(EntryAppKey); held {
		t.Fatal("the archive of a deployment holding no key carries one")
	}
}

// THE CLAIM IS WHAT DECIDES, IN BOTH DIRECTIONS. An archive not saying so is not whole
// without its key, and one saying so may not carry one.
func TestAMissingKeyIsATruncationUnlessTheManifestSaysABrokerHoldsIt(t *testing.T) {
	d := newDeployment(t)
	dest := filepath.Join(t.TempDir(), "archive")

	brokerBackupTo(t, d, dest)

	rewriteManifest(t, dest, func(m *Manifest) { m.KeysWithBroker = false })

	if _, err := Open(t.Context(), dest); err == nil || !strings.Contains(err.Error(), EntryAppKey) {
		t.Fatalf("an archive with no key and no claim answered %v", err)
	}

	withKey := filepath.Join(t.TempDir(), "archive")
	backupTo(t, d, withKey)

	rewriteManifest(t, withKey, func(m *Manifest) { m.Schema, m.KeysWithBroker = 5, true })

	if _, err := Open(t.Context(), withKey); err == nil || !strings.Contains(err.Error(), EntryAppKey) {
		t.Fatalf("an archive claiming a broker and carrying a key answered %v", err)
	}
}

// A SCHEMA THAT CANNOT EXPRESS THE CLAIM MUST NOT BE READ AS MAKING IT.
func TestAnOlderSchemaCannotClaimABroker(t *testing.T) {
	d := newDeployment(t)
	dest := filepath.Join(t.TempDir(), "archive")

	brokerBackupTo(t, d, dest)

	rewriteManifest(t, dest, func(m *Manifest) { m.Schema = 4 })

	if _, err := Open(t.Context(), dest); err == nil || !strings.Contains(err.Error(), "broker") {
		t.Fatalf("a schema 4 manifest claiming a broker answered %v", err)
	}
}

// A BACKUP REFUSES A KEY IT WAS TOLD THERE IS NONE OF, and no key it was not.
func TestABackupHoldsTheBrokerClaimToItsKeys(t *testing.T) {
	d := newDeployment(t)

	_, err := Write(t.Context(), BackupRequest{
		Dest: filepath.Join(t.TempDir(), "archive"), StateDir: d.stateDir, ConfigPath: d.configPath,
		DeploymentID: d.id, GitHub: d.github, AppKeyPEM: d.appKey, KeysWithBroker: true,
		ConfigBody:     []byte("server: {}\n"),
		ExternalLedger: &ExternalLedger{Backend: "postgres", DSNEnv: "BILLET_STATE_DSN"},
		Now:            nowStub, Hostname: "test-host",
	})
	if err == nil || !strings.Contains(err.Error(), "broker") {
		t.Fatalf("a key beside the claim answered %v", err)
	}

	_, err = Write(t.Context(), BackupRequest{
		Dest: filepath.Join(t.TempDir(), "archive"), StateDir: d.stateDir, ConfigPath: d.configPath,
		DeploymentID: d.id, GitHub: d.github,
		ConfigBody:     []byte("server: {}\n"),
		ExternalLedger: &ExternalLedger{Backend: "postgres", DSNEnv: "BILLET_STATE_DSN"},
		Now:            nowStub, Hostname: "test-host",
	})
	if err == nil || !strings.Contains(err.Error(), "App private key is part of the deployment unit") {
		t.Fatalf("no key and no claim answered %v", err)
	}
}

func brokerArchive(t *testing.T) *Archive {
	t.Helper()

	d := newDeployment(t)
	dest := filepath.Join(t.TempDir(), "archive")

	brokerBackupTo(t, d, dest)

	a, err := Open(t.Context(), dest)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	return a
}

// A RESTORE ONTO A HOST ON A BROKER INSTALLS NO KEY, whichever archive it is, and one
// with no key cannot be restored onto a host that wants one.
func TestARestorePlansNoKeyForAHostOnABroker(t *testing.T) {
	a := brokerArchive(t)

	onBroker := externalTarget(t, true)
	onBroker.GitHub = a.Manifest.GitHub
	onBroker.AppKeyPath = ""
	onBroker.KeysWithBroker = true

	p, err := PlanRestore(t.Context(), a, onBroker)
	if err != nil {
		t.Fatalf("PlanRestore: %v", err)
	}

	var planned []string
	for _, act := range p.Actions {
		planned = append(planned, act.Entry)
	}

	if len(p.Refusals) != 0 {
		t.Errorf("a restore onto a host on a broker is refused: %+v", p.Refusals)
	}

	if slices.Contains(planned, EntryAppKey) {
		t.Errorf("a restore onto a host holding no key plans one: %v", planned)
	}
	if !slices.Contains(planned, EntryIdentity) {
		t.Errorf("the restore plans %v, which is not a deployment", planned)
	}

	wantsKey := externalTarget(t, true)
	wantsKey.GitHub = a.Manifest.GitHub

	refused, err := PlanRestore(t.Context(), a, wantsKey)
	if err != nil {
		t.Fatalf("PlanRestore: %v", err)
	}

	if got := refusalsMentioning(refused, "carries no App key"); len(got) != 1 {
		t.Fatalf("an archive with no key onto a host wanting one was refused for %+v", refused.Refusals)
	}
}

// A HOST THAT HAS MOVED TO A BROKER TAKES NO KEY FROM AN OLDER ARCHIVE, though the
// archive holds one: the config says this host holds none, and a restore does not
// make it hold one.
func TestAHostOnABrokerTakesNoKeyFromAnOlderArchive(t *testing.T) {
	a := externalArchive(t)

	target := externalTarget(t, true)
	target.AppKeyPath = ""
	target.KeysWithBroker = true

	p, err := PlanRestore(t.Context(), a, target)
	if err != nil {
		t.Fatalf("PlanRestore: %v", err)
	}

	if len(p.Refusals) != 0 {
		t.Errorf("a host on a broker refused an older archive: %+v", p.Refusals)
	}

	for _, act := range p.Actions {
		if act.Entry == EntryAppKey {
			t.Fatalf("a host on a broker was given the archive's key: %+v", act)
		}
	}
}
