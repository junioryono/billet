package e2e

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"log/slog"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/deployarchive"
	"github.com/junioryono/billet/internal/nodeclient"
	"github.com/junioryono/billet/internal/nodeplane"
	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/wirecert"
	"github.com/junioryono/billet/internal/wiring"
)

// The rehearsal: a deployment is backed up, restored where nothing has seen it,
// and the fleet that trusted the old control plane connects to the new one.
//
// EVERY OTHER TEST OF THIS AREA IS A READING. internal/deployarchive proves an
// archive round-trips into a directory that opens as a control plane, and
// cmd/billet proves the commands refuse what they should — neither is a
// rehearsal, and that is the gap: what has never happened is taking an archive
// off a deployment, putting it on a machine that has never seen it, starting a
// control plane, and having a node connect. The drift this catches is between
// the archive format and what a control plane will ACCEPT, which is invisible
// until the day somebody needs it.
//
// WHAT IS REAL HERE AND WHAT IS NOT. The ledger, the deployment identity, the
// authority, the archive, the node wire, its TLS and the node client are all the
// production ones, assembled through internal/wiring exactly as `billet server`
// assembles them. GitHub is absent rather than faked, because none of the four
// things a backup captures needs it — and `billet server` cannot START without
// reaching GitHub (server.Run returns an error when EnsureScaleSet fails, and no
// config can point the client elsewhere), so the restored plane here is
// assembled in-process. The packaged-Linux half — the service account,
// StateDirectory ownership, and the real binary's `local backup`/`restore` — is
// scripts/test-restore-rehearsal.sh.
//
// NO CONTAINER RUNTIME IS INVOLVED, deliberately: nothing here launches
// anything, so this must not skip on a machine without docker the way the rest
// of this package does.

// rehearsalNode is the host these tests speak for. It is issued a certificate by
// the ORIGINAL deployment and never re-enrolled, which is the whole point.
const rehearsalNode = "rehearsal-1"

// rehearsalTiers is the catalogue both control planes are built from. A restored
// plane reads its tiers from the operator's config rather than from the archive,
// so one catalogue on both sides is what a restore onto a host carrying the same
// billet.yaml looks like.
func rehearsalTiers() []config.Tier {
	return []config.Tier{{
		Label:       "billet-2vcpu-rehearsal",
		Provider:    config.ProviderDocker,
		GuestOS:     config.GuestLinux,
		VCPU:        2,
		Memory:      2 * config.GiB,
		Disk:        10 * config.GiB,
		Image:       testImage,
		RunnerGroup: testGroup,
		Trust:       config.WorkloadUntrusted,
	}}
}

// controlPlane is a running one: the ledger, the allocator, the authority and
// the node wire, over the state directory it was given.
type controlPlane struct {
	dir        string
	deployment string
	db         *state.DB
	alloc      *alloc.Allocator
	authority  wirecert.Serving
	url        string
}

