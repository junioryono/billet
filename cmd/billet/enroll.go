package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"text/tabwriter"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/nodeclient"
	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/wirecert"
)

// enrollPollEvery is how often a waiting node asks again.
//
// Slow, because the thing it is waiting for is a HUMAN reading a fingerprint. A
// tight loop would fill the control plane's log with one line per node per
// second while somebody walks to another machine to compare a value.
const enrollPollEvery = 5 * time.Second

// cmdNodes is the operator's side of enrollment.
func cmdNodes(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: billet nodes pending | billet nodes approve <node> --fingerprint <fp> | " +
			"billet nodes deny <node> --fingerprint <fp> | billet nodes revoke <node>")
	}

	switch args[0] {
	case "pending":
		return cmdNodesPending(ctx, args[1:])
	case "approve":
		return cmdNodesDecide(ctx, args[1:], alloc.EnrollApproved)
	case "deny":
		return cmdNodesDecide(ctx, args[1:], alloc.EnrollDenied)
	case "revoke":
		return cmdNodesRevoke(ctx, args[1:])
	}

	return fmt.Errorf("unknown nodes command %q; try pending, approve, deny or revoke", args[0])
}

// cmdNodesRevoke takes back every credential a machine currently holds.
//
// THE HANDLE AN OPERATOR ACTUALLY HAS. `billet ca revoke` names one serial, read
// out of the bundle that was issued — and a node renews itself, so after the
// first renewal that file describes a credential the machine stopped presenting
// months ago. Revoking it succeeds, says the certificate will be refused on its
// next request, and changes nothing: the host keeps registering, binding leases
// and drawing JIT registrations against the organisation.
//
// A REPLACEMENT UNDER THE SAME NAME IS UNAFFECTED, which is why this is not the
// same as banning a name. It revokes the serials outstanding at this moment; a
// certificate issued afterwards is not one of them.
func cmdNodesRevoke(ctx context.Context, args []string) error {
	fs := newFlagSet("billet nodes revoke")
	cfgPath := addConfigFlag(fs)
	reason := fs.String("reason", "", "why, recorded alongside it")

	name, err := parseWithName(fs, args)
	if err != nil {
		return err
	}

	if name == "" {
		return errors.New("usage: billet nodes revoke <node> [--reason why]")
	}

	a, closeDB, err := controlPlaneAllocator(ctx, *cfgPath)
	if err != nil {
		return err
	}

	defer closeDB()

	// EVERY CREDENTIAL THIS DEPLOYMENT EVER HANDED OUT, including the ones
	// admitted before billet recorded serials. Without this an upgraded
	// deployment answers "holds no certificate" for a machine that is holding a
	// working one.
	if err := backfillIssuedCerts(ctx, a); err != nil {
		return err
	}

	revoked, err := a.RevokeNode(ctx, name, *reason)
	if err != nil {
		return err
	}

	if len(revoked) == 0 {
		fmt.Printf("%s holds no certificate this deployment issued, so there is nothing to "+
			"take back.\n", name)

		return nil
	}

	fmt.Printf("Revoked %d certificate(s) held by %s:\n\n", len(revoked), name)

	for i := range revoked {
		fmt.Printf("  %s  %s  expires %s\n", revoked[i].Serial, revoked[i].Source, revoked[i].NotAfter)
	}

	fmt.Printf("\nEach is refused on the next request it makes. Issue a replacement with\n")
	fmt.Printf("`billet ca issue %s` if the machine is coming back.\n", name)

	return nil
}

