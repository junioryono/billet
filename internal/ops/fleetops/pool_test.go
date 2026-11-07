package fleetops

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/cli"
	"github.com/junioryono/billet/internal/config"
)

func poolEnv() (cli.Env, *bytes.Buffer) {
	var out bytes.Buffer

	return cli.Env{Stdout: &out, Stderr: &out, Stdin: strings.NewReader(""), Getenv: os.Getenv}, &out
}

// `billet pool` LISTS EVERY ORDER, says what an uncertain one asks of a person, and
// `billet pool abandon` closes only an uncertain order, with the reason recorded.
func TestThePoolCommandListsOrdersAndClosesOnlyAnUncertainOne(t *testing.T) {
	stateDir := t.TempDir()
	cfg := writeCAConfig(t, stateDir)
	ctx := t.Context()

	env, out := poolEnv()
	if err := Pool(ctx, env, []string{"--config", cfg}); err != nil || !strings.Contains(out.String(), "placed no order") {
		t.Fatalf("an empty pool: %v\n%s", err, out)
	}

	a, closeDB, err := ControlPlaneAllocator(ctx, cfg)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	for _, id := range []string{"p-placed", "p-lost"} {
		if err := a.RecordPoolOrder(ctx, alloc.PoolOrder{
			ID: id, Source: "aws-metal", Scope: "aws:us-west-2:subnet-0abc:us-west-2a", Shape: "z1d.metal",
			Location: "us-west-2a", Arch: config.ArchX64,
		}); err != nil {
			t.Fatalf("record: %v", err)
		}
	}
	if err := a.PoolOrderPlaced(ctx, "p-placed", "i-0abc"); err != nil {
		t.Fatalf("placed: %v", err)
	}
	if err := a.PoolOrderUncertain(ctx, "p-lost", "the answer was\nlost"); err != nil {
		t.Fatalf("uncertain: %v", err)
	}
	closeDB()

	env, out = poolEnv()
	if err := Pool(ctx, env, []string{"--config", cfg}); err != nil {
		t.Fatalf("list: %v", err)
	}
	rows := map[string][]string{}
	for _, line := range strings.Split(out.String(), "\n") {
		if f := strings.Fields(line); len(f) > 0 {
			rows[f[0]] = f
		}
	}
	if r := rows["p-placed"]; len(r) < 9 || r[1] != "x64" || r[2] != "aws-metal" ||
		r[3] != "aws:us-west-2:subnet-0abc:us-west-2a" || r[6] != "placed" || r[7] != "i-0abc" {
		t.Errorf("the p-placed row = %q", r)
	}
	if r := rows["p-lost"]; len(r) < 9 || r[6] != "uncertain" || r[7] != "-" {
		t.Errorf("the p-lost row = %q", r)
	}
	for _, want := range []string{"the answer was lost", "1 uncertain", "billet pool abandon"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the listing has no %q:\n%s", want, out)
		}
	}

	env, out = poolEnv()
	if err := Pool(ctx, env, []string{"abandon", "--config", cfg, "--reason", "no such instance", "p-placed"}); err == nil ||
		!strings.Contains(err.Error(), "not uncertain") {
		t.Fatalf("abandoning a placed order: %v\n%s", err, out)
	}
	if err := Pool(ctx, env, []string{"abandon", "--config", cfg, "p-lost"}); err == nil || !strings.Contains(err.Error(), "usage:") {
		t.Fatalf("an order abandoned without a reason answered %v", err)
	}
	if err := Pool(ctx, env, []string{"abandon", "--config", cfg, "--reason", "no such instance", "p-lost"}); err != nil {
		t.Fatalf("abandon: %v", err)
	}
	env, out = poolEnv()
	if err := Pool(ctx, env, []string{"--config", cfg}); err != nil ||
		!strings.Contains(out.String(), "abandoned") || !strings.Contains(out.String(), "no such instance") ||
		strings.Contains(out.String(), "uncertain:") {
		t.Fatalf("after abandoning: %v\n%s", err, out)
	}
}

// A HELD JOIN IS LISTED WITH ITS REASON AND WHAT A PERSON DOES ABOUT IT, on one line
// whatever the reason held; an order held for no one is not listed as held.
func TestThePoolCommandSaysWhichJoinsAreHeld(t *testing.T) {
	stateDir := t.TempDir()
	cfg := writeCAConfig(t, stateDir)
	ctx := t.Context()
	a, closeDB, err := ControlPlaneAllocator(ctx, cfg)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	for _, id := range []string{"p-held", "p-fine"} {
		if err := a.RecordPoolOrder(ctx, alloc.PoolOrder{
			ID: id, Source: "aws-metal", Scope: "aws:us-west-2:subnet-0abc:us-west-2a", Shape: "z1d.metal",
			Location: "us-west-2a", Arch: config.ArchX64,
		}); err != nil {
			t.Fatalf("record: %v", err)
		}
		if err := a.PoolOrderPlaced(ctx, id, "i-"+id); err != nil {
			t.Fatalf("placed: %v", err)
		}
	}
	if err := a.HoldPoolJoin(ctx, "p-held", "the console names\nanother key", heldAt(t, a, "p-held")); err != nil {
		t.Fatalf("hold: %v", err)
	}
	closeDB()

	env, out := poolEnv()
	if err := Pool(ctx, env, []string{"--config", cfg}); err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, want := range []string{"held p-held: the console names another key", "billet nodes approve"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the listing has no %q:\n%s", want, out)
		}
	}
	if strings.Contains(out.String(), "held p-fine") {
		t.Errorf("an order held for no one is listed as held:\n%s", out)
	}
}