// startControlPlane brings one up over dir, the way `billet server` does.
//
// THROUGH wiring.BuildNodeWire RATHER THAN BY HAND, because that seam is as much
// under test as the archive is: what the server PRESENTS, what it ACCEPTS and
// what renewal SIGNS with are three answers that have to come from one read of
// the restored authority, and a hand-assembled wire in a test is a different
// program from the one an operator runs.
func startControlPlane(t *testing.T, dir string) *controlPlane {
	t.Helper()

	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("create %s: %v", dir, err)
	}

	db, err := state.Open(t.Context(), dir)
	if err != nil {
		t.Fatalf("state.Open(%s): %v", dir, err)
	}

	closeDB := sync.OnceFunc(func() { _ = db.Close() })

	t.Cleanup(closeDB)

	deployment, err := state.DeploymentID(dir)
	if err != nil {
		t.Fatalf("DeploymentID: %v", err)
	}

	tiers := rehearsalTiers()

	a, err := alloc.New(db, alloc.Limits{MaxVCPU: 8, MaxMemory: 16 * config.GiB}, tiers)
	if err != nil {
		t.Fatalf("alloc.New: %v", err)
	}

	// EXACTLY WHAT runServer DOES BEFORE IT SERVES. Liveness is the plane's
	// judgement and a plane that has just started has none, so rows left by the
	// previous process must not back advertisements for machines this one has
	// never heard from. It is also what makes the restored plane's acceptance of
	// the node below a real registration rather than a row the archive carried.
	if err := a.ForgetEveryNode(t.Context()); err != nil {
		t.Fatalf("ForgetEveryNode: %v", err)
	}

	log := slog.New(slog.DiscardHandler)

	plane := nodeplane.New(log, deployment, a.LeaseTTL(),
		nodeplane.WithRegistrar(a),
		nodeplane.WithTierCatalog(tiers))

	wire, err := wiring.BuildNodeWire(wiring.NodeWireRequest{
		StateDir:    dir,
		Deployment:  deployment,
		Hosts:       []string{"127.0.0.1"},
		Log:         log,
		Plane:       plane,
		Leases:      a,
		Revocations: a,
		Enrollments: a,
		CachePolicy: db,
	})
	if err != nil {
		t.Fatalf("BuildNodeWire: %v", err)
	}

	srv := httptest.NewUnstartedServer(wire.Handler)
	srv.TLS = wire.TLS
	srv.StartTLS()

	t.Cleanup(srv.Close)

	// The same one read `billet ca issue` takes, so a bundle minted below carries
	// what an operator's bundle carries: signed by the ISSUING authority, and
	// trusting the whole bundle.
	authority, err := wirecert.LoadServing(dir, deployment)
	if err != nil {
		t.Fatalf("LoadServing: %v", err)
	}

	return &controlPlane{
		dir: dir, deployment: deployment, db: db, alloc: a,
		authority: authority, url: srv.URL,
	}
}

// issueBundle mints a node bundle the way `billet ca issue` does, and records it
// the way that command does, so the ledger carries the admission trail a restore
// has to bring back.
func (c *controlPlane) issueBundle(t *testing.T, name string) wirecert.Bundle {
	t.Helper()

	bundle, err := c.authority.Issuing.IssueNode(name)
	if err != nil {
		t.Fatalf("issue a certificate for %s: %v", name, err)
	}

	// THE WHOLE TRUST BUNDLE, not the authority that signed the leaf — the rule
	// `billet ca issue` follows, because during an overlap the new authority
	// issues and the old one signs what the plane presents.
	bundle.CAPEM = c.authority.Trust

	leaf, err := wirecert.LeafOf(bundle)
	if err != nil {
		t.Fatalf("read back the certificate issued to %s: %v", name, err)
	}

	if err := c.alloc.RecordIssuedCert(t.Context(), alloc.IssuedCert{
		Serial:   wirecert.Serial(leaf),
		Node:     name,
		Source:   alloc.CertIssued,
		NotAfter: leaf.NotAfter.UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("record the issued certificate: %v", err)
	}

	if _, err := c.alloc.RecordIssued(t.Context(), name,
		wirecert.FingerprintOfCert(leaf), string(bundle.CertPEM)); err != nil {
		t.Fatalf("record the admission of %s: %v", name, err)
	}

	return bundle
}

// dial builds the node's client from a bundle, with no re-enrollment of any kind.
func (c *controlPlane) dial(t *testing.T, bundle wirecert.Bundle, name string) *nodeclient.Client {
	t.Helper()

	conf, err := wirecert.ClientTLS(bundle)
	if err != nil {
		t.Fatalf("client tls: %v", err)
	}

	client, err := nodeclient.New(nodeclient.Options{Base: c.url, Node: name, TLS: conf})
	if err != nil {
		t.Fatalf("nodeclient.New: %v", err)
	}

	return client
}

// registerAs is what a node does on its first poll, with the deployment it read
// out of its own certificate.
func registerAs(t *testing.T, client *nodeclient.Client, deployment string) error {
	t.Helper()

	return client.Register(t.Context(), nodeclient.Registration{
		Provider:   config.ProviderDocker,
		GuestOS:    []config.GuestOS{config.GuestLinux},
		Deployment: deployment,
		VCPU:       testNodeVCPU,
		Memory:     testNodeMemory,
	})
}

// usage asks what the ledger says is held. Through a helper because Usage
// returns the ZERO value beside its error, so an assertion that capacity
// matches would pass on a call that failed.
func usage(t *testing.T, a *alloc.Allocator) alloc.Usage {
	t.Helper()

	u, err := a.Usage(t.Context())
	if err != nil {
		t.Fatalf("Usage: %v", err)
	}

	return u
}

// admitted names every node this ledger has admitted, with the fingerprint an
// operator compared.
func admitted(t *testing.T, a *alloc.Allocator) map[string]string {
	t.Helper()

	rows, err := a.Enrollments(t.Context(), alloc.EnrollApproved)
	if err != nil {
		t.Fatalf("Enrollments: %v", err)
	}

	out := make(map[string]string, len(rows))
	for i := range rows {
		out[rows[i].Name] = rows[i].Fingerprint
	}

	return out
}

// appKeyPEM is one throwaway App key. Generated once for the package: 2048 bits
// per test dominates the runtime and proves nothing extra, since what these
// tests are about is which BYTES land where.
var appKeyPEM = sync.OnceValues(func() ([]byte, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}

	return pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	}), nil
})