// cmdNodesPending lists machines waiting to be let in.
func cmdNodesPending(ctx context.Context, args []string) error {
	fs := newFlagSet("billet nodes pending")
	cfgPath := addConfigFlag(fs)
	all := fs.Bool("all", false, "include decided requests")

	if err := parse(fs, args); err != nil {
		return err
	}

	a, closeDB, err := controlPlaneAllocator(ctx, *cfgPath)
	if err != nil {
		return err
	}

	defer closeDB()

	want := alloc.EnrollPending
	if *all {
		want = ""
	}

	pending, err := a.Enrollments(ctx, want)
	if err != nil {
		return err
	}

	if len(pending) == 0 {
		fmt.Println("No nodes are waiting to be approved.")

		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NODE\tFINGERPRINT\tSTATE\tHOW\tASKED")

	for i := range pending {
		e := &pending[i]
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", e.Name, e.Fingerprint, e.State, e.Source, e.RequestedAt)
	}

	if err := w.Flush(); err != nil {
		return err
	}

	fmt.Printf("\nCompare a fingerprint against what the node printed on its own console, then:\n")
	fmt.Printf("  billet nodes approve <node> --fingerprint <the value you compared>\n")

	return nil
}

// cmdNodesDecide approves or denies one machine.
//
// THE FINGERPRINT IS REQUIRED, and that is the whole security of this command.
// Approving by name alone approves whatever currently holds the name; approving
// by fingerprint approves the machine whose key an operator actually compared.
func cmdNodesDecide(ctx context.Context, args []string, decision string) error {
	fs := newFlagSet("billet nodes " + decision)
	cfgPath := addConfigFlag(fs)
	fingerprint := fs.String("fingerprint", "",
		"the fingerprint you compared against the node's console (required)")

	name, err := parseWithName(fs, args)
	if err != nil {
		return err
	}

	if *fingerprint == "" {
		return errors.New("--fingerprint is required: it names WHICH machine you are deciding " +
			"about, and without it you would be deciding about whatever currently holds the name")
	}

	a, closeDB, err := controlPlaneAllocator(ctx, *cfgPath)
	if err != nil {
		return err
	}

	defer closeDB()

	certPEM := ""

	if decision == alloc.EnrollApproved {
		cfg, err := config.Load(*cfgPath)
		if err != nil {
			return err
		}

		deployment, err := state.DeploymentID(cfg.Server.StateDir)
		if err != nil {
			return err
		}

		ca, err := wirecert.LoadOrCreateCA(cfg.Server.StateDir, deployment)
		if err != nil {
			return err
		}

		requests, err := a.Enrollments(ctx, alloc.EnrollPending)
		if err != nil {
			return err
		}

		var csr string

		for i := range requests {
			if requests[i].Name == name {
				csr = requests[i].CSRPEM
			}
		}

		if csr == "" {
			return fmt.Errorf("no pending request from %s", name)
		}

		// SIGNED FOR THE NAME BEING APPROVED, never for whatever the request
		// claimed: the operator is deciding about a name, and the CSR's own
		// subject is only what the requester typed.
		bundle, err := ca.SignNodeCSR(name, []byte(csr))
		if err != nil {
			return err
		}

		// RECORDED BEFORE IT IS HANDED OVER. Revocation names a serial, so a
		// credential billet never wrote down cannot be taken back — and this is
		// the first of the three ways one comes into existence.
		if err := recordIssuedCert(ctx, a, bundle, name, alloc.CertEnrolled); err != nil {
			return err
		}

		certPEM = string(bundle.CertPEM)
	}

	if err := a.DecideEnrollment(ctx, name, *fingerprint, decision, certPEM); err != nil {
		return err
	}

	fmt.Printf("%s %s\n", map[string]string{
		alloc.EnrollApproved: "Approved", alloc.EnrollDenied: "Denied",
	}[decision], name)

	if decision == alloc.EnrollDenied {
		fmt.Printf("\nThe name is free again: another key may now ask for it. That is the way " +
			"back\nfor a machine that lost its key while waiting to be approved.\n")
	}

	if decision == alloc.EnrollApproved {
		fmt.Printf("\nIt picks up its certificate on its next attempt, within a few seconds.\n")
	}

	return nil
}

// backfillIssuedCerts records certificates admitted before billet tracked
// serials, so revocation can reach them.
//
// A CREDENTIAL BILLET DOES NOT KNOW ABOUT CANNOT BE TAKEN BACK, and an upgrade
// is exactly how one comes to exist: every node admitted before issued_certs
// existed holds a working certificate whose serial was never written down.
// `billet nodes revoke` would report that the machine holds nothing and change
// nothing, which is the worst possible answer to a compromise.
//
// The admission trail is what makes this recoverable: both ways in — approval
// and `billet ca issue` — stored the certificate they handed over, so the serial
// can be read back out of it. Idempotent, so running it on every revocation
// costs one query on a deployment that is already complete.
func backfillIssuedCerts(ctx context.Context, a *alloc.Allocator) error {
	admitted, err := a.Enrollments(ctx, alloc.EnrollApproved)
	if err != nil {
		return err
	}

	for i := range admitted {
		rec := &admitted[i]
		if rec.CertPEM == "" {
			continue
		}

		leaf, err := wirecert.LeafOf(wirecert.Bundle{CertPEM: []byte(rec.CertPEM)})
		if err != nil {
			// A stored certificate billet cannot read is worth saying out loud —
			// that node's credential is outside revocation — but it must not stop
			// the ones that can be read from being recorded.
			fmt.Fprintf(os.Stderr, "the certificate recorded for node %q cannot be parsed, so "+
				"it cannot be revoked by serial: %v\n", rec.Name, err)

			continue
		}

		source := alloc.CertEnrolled
		if rec.Source == "issued" {
			source = alloc.CertIssued
		}

		if err := a.RecordIssuedCert(ctx, alloc.IssuedCert{
			Serial:   wirecert.Serial(leaf),
			Node:     rec.Name,
			Source:   source,
			NotAfter: leaf.NotAfter.UTC().Format(time.RFC3339),
		}); err != nil {
			return err
		}
	}

	return nil
}

// recordIssuedCert writes a credential into the list revocation reads.
//
// EVERY WAY A CERTIFICATE COMES INTO EXISTENCE GOES THROUGH HERE — approval,
// `billet ca issue`, and renewal over the wire. Revocation is keyed on serial,
// which is the right granularity because a node name is legitimately re-issued
// to a replacement machine; the cost is that a serial billet does not know about
// is a credential it cannot take back.
func recordIssuedCert(
	ctx context.Context, a *alloc.Allocator, bundle wirecert.Bundle, node, source string,
) error {
	leaf, err := wirecert.LeafOf(bundle)
	if err != nil {
		return fmt.Errorf("read back the certificate just issued to %s: %w", node, err)
	}

	return a.RecordIssuedCert(ctx, alloc.IssuedCert{
		Serial:   wirecert.Serial(leaf),
		Node:     node,
		Source:   source,
		NotAfter: leaf.NotAfter.UTC().Format(time.RFC3339),
	})
}

// controlPlaneAllocator opens the ledger for a command that runs on the server.
func controlPlaneAllocator(ctx context.Context, cfgPath string) (*alloc.Allocator, func(), error) {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return nil, nil, err
	}

	if cfg.Server == nil {
		return nil, nil, errors.New("this command runs on the control plane, and this config has " +
			"no server section")
	}

	db, err := state.Open(ctx, cfg.Server.StateDir)
	if err != nil {
		return nil, nil, fmt.Errorf("server state: %w", err)
	}

	a, err := alloc.New(db, alloc.Limits{
		MaxVCPU:   cfg.Server.MaxVCPU,
		MaxMemory: cfg.Server.MaxMemory,
		Nodes:     cfg.NodePolicies(),
	}, cfg.Tiers)
	if err != nil {
		db.Close()

		return nil, nil, fmt.Errorf("capacity allocator: %w", err)
	}

	return a, func() { db.Close() }, nil
}

