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
			"billet nodes deny <node> --fingerprint <fp>")
	}

	switch args[0] {
	case "pending":
		return cmdNodesPending(ctx, args[1:])
	case "approve":
		return cmdNodesDecide(ctx, args[1:], alloc.EnrollApproved)
	case "deny":
		return cmdNodesDecide(ctx, args[1:], alloc.EnrollDenied)
	}

	return fmt.Errorf("unknown nodes command %q; try pending, approve or deny", args[0])
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
	fmt.Fprintln(w, "NODE\tFINGERPRINT\tSTATE\tASKED")

	for _, e := range pending {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", e.Name, e.Fingerprint, e.State, e.RequestedAt)
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

		for _, e := range requests {
			if e.Name == name {
				csr = e.CSRPEM
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

		certPEM = string(bundle.CertPEM)
	}

	if err := a.DecideEnrollment(ctx, name, *fingerprint, decision, certPEM); err != nil {
		return err
	}

	fmt.Printf("%s %s\n", map[string]string{
		alloc.EnrollApproved: "Approved", alloc.EnrollDenied: "Denied",
	}[decision], name)

	if decision == alloc.EnrollApproved {
		fmt.Printf("\nIt picks up its certificate on its next attempt, within a few seconds.\n")
	}

	return nil
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
func enrollNode(ctx context.Context, cfg *config.Config, caFingerprint string) error {
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

	csrPEM, keyPEM, err := wirecert.NewNodeCSR(name)
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
		certPEM, err := nodeclient.Enroll(ctx, "https://"+cfg.Node.ServerAddr, name, caPEM, csrPEM)

		switch {
		case err == nil:
			return writeBundle(cfg.Node.TLS, certPEM, keyPEM, caPEM)
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
		if err := os.WriteFile(f.path, f.data, f.mode); err != nil {
			return fmt.Errorf("write %s: %w", f.path, err)
		}
	}

	fmt.Printf("Approved. Wrote:\n  %s\n  %s\n  %s\n\n", tls.CertPath, tls.KeyPath, tls.CAPath)
	fmt.Printf("Start the node normally now: billet node\n")

	return nil
}