// backUp captures a deployment exactly as `billet local backup` does: through
// OpenAdmin, against a control plane that is still RUNNING.
func backUp(t *testing.T, c *controlPlane, appKey []byte, dest string) deployarchive.Manifest {
	t.Helper()

	// A SECOND TARGET'S KEY TRAVELS WITH THE FIRST. A deployment serving several
	// GitHub targets holds one App key per target, and an archive that carried
	// only the first would restore a control plane that serves one owner and
	// refuses the other; the same second key is what newTarget expects back.
	second, err := secondTargetKey()
	if err != nil {
		t.Fatalf("generate the second target's key: %v", err)
	}

	admin, err := state.OpenAdmin(t.Context(), c.dir)
	if err != nil {
		t.Fatalf("OpenAdmin: %v", err)
	}

	defer func() { _ = admin.Close() }()

	m, err := deployarchive.Write(t.Context(), deployarchive.BackupRequest{
		Dest:         dest,
		StateDir:     c.dir,
		DeploymentID: c.deployment,
		GitHub:       rehearsalApp,
		AppKeyPEM:    appKey,
		Targets: []deployarchive.TargetKey{
			{Name: rehearsalSecondTarget, GitHub: rehearsalSecondApp, AppKeyPEM: second},
		},
		Snapshot: admin.SnapshotInto,
		Now:      time.Now,
		Hostname: "rehearsal-source",
	})
	if err != nil {
		t.Fatalf("deployarchive.Write: %v", err)
	}

	return m
}

// rehearsalApp is the App identity the archive records and the target's config
// declares. A restore refuses a key that would land beside a config naming a
// different App.
var rehearsalApp = deployarchive.GitHubIdentity{Org: "acme", AppID: 12345, InstallationID: 67890}

// rehearsalSecondApp is the second target's App: a repository, because that is
// the target kind the archive did not carry before targets existed.
var (
	rehearsalSecondTarget = "personal"
	rehearsalSecondApp    = deployarchive.GitHubIdentity{
		Repository: "someone/widgets", AppID: 12346, InstallationID: 67891,
	}
)

// secondTargetKey is the second target's App key, generated once so the
// archive and the assertion after the restore hold the same bytes.
var secondTargetKey = sync.OnceValues(appKeyPEM)