// enrollNode asks a control plane to admit this machine and waits to be let in.
//
// IT PRINTS ITS OWN FINGERPRINT AND STOPS THERE. The operator compares that
// value against what `billet nodes pending` shows on the control plane — two
// ends displaying the same number, over a channel an attacker on the network
// does not control. That comparison is the trust decision; everything else here
// is transport.
func enrollNode(ctx context.Context, cfg *config.Config, caFingerprint, joinToken string) error {
	if cfg.Node.TLS == nil {
		return errors.New("enrolling writes a certificate, so node.tls must say where to put it")
	}

	caPEM, deployment, err := nodeclient.FetchCA(ctx, "https://"+cfg.Node.ServerAddr, caFingerprint)
	if err != nil {
		return err
	}

	name := cfg.Node.Name
	if name == "" {
		return errors.New("enrolling needs node.name: the certificate does not exist yet, so " +
			"there is nothing to take it from")
	}

	// KEPT ACROSS ATTEMPTS, because the wait is for a human and this process may
	// not survive it.
	//
	// The key used to exist only in memory. A reboot or a Ctrl-C during the
	// approval wait lost it while the control plane kept the pending row, so the
	// machine came back with a NEW key and was refused: the name was claimed by a
	// fingerprint nothing could present any more. Reusing the staged key makes a
	// retry the same request rather than a second one.
	csrPEM, keyPEM, err := pendingIdentity(cfg.Node.TLS, name)
	if err != nil {
		return err
	}

	fingerprint, err := wirecert.FingerprintOfCSR(csrPEM)
	if err != nil {
		return err
	}

	fmt.Printf("Asking to join deployment %s as %q.\n\n", deployment, name)
	fmt.Printf("  this node's fingerprint  %s\n\n", fingerprint)
	fmt.Printf("On the control plane, check it matches and approve:\n\n")
	fmt.Printf("  billet nodes approve %s --fingerprint %s\n\n", name, fingerprint)

	for {
		certPEM, signedBy, err := nodeclient.Enroll(
			ctx, "https://"+cfg.Node.ServerAddr, name, joinToken, caPEM, csrPEM)

		switch {
		case err == nil:
			// THE AUTHORITY THAT SIGNED IT, NOT THE ONE WE STARTED WITH. Waiting
			// for a human is unbounded, so the deployment's CA can rotate while
			// this loop is polling and approval then signs with the new one.
			// Writing the bootstrap authority here left a node whose own
			// certificate does not chain to its own ca.crt.
			if _, verifyErr := wirecert.ClientTLS(wirecert.Bundle{
				CertPEM: certPEM, KeyPEM: keyPEM, CAPEM: signedBy,
			}); verifyErr != nil {
				return fmt.Errorf("the control plane approved this node but the bundle it "+
					"returned does not verify against itself, so it would not start: %w", verifyErr)
			}

			if err := writeBundle(cfg.Node.TLS, certPEM, keyPEM, signedBy); err != nil {
				return err
			}

			// The staged request has become a certificate, so the copy of the key
			// beside it is a second copy of a secret and nothing else.
			clearPendingIdentity(cfg.Node.TLS)

			return nil
		case errors.Is(err, nodeclient.ErrDenied):
			return fmt.Errorf("an operator denied this node; nothing to retry")
		case errors.Is(err, nodeclient.ErrNotApproved):
			// Expected, repeatedly, while a human decides.
		default:
			return err
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(enrollPollEvery):
		}
	}
}