func heldAt(t *testing.T, a *alloc.Allocator, order string) time.Time {
	t.Helper()
	o, err := a.ReadPoolOrder(t.Context(), order)
	if err != nil {
		t.Fatal(err)
	}

	return o.Updated
}

// `billet pool release` SEALS A HOST NOTHING JOINED FROM AT ONCE, AND SAYS SO; it
// refuses without a reason, and a second release of the same order is refused. The
// listing then shows the order sealed.
func TestThePoolCommandReleasesAHost(t *testing.T) {
	stateDir := t.TempDir()
	cfg := writeCAConfig(t, stateDir)
	ctx := t.Context()
	a, closeDB, err := ControlPlaneAllocator(ctx, cfg)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := a.RecordPoolOrder(ctx, alloc.PoolOrder{
		ID: "p-idle", Source: "aws-metal", Scope: "aws:us-west-2:subnet-0abc:us-west-2a", Shape: "z1d.metal",
		Location: "us-west-2a", Arch: config.ArchX64,
	}); err != nil {
		t.Fatalf("record: %v", err)
	}
	if err := a.PoolOrderPlaced(ctx, "p-idle", "i-idle"); err != nil {
		t.Fatalf("placed: %v", err)
	}
	closeDB()

	env, out := poolEnv()
	if err := Pool(ctx, env, []string{"release", "--config", cfg, "p-idle"}); err == nil ||
		!strings.Contains(err.Error(), "no release of a joined host waiting to be sealed") {
		t.Fatalf("a release without a reason of an order nothing was asked of answered %v", err)
	}
	if err := Pool(ctx, env, []string{"release", "--config", cfg, "--reason", "it never\nbooted", "p-idle"}); err != nil ||
		!strings.Contains(out.String(), "Sealed p-idle") {
		t.Fatalf("release: %v\n%s", err, out)
	}
	if err := Pool(ctx, env, []string{"release", "--config", cfg, "--reason", "again", "p-idle"}); err == nil {
		t.Fatal("a second release was taken")
	}
	env, out = poolEnv()
	if err := Pool(ctx, env, []string{"--config", cfg}); err != nil {
		t.Fatalf("list: %v", err)
	}
	rows := map[string][]string{}
	for _, line := range strings.Split(out.String(), "\n") {
		if f := strings.Fields(line); len(f) > 0 && rows[f[0]] == nil {
			rows[f[0]] = f
		}
	}
	if r := rows["p-idle"]; len(r) < 7 || r[6] != "sealed" {
		t.Fatalf("the p-idle row = %q\n%s", r, out)
	}

	// A SEALED OR RELEASED ORDER IS CLOSED BY HAND, with a reason, and no other.
	a, closeDB, err = ControlPlaneAllocator(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.RecordPoolOrder(ctx, alloc.PoolOrder{
		ID: "p-busy", Source: "aws-metal", Scope: "aws:us-west-2:subnet-0abc:us-west-2a", Shape: "z1d.metal",
		Location: "us-west-2a", Arch: config.ArchX64,
	}); err != nil {
		t.Fatal(err)
	}
	if err := a.PoolOrderPlaced(ctx, "p-busy", "i-busy"); err != nil {
		t.Fatal(err)
	}
	closeDB()
	if err := Pool(ctx, env, []string{"close", "--config", cfg, "--reason", "looked", "p-busy"}); err == nil ||
		!strings.Contains(err.Error(), "neither sealed nor released") {
		t.Fatalf("closing a placed order answered %v", err)
	}
	if err := Pool(ctx, env, []string{"close", "--config", cfg, "p-idle"}); err == nil || !strings.Contains(err.Error(), "usage:") {
		t.Fatalf("a closure without a reason answered %v", err)
	}
	env, out = poolEnv()
	if err := Pool(ctx, env, []string{"close", "--config", cfg, "--reason", "the console lists no such server", "p-idle"}); err != nil ||
		!strings.Contains(out.String(), "Closed p-idle") {
		t.Fatalf("close: %v\n%s", err, out)
	}
	env, out = poolEnv()
	if err := Pool(ctx, env, []string{"--config", cfg}); err != nil || !strings.Contains(out.String(), "closed by a person") {
		t.Fatalf("the listing after a closure: %v\n%s", err, out)
	}
}

// A SECOND `billet pool release` OF A HOST ALREADY OUT OF PLACEMENT TRIES THE SEAL ITSELF,
// so a deployment whose pool is not running can still finish a release: a host admitted
// but never registered is sealed, and one whose node registered and was never
// decommissioned on proof is refused with the procedure that proves it.
func TestASecondReleaseSealsWhatTheLedgerProves(t *testing.T) {
	stateDir := t.TempDir()
	cfg := writeCAConfig(t, stateDir)
	ctx := t.Context()
	a, closeDB, err := ControlPlaneAllocator(ctx, cfg)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	for i, id := range []string{"p-quiet", "p-live"} {
		if err := a.RecordPoolOrder(ctx, alloc.PoolOrder{
			ID: id, Source: "aws-metal", Scope: "aws:us-west-2:subnet-0abc:us-west-2a", Shape: "z1d.metal",
			Location: "us-west-2a", Arch: config.ArchX64,
		}); err != nil {
			t.Fatal(err)
		}
		if err := a.PoolOrderPlaced(ctx, id, "i-"+id); err != nil {
			t.Fatal(err)
		}
		tok, err := a.NewPoolJoinToken(ctx, id, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		o, err := a.ReadPoolOrder(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := a.AdmitPoolEnrollment(ctx, alloc.PoolAdmission{
			Order: id, Fingerprint: "SHA256:" + id, CSRPEM: "csr", Token: tok, CertPEM: "cert",
			Serial: string(rune('a' + i)), NotAfter: "2027-11-06T00:00:00Z", Seen: o.Updated,
		}); err != nil {
			t.Fatalf("admit %s: %v", id, err)
		}
	}
	if _, err := a.RegisterNode(ctx, alloc.NodeRegistration{Name: alloc.PoolNodeName("p-live"),
		Provider: config.ProviderFirecracker, VCPU: 8, Memory: 32 * config.GiB,
		Architectures: []config.Arch{config.ArchX64}}); err != nil {
		t.Fatal(err)
	}
	closeDB()

	for _, id := range []string{"p-quiet", "p-live"} {
		env, out := poolEnv()
		if err := Pool(ctx, env, []string{"release", "--config", cfg, "--reason", "the pool shrinks", id}); err != nil ||
			!strings.Contains(out.String(), "Released "+id+" from placement") {
			t.Fatalf("the first release of %s: %v\n%s", id, err, out)
		}
	}
	env, out := poolEnv()
	if err := Pool(ctx, env, []string{"release", "--config", cfg, "p-quiet"}); err != nil ||
		!strings.Contains(out.String(), "Sealed p-quiet: its host was proved to run nothing") {
		t.Fatalf("the second release of a host whose node never registered: %v\n%s", err, out)
	}
	err = Pool(ctx, env, []string{"release", "--config", cfg, "--reason", "again", "p-live"})
	if err == nil || !strings.Contains(err.Error(), "cannot be sealed yet") {
		t.Fatalf("a second release with a reason of a registered host never decommissioned answered %v", err)
	}
	err = Pool(ctx, env, []string{"release", "--config", cfg, "p-live"})
	if err == nil || !strings.Contains(err.Error(), "cannot be sealed yet") ||
		!strings.Contains(err.Error(), "billet nodes decommission pool-p-live") {
		t.Fatalf("the second release of a registered host never decommissioned answered %v", err)
	}

	env, out = poolEnv()
	if err := Pool(ctx, env, []string{"--config", cfg}); err != nil {
		t.Fatalf("list: %v", err)
	}
	rows := map[string][]string{}
	for _, line := range strings.Split(out.String(), "\n") {
		if f := strings.Fields(line); len(f) > 0 && rows[f[0]] == nil {
			rows[f[0]] = f
		}
	}
	if r := rows["p-quiet"]; len(r) < 7 || r[6] != "sealed" {
		t.Fatalf("the p-quiet row = %q\n%s", r, out)
	}
	if r := rows["p-live"]; len(r) < 7 || r[6] != "draining" {
		t.Fatalf("the p-live row = %q\n%s", r, out)
	}
	for _, want := range []string{"p-quiet (sealed) since", "p-live (draining) since"} {
		if !strings.Contains(out.String(), want) || !strings.Contains(out.String(), ": the pool shrinks") {
			t.Fatalf("the listing does not say why %q was released:\n%s", want, out)
		}
	}

	// A DRAINING HOST THAT CAN NEVER PROVE ITSELF is closed only with --force, which says so.
	if err := Pool(ctx, env, []string{"close", "--config", cfg, "--reason", "it died", "p-live"}); err == nil ||
		!strings.Contains(err.Error(), "closed with --force") {
		t.Fatalf("closing a draining host without --force answered %v", err)
	}
	env, out = poolEnv()
	if err := Pool(ctx, env, []string{"close", "--config", cfg, "--force", "--reason", "it died", "p-live"}); err != nil ||
		!strings.Contains(out.String(), "Closed p-live without proof") {
		t.Fatalf("closing a draining host with --force: %v\n%s", err, out)
	}
	env, out = poolEnv()
	if err := Pool(ctx, env, []string{"--config", cfg}); err != nil ||
		!strings.Contains(out.String(), "closed by a person without proof: it died") {
		t.Fatalf("the listing after a forced closure: %v\n%s", err, out)
	}
}