// restoreOnto publishes an archive into a directory that has never held a
// deployment, through the planner and executor the command drives.
func restoreOnto(t *testing.T, archiveDir string, target deployarchive.Target) {
	t.Helper()

	a, err := deployarchive.Open(t.Context(), archiveDir)
	if err != nil {
		t.Fatalf("deployarchive.Open: %v", err)
	}

	plan, err := deployarchive.PlanRestore(t.Context(), a, target)
	if err != nil {
		t.Fatalf("PlanRestore: %v", err)
	}

	if len(plan.Refusals) > 0 {
		t.Fatalf("a restore onto an untouched directory was refused: %v", plan.Refusals)
	}

	res, err := deployarchive.Execute(t.Context(), deployarchive.RestoreRequest{
		Plan:          plan,
		InstallAppKey: installAppKeyExclusively,
		Now:           time.Now,
		Actor:         "rehearsal",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if len(res.Installed) == 0 {
		t.Fatal("a restore onto an untouched directory installed nothing")
	}
}

// newTarget is the other machine: a state directory that has never held a
// deployment, beside a config naming the same App.
func newTarget(t *testing.T, root string) deployarchive.Target {
	t.Helper()

	stateDir := filepath.Join(root, "state")

	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatalf("prepare the target: %v", err)
	}

	return deployarchive.Target{
		ConfigPath: filepath.Join(root, "billet.yaml"),
		StateDir:   stateDir,
		AppKeyPath: filepath.Join(root, "app-private-key.pem"),
		GitHub:     rehearsalApp,
		Targets: []deployarchive.TargetPath{{
			Name:       rehearsalSecondTarget,
			AppKeyPath: filepath.Join(root, "app-private-key-personal.pem"),
			GitHub:     rehearsalSecondApp,
		}},
	}
}

// installAppKeyExclusively stands in for the command layer's no-clobber
// installer. It REFUSES an occupied destination, which is the property the
// executor depends on: a stand-in that overwrote would make the restore's
// central refusal untestable from here.
func installAppKeyExclusively(path string, body []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}

	defer func() { _ = f.Close() }()

	if _, err := f.Write(body); err != nil {
		return err
	}

	return f.Sync()
}

// TestARestoredDeploymentServesTheFleetThatTrustedTheOldOne is the rehearsal.
//
// A deployment is stood up and used — a node registers over the real wire, a
// certificate is issued and recorded, capacity is escrowed and bound — then
// backed up while it is still running, restored onto a directory that has never
// seen it, and a control plane is started THERE. The same node, holding the
// certificate the ORIGINAL deployment issued and having enrolled with nothing,
// connects to it and is accepted.
func TestARestoredDeploymentServesTheFleetThatTrustedTheOldOne(t *testing.T) {
	root := t.TempDir()

	// ---- The deployment somebody is running.
	source := startControlPlane(t, filepath.Join(root, "source"))

	bundle := source.issueBundle(t, rehearsalNode)

	if err := registerAs(t, source.dial(t, bundle, rehearsalNode), source.deployment); err != nil {
		t.Fatalf("the node was refused by the deployment that issued its certificate: %v", err)
	}

	// A lease, so the ledger carries capacity accounting rather than only rows
	// about identity. What must survive is the CHARGE: a restored plane that
	// forgot it would resell a machine already holding work.
	lease, err := source.alloc.Reserve(t.Context(), rehearsalTiers()[0].Label)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	if err := source.alloc.Bind(t.Context(), lease.ID, lease.Epoch, rehearsalNode); err != nil {
		t.Fatalf("Bind: %v", err)
	}

	wantUsage := usage(t, source.alloc)
	wantAdmitted := admitted(t, source.alloc)

	if wantUsage.Leases == 0 || len(wantAdmitted) == 0 {
		t.Fatalf("the source deployment is not populated (usage %+v, admitted %v); the rest of "+
			"this test would prove nothing", wantUsage, wantAdmitted)
	}

	key, err := appKeyPEM()
	if err != nil {
		t.Fatalf("generate an App key: %v", err)
	}

	// ---- The backup, taken against the live plane above.
	archiveDir := filepath.Join(root, "archive")

	manifest := backUp(t, source, key, archiveDir)

	if manifest.DeploymentID != source.deployment {
		t.Fatalf("the archive names deployment %s, want %s",
			manifest.DeploymentID, source.deployment)
	}

	// ---- The other machine. Nothing here has ever heard of this deployment.
	target := newTarget(t, filepath.Join(root, "elsewhere"))

	restoreOnto(t, archiveDir, target)

	// ---- And a control plane on it.
	restored := startControlPlane(t, target.StateDir)

	if restored.deployment != source.deployment {
		t.Fatalf("the restored control plane is deployment %s, not %s — every instance the old "+
			"one launched is invisible to it", restored.deployment, source.deployment)
	}

	// THE HEADLINE: the node reconnects with the certificate the ORIGINAL
	// deployment issued, having enrolled with nothing and been told nothing.
	if err := registerAs(t, restored.dial(t, bundle, rehearsalNode), source.deployment); err != nil {
		t.Fatalf("the restored control plane refused the fleet that trusted the old one: %v", err)
	}

	// And it holds what the old one held.
	if got := usage(t, restored.alloc); got != wantUsage {
		t.Errorf("the restored ledger holds %+v, want %+v; capacity a restored plane cannot see "+
			"is capacity it resells", got, wantUsage)
	}

	gotAdmitted := admitted(t, restored.alloc)
	for name, fingerprint := range wantAdmitted {
		if gotAdmitted[name] != fingerprint {
			t.Errorf("the restored ledger admits %s as %q, want %q", name,
				gotAdmitted[name], fingerprint)
		}
	}

	// The App key is the one GitHub will not reissue.
	installed, err := os.ReadFile(target.AppKeyPath)
	if err != nil {
		t.Fatalf("read the restored App key: %v", err)
	}

	if !bytes.Equal(installed, key) {
		t.Error("the restored App key is not byte-identical to the original")
	}

	// AND THE SECOND TARGET'S, at the path the restored config names for it.
	second, err := secondTargetKey()
	if err != nil {
		t.Fatalf("the second target's key: %v", err)
	}

	installedSecond, err := os.ReadFile(target.Targets[0].AppKeyPath)
	if err != nil {
		t.Fatalf("read the restored second target's App key: %v", err)
	}

	if !bytes.Equal(installedSecond, second) {
		t.Error("the restored second target's App key is not byte-identical to the original")
	}
}