// pendingIdentity is the key and request this node is enrolling with, generated
// once and reused until enrollment finishes.
//
// STAGED BESIDE THE BUNDLE, so it survives the process that made it. The name is
// claimed by the first key to ask; a machine that loses its key mid-wait and
// generates another is a second key asking for a name that is already taken, and
// the control plane is right to refuse it.
//
// Written 0600 with O_EXCL, so a stale staging file is reused rather than
// silently replaced — replacing it is the very thing that strands the name.
func pendingIdentity(tls *config.NodeTLS, name string) ([]byte, []byte, error) {
	dir := filepath.Dir(tls.KeyPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, nil, fmt.Errorf("create %s: %w", dir, err)
	}

	keyStage := filepath.Join(dir, "pending.key")
	csrStage := filepath.Join(dir, "pending.csr")
	// BOUND TO THE NAME IT WAS MADE FOR. The server derives the certificate
	// subject from the REQUEST, not from the CSR, so reusing a stage after
	// node.name changed would let one key collect certificates for two names —
	// and revoking one of them would leave the other working.
	forStage := filepath.Join(dir, "pending.node")

	staged, keyErr := wirecert.ReadSecret(keyStage)
	stagedCSR, csrErr := os.ReadFile(csrStage)
	stagedFor, forErr := os.ReadFile(forStage)

	// ALL THREE OR NONE. A partial stage is not "nothing staged": the key may
	// already have a pending request against it on the control plane, and
	// generating a fresh one replaces it — after which the name is held by a
	// fingerprint this machine can no longer present, and only an operator can
	// free it. So only a complete absence starts over.
	missing := errors.Is(keyErr, os.ErrNotExist) &&
		errors.Is(csrErr, os.ErrNotExist) &&
		errors.Is(forErr, os.ErrNotExist)

	switch {
	case keyErr == nil && csrErr == nil && forErr == nil && string(stagedFor) == name:
		fmt.Printf("Resuming the enrollment already staged in %s.\n\n", dir)

		return stagedCSR, staged, nil

	case missing:
		// Nothing staged, which is the ordinary first run.

	case keyErr == nil && (csrErr != nil || forErr != nil):
		// THE KEY SURVIVED AND ITS COMPANIONS DID NOT. Replacing it would abandon
		// a request the control plane may already be holding under this name.
		return nil, nil, fmt.Errorf(
			"%s holds a staged enrollment key without the request that goes with it, so this "+
				"machine cannot present what it already asked for. Remove pending.key, "+
				"pending.csr and pending.node to start again, and deny the pending request on "+
				"the control plane so the name is free", dir)

	case keyErr != nil:
		// A STAGED KEY THAT FAILS THE CHECKS IS NOT SILENTLY REPLACED. It may be
		// the identity this machine is about to be approved for, and replacing it
		// strands the name; it may also be readable by somebody else, which is why
		// it cannot simply be used. Both need an operator.
		return nil, nil, fmt.Errorf("the enrollment staged in %s cannot be used: %w. Remove "+
			"those files to start again, and deny the pending request on the control plane "+
			"so the name is free", dir, keyErr)

	case string(stagedFor) != name:
		return nil, nil, fmt.Errorf(
			"%s holds an enrollment staged for node %q and this config says %q. One key must "+
				"not claim two names: remove those files to start again, and deny the pending "+
				"request for %q so its name is free",
			dir, stagedFor, name, stagedFor)
	}

	csr, key, err := wirecert.NewNodeCSR(name)
	if err != nil {
		return nil, nil, err
	}

	if err := wirecert.WriteFileAtomic(keyStage, key, 0o600); err != nil {
		return nil, nil, err
	}

	if err := wirecert.WriteFileAtomic(csrStage, csr, 0o644); err != nil {
		return nil, nil, err
	}

	if err := wirecert.WriteFileAtomic(forStage, []byte(name), 0o644); err != nil {
		return nil, nil, err
	}

	return csr, key, nil
}

// clearPendingIdentity drops the staged request once it has become a bundle.
func clearPendingIdentity(tls *config.NodeTLS) {
	dir := filepath.Dir(tls.KeyPath)

	for _, name := range []string{"pending.key", "pending.csr", "pending.node"} {
		if err := os.Remove(filepath.Join(dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			// SAID OUT LOUD rather than swallowed: what is left behind is a second
			// copy of a private key, and the enrollment itself succeeded — so this
			// is a warning, not a failure.
			fmt.Fprintf(os.Stderr, "the enrollment succeeded but %s could not be removed: %v\n"+
				"That file is a copy of this node's private key; delete it.\n",
				filepath.Join(dir, name), err)
		}
	}
}

// writeBundle puts an enrolled identity on disk, the key first and 0600.
func writeBundle(tls *config.NodeTLS, certPEM, keyPEM, caPEM []byte) error {
	for _, dir := range []string{filepath.Dir(tls.KeyPath), filepath.Dir(tls.CertPath),
		filepath.Dir(tls.CAPath)} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
	}

	// THE KEY FIRST AND 0600. A certificate without its key is useless rather
	// than dangerous; the other order leaves a window where the secret exists
	// under a mode nobody has set yet.
	for _, f := range []struct {
		path string
		data []byte
		mode os.FileMode
	}{
		{tls.KeyPath, keyPEM, 0o600},
		{tls.CertPath, certPEM, 0o644},
		{tls.CAPath, caPEM, 0o644},
	} {
		// EXACTLY THIS MODE, whatever was there before. os.WriteFile applies its
		// mode only when it creates the file, so re-enrolling a machine whose
		// node.key was already 0644 wrote a fresh secret into a world-readable
		// file and said it had succeeded.
		if err := wirecert.WriteFileAtomic(f.path, f.data, f.mode); err != nil {
			return err
		}
	}

	fmt.Printf("Approved. Wrote:\n  %s\n  %s\n  %s\n\n", tls.CertPath, tls.KeyPath, tls.CAPath)
	fmt.Printf("Start the node normally now: billet node\n")

	return nil
}