// TestARestoredControlPlaneRefusesANodeFromAnotherDeployment is the other
// direction, without which the test above would also pass against a wire that
// accepts everybody.
//
// THE REFUSAL IS THE HANDSHAKE'S, not a handler's. A certificate from an
// authority this deployment never had cannot reach billet's HTTP server at all,
// which is why the assertion is on the transport error rather than on a status.
func TestARestoredControlPlaneRefusesANodeFromAnotherDeployment(t *testing.T) {
	root := t.TempDir()

	source := startControlPlane(t, filepath.Join(root, "source"))

	key, err := appKeyPEM()
	if err != nil {
		t.Fatalf("generate an App key: %v", err)
	}

	archiveDir := filepath.Join(root, "archive")

	backUp(t, source, key, archiveDir)

	target := newTarget(t, filepath.Join(root, "elsewhere"))

	restoreOnto(t, archiveDir, target)

	restored := startControlPlane(t, target.StateDir)

	// A whole other deployment, with its own authority — a second billet
	// somewhere else rather than a forgery.
	foreign, err := wirecert.LoadOrCreateCA(t.TempDir(), "ffffffffffffffffffffffffffffffff")
	if err != nil {
		t.Fatalf("create a foreign authority: %v", err)
	}

	stranger, err := foreign.IssueNode(rehearsalNode)
	if err != nil {
		t.Fatalf("issue a foreign certificate: %v", err)
	}

	// ITS OWN AUTHORITY *AND* THE RESTORED ONE, so nothing on the CLIENT side can
	// produce the failure. A bundle carrying only the restored authority is
	// refused by wirecert.ClientTLS before a connection is made — the leaf does
	// not verify against it — and one carrying only its own would have the client
	// reject the server. With both in its ca.crt the stranger dials happily and
	// trusts what answers, so the refusal below can only be the server's.
	stranger.CAPEM = append(append([]byte{}, foreign.CertPEM()...), restored.authority.Trust...)

	err = registerAs(t, restored.dial(t, stranger, rehearsalNode), restored.deployment)
	if err == nil {
		t.Fatal("a node holding another deployment's certificate registered with the restored " +
			"control plane")
	}

	// MEASURED RATHER THAN READ: crypto/tls's alert type is unexported on this
	// path, so errors.As against tls.AlertError is false. What the client sees is
	// the peer's alert arriving as a transport error.
	op, ok := errors.AsType[*net.OpError](err)
	if !ok || op.Op != "remote error" {
		t.Fatalf("the refusal was %v (%T), want the server's TLS alert; an error from anywhere "+
			"else would let this test pass against a plane that never checked", err, err)
	}
}