// cmdCAToken mints the credential a machine needs to ASK to enroll.
//
// SHOWN ONCE AND STORED AS A HASH, for the same reason a password is: the ledger
// needs to recognise the token, not to be able to reproduce it.
//
// It admits nothing on its own. A request still waits for an operator to compare
// fingerprints; what the token stops is a stranger who can reach the port filling
// the pending list, or taking a name before the machine that should have it.
func cmdCAToken(ctx context.Context, args []string) error {
	fs := newFlagSet("billet ca token")
	cfgPath := addConfigFlag(fs)
	ttl := fs.Duration("ttl", time.Hour, "how long the token may be used for")
	uses := fs.Int("uses", 1, "how many machines may enroll with it")
	note := fs.String("note", "", "what it is for, recorded alongside it")

	if err := parse(fs, args); err != nil {
		return err
	}

	a, closeDB, err := controlPlaneAllocator(ctx, *cfgPath)
	if err != nil {
		return err
	}

	defer closeDB()

	token, err := a.NewJoinToken(ctx, *ttl, *uses, *note)
	if err != nil {
		return err
	}

	fmt.Printf("Join token (shown once, valid for %s, %d use(s)):\n\n  %s\n\n", *ttl, *uses, token)
	fmt.Printf("On the machine that should join:\n\n")
	fmt.Printf("  billet node --enroll --ca-fingerprint <from `billet ca show`> --join-token %s\n", token)

	return nil
}

// cmdCARotate starts replacing the deployment's authority.
//
// PHASE ONE OF TWO. From here the new authority issues node certificates while
// the OLD one still signs what the control plane presents, and both are trusted.
// Nodes adopt the new one through ordinary renewal, which carries the trust
// bundle alongside the certificate.
//
// Nothing breaks at this point, and nothing is finished either: `billet ca
// retire` is what ends it, and running that before the fleet has renewed is what
// would cut a node off.
func cmdCARotate(args []string) error {
	fs := newFlagSet("billet ca rotate")
	cfgPath := addConfigFlag(fs)

	if err := parse(fs, args); err != nil {
		return err
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}

	if cfg.Server == nil {
		return errors.New("rotating is done on the control plane, and this config has no server section")
	}

	deployment, err := state.DeploymentID(cfg.Server.StateDir)
	if err != nil {
		return err
	}

	ca, err := wirecert.Rotate(cfg.Server.StateDir, deployment)
	if err != nil {
		return err
	}

	fmt.Printf("Rotated. The new authority is %s\n\n", ca.Fingerprint())
	fmt.Printf("  new node certificates are issued by it\n")
	fmt.Printf("  the previous authority still signs what the control plane presents\n")
	fmt.Printf("  both are trusted, so nothing has to be restarted in a hurry\n\n")
	fmt.Printf("Restart the control plane to pick this up, then let nodes renew. Watch\n")
	fmt.Printf("`billet ca show` until nothing is left on the old authority, and finish with:\n\n")
	fmt.Printf("  billet ca retire --config %s\n\n", *cfgPath)
	fmt.Printf("A node that never renews during the overlap has to be re-enrolled, which is\n")
	fmt.Printf("why retiring is yours to run rather than something that happens on a timer.\n")

	return nil
}

// cmdCARetire finishes a rotation by dropping the old authority.
func cmdCARetire(args []string) error {
	fs := newFlagSet("billet ca retire")
	cfgPath := addConfigFlag(fs)
	force := fs.Bool("force", false, "retire even though a node may not have renewed")

	if err := parse(fs, args); err != nil {
		return err
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}

	if cfg.Server == nil {
		return errors.New("retiring is done on the control plane, and this config has no server section")
	}

	prev, _, err := wirecert.PreviousCA(cfg.Server.StateDir)
	if err != nil {
		return err
	}

	if prev == nil {
		fmt.Println("No rotation is running; there is nothing to retire.")

		return nil
	}

	// THE OPERATOR CONFIRMS, because billet cannot see what it needs to. A node
	// that has not renewed still trusts only the old authority, and retiring it
	// makes the control plane unverifiable to that node — over the wire it would
	// need in order to recover.
	if !*force {
		age, _ := wirecert.RotationAge(cfg.Server.StateDir)

		fmt.Printf("This rotation started %s ago.\n\n", age.Round(time.Hour))
		fmt.Printf("Every node has to have renewed since then. A node that has not still trusts\n")
		fmt.Printf("only the old authority, and retiring it means that node can no longer verify\n")
		fmt.Printf("this control plane — it would have to be re-enrolled by hand.\n\n")
		fmt.Printf("Re-run with --force when you have checked.\n")

		return nil
	}

	if err := wirecert.Retire(cfg.Server.StateDir); err != nil {
		return err
	}

	fmt.Println("Retired the previous authority. Restart the control plane to present a")
	fmt.Println("certificate from the new one.")

	return nil
}
