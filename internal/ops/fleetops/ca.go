package fleetops

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"text/tabwriter"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/app"
	"github.com/junioryono/billet/internal/cli"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/hostauthority"
	"github.com/junioryono/billet/internal/nodeplane"
	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/wirecert"
)

// cmdCA issues the certificates the node wire authenticates with.
//
// RUN ON THE CONTROL PLANE, because that is where the authority's private key
// is and where it stays. The bundle it writes is copied to the node — the key
// travels once, by an operator, rather than over a wire that does not yet trust
// anybody.
func CA(ctx context.Context, env cli.Env, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: billet ca issue <node> [--out <dir>] | billet ca token | " +
			"billet ca rotate | billet ca retire | billet ca revoke <node> | " +
			"billet ca revocations | billet ca show")
	}

	switch args[0] {
	case "issue":
		return CAIssue(ctx, env, args[1:])
	case "revoke":
		return CARevoke(ctx, env, args[1:])
	case "revocations":
		return CARevocations(ctx, env, args[1:])
	case "token":
		return CAToken(ctx, env, args[1:])
	case "rotate":
		return cmdCARotate(ctx, env, args[1:])
	case "retire":
		return cmdCARetire(ctx, env, args[1:])
	case "show":
		return cmdCAShow(ctx, env, args[1:])
	case "sync":
		return cmdCASync(ctx, env, args[1:])
	}

	return fmt.Errorf(
		"unknown ca command %q; try issue, token, rotate, retire, sync, revoke, "+
			"revocations or show", args[0])
}

// cmdCARevoke withdraws a node's certificate.
//
// BY SERIAL, taken from the bundle the operator issued. A name would be the
// obvious handle and is the wrong one: a name is legitimately re-issued to a
// replacement machine, so revoking it would refuse the replacement too. The
// serial names the one credential being taken back.
//
// WRITES TO THE LEDGER, so it takes effect on the next request the revoked host
// makes rather than at the next restart of anything.
func CARevoke(ctx context.Context, env cli.Env, args []string) error {
	fs := cli.NewFlagSet("billet ca revoke", env.Stdout)
	cfgPath := cli.AddConfigFlag(fs)
	certPath := fs.String("cert", "", "the certificate to revoke (default <node>-billet-tls/node.crt)")
	reason := fs.String("reason", "", "why, recorded alongside it")

	name, err := cli.ParseWithName(fs, args)
	if err != nil {
		return err
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}

	if cfg.Server == nil {
		return errors.New("revoking is done on the control plane, and this config has no server section")
	}

	path := *certPath
	if path == "" {
		path = filepath.Join(name+"-billet-tls", "node.crt")
	}

	serial, err := serialFromCert(path)
	if err != nil {
		return err
	}

	// Revoking matters most while the control plane is UP, so it must not need
	// the directory lock that plane is holding. See state.OpenAdmin.
	db, err := app.OpenLedger(ctx, cfg, app.LedgerOperator)
	if err != nil {
		return fmt.Errorf("server state: %w", err)
	}

	defer db.Close()

	allocator, err := alloc.New(db, alloc.Limits{
		MaxVCPU:   cfg.Server.MaxVCPU,
		MaxMemory: cfg.Server.MaxMemory,
		Nodes:     cfg.NodePolicies(),
		Shares:    cfg.TargetShares(),
	}, cfg.Tiers)
	if err != nil {
		return fmt.Errorf("capacity allocator: %w", err)
	}

	if err := allocator.RevokeCert(ctx, serial, name, *reason); err != nil {
		return err
	}

	fmt.Fprintf(env.Stdout, "Revoked %s (node %s)\n", serial, name)
	fmt.Fprintf(env.Stdout, "\nIt is refused on the next request that certificate makes. Issue a replacement\n")
	fmt.Fprintf(env.Stdout, "with `billet ca issue %s` if the machine is coming back.\n", name)

	return nil
}

// cmdCARevocations lists what has been withdrawn.
func CARevocations(ctx context.Context, env cli.Env, args []string) error {
	fs := cli.NewFlagSet("billet ca revocations", env.Stdout)
	cfgPath := cli.AddConfigFlag(fs)

	if err := cli.Parse(fs, args); err != nil {
		return err
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}

	if cfg.Server == nil {
		return errors.New("the revocation list lives on the control plane, and this config has no server section")
	}

	db, err := app.OpenLedger(ctx, cfg, app.LedgerOperator)
	if err != nil {
		return fmt.Errorf("server state: %w", err)
	}

	defer db.Close()

	allocator, err := alloc.New(db, alloc.Limits{
		MaxVCPU:   cfg.Server.MaxVCPU,
		MaxMemory: cfg.Server.MaxMemory,
		Nodes:     cfg.NodePolicies(),
		Shares:    cfg.TargetShares(),
	}, cfg.Tiers)
	if err != nil {
		return fmt.Errorf("capacity allocator: %w", err)
	}

	revoked, err := allocator.RevokedCerts(ctx)
	if err != nil {
		return err
	}

	if len(revoked) == 0 {
		fmt.Fprintln(env.Stdout, "No certificates have been revoked.")

		return nil
	}

	w := tabwriter.NewWriter(env.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "SERIAL\tNODE\tREVOKED\tREASON")

	for _, r := range revoked {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", r.Serial, r.Node, r.RevokedAt, r.Reason)
	}

	return w.Flush()
}

// serialFromCert reads the serial out of a PEM certificate on disk.
func serialFromCert(path string) (string, error) {
	// The path is the operator's own argument on their own machine, naming a
	// certificate they issued. There is no boundary here to cross: `billet ca
	// revoke` is already a command that writes to the deployment's ledger.
	raw, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}

	block, _ := pem.Decode(raw)
	if block == nil || block.Type != "CERTIFICATE" {
		return "", fmt.Errorf("%s is not a PEM certificate", path)
	}

	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "", fmt.Errorf("parse %s: %w", path, err)
	}

	return wirecert.Serial(cert), nil
}

func CAIssue(ctx context.Context, env cli.Env, args []string) (err error) {
	fs := cli.NewFlagSet("billet ca issue", env.Stdout)
	cfgPath := cli.AddConfigFlag(fs)
	out := fs.String("out", "", "directory to write the bundle to (default ./<node>-billet-tls)")
	reissue := fs.Bool("reissue", false,
		"deliberately replace an existing bundle directory (the old one is moved to "+
			"<dir>.replaced; the old certificate stays valid until revoked)")
	lifetime := fs.Duration("lifetime", wirecert.LeafLifetime,
		"how long the certificate is good for (default a year; the node renews it on its "+
			"own once less than a third remains). Shorter for a short-lived host or a "+
			"rotation rehearsal; never below "+wirecert.MinIssuedLifetime.String())

	name, err := cli.ParseWithName(fs, args)
	if err != nil {
		return err
	}

	if name == "" {
		return errors.New("usage: billet ca issue <node> [--out <dir>] [--lifetime <duration>]")
	}

	// The bounds are wirecert's (IssueNodeFor refuses outside them); checked here
	// too so the refusal arrives before the config is loaded and the authority
	// touched, naming the flag.
	if *lifetime < wirecert.MinIssuedLifetime || *lifetime > wirecert.LeafLifetime {
		return fmt.Errorf("--lifetime %s is outside [%s, %s]: a node renews on a five-minute "+
			"sweep once a third of the life remains, so a shorter leaf expires in place, and a "+
			"longer one is not something an authority issues", *lifetime,
			wirecert.MinIssuedLifetime, wirecert.LeafLifetime)
	}

	// VALIDATED HERE, not on first connection. The common name IS the node's
	// identity on the wire, so a certificate whose name the server would never
	// accept is a bundle an operator installs, restarts a host for, and only then
	// discovers is useless.
	if err := config.ValidateNodeName("node", name); err != nil {
		return err
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}

	if cfg.Server == nil {
		return fmt.Errorf("%s has no server section, so it does not hold a certificate "+
			"authority; run this on the control plane", *cfgPath)
	}

	// HELD THROUGH THE LEDGER RECORD BELOW, not released after the authority
	// load: `recordIssued` opens the ledger, which creates the directory and its
	// lock on first use, and an issue interrupted by a closure between the load
	// and the record would otherwise open a directory a retirement had moved.
	acc, err := hostauthority.Open(ctx, cfg.Server.IdentityDir, hostauthority.Intent{Create: true, Wait: hostauthority.Wait})
	if err != nil {
		return err
	}

	defer func() { err = errors.Join(err, acc.Release()) }()

	deployment, err := state.DeploymentID(cfg.Server.IdentityDir)
	if err != nil {
		return err
	}

	authority, err := wirecert.LoadServing(cfg.Server.IdentityDir, deployment)
	if err != nil {
		return err
	}

	bundle, err := authority.Issuing.IssueNodeFor(name, *lifetime)
	if err != nil {
		return err
	}

	// THE WHOLE TRUST BUNDLE, NOT THE AUTHORITY THAT SIGNED THIS LEAF. During an
	// overlap the new authority issues and the OLD one signs what the control
	// plane presents, so a bundle carrying only the issuer hands the operator a
	// node that cannot verify the server it was just enrolled against — the one
	// machine a rotation is supposed not to strand. The wire's own enroll and
	// renew responses have always carried the bundle (nodeplane's trustBundle);
	// this is the out-of-band path, and it did not.
	bundle.CAPEM = authority.Trust

	// The destination is resolved and vetted BEFORE the ledger records the new
	// serial: a refusal (an existing .replaced archive, a bad path) must not
	// leave the ledger claiming a credential no bundle carries.
	dir := *out
	if dir == "" {
		dir = name + "-billet-tls"
	}
	dir = filepath.Clean(dir)

	// FAIL CLOSED ON EVERY VETTING UNCERTAINTY, and vet the plain-issue
	// destination too: a refusal here must come BEFORE the ledger records the
	// new serial and displaces the admitted fingerprint.
	if *reissue {
		if _, err := os.Stat(dir + ".replaced"); err == nil {
			return fmt.Errorf("%s.replaced already exists — it holds the certificate from the "+
				"previous reissue, which stays VALID until revoked, and overwriting it would "+
				"destroy the only copy the revoke command reads. Revoke it first (`billet ca "+
				"revoke %s --cert %s`), then remove the directory and re-run",
				dir, cli.ShellArg(name), cli.ShellArg(filepath.Join(dir+".replaced", "node.crt")))
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("check %s.replaced: %w", dir, err)
		}
	} else if _, err := os.Stat(filepath.Join(dir, "node.key")); err == nil {
		return fmt.Errorf("%s already holds a bundle and billet will not overwrite it — that "+
			"node is probably enrolled. Write to a new directory with --out, or replace it "+
			"deliberately with --reissue", dir)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("check %s: %w", dir, err)
	}

	// RECORDED BEFORE IT IS WRITTEN DOWN, and fatal if it fails.
	//
	// Two facts go in: the admission trail, so a fleet built by issuing directly
	// is visible to the same list that shows what is waiting, and the SERIAL,
	// without which this credential can never be revoked. Handing an operator a
	// bundle billet cannot take back is worse than handing them an error, and the
	// error is recoverable — nothing has been written yet, so re-running is safe.
	if err := recordIssued(ctx, env, *cfgPath, name, bundle); err != nil {
		return err
	}

	// --reissue MOVES the old bundle aside rather than deleting it: the node is
	// still holding that key until someone installs the new bundle and restarts
	// it, and the old certificate stays VALID until revoked — both facts the
	// operator acts on, so both survive on disk and are said below. A prior
	// .replaced was refused above, so nothing is ever destroyed here.
	replaced := false
	if *reissue {
		if _, err := os.Stat(filepath.Join(dir, "node.key")); err == nil {
			if err := os.Rename(dir, dir+".replaced"); err != nil {
				return fmt.Errorf("move the old bundle aside: %w", err)
			}
			replaced = true
		}
	}

	if err := bundle.Write(dir); err != nil {
		return err
	}

	if replaced {
		fmt.Fprintf(env.Stdout, "billet ca: the previous bundle was moved to %s.replaced. The node keeps "+
			"using its old key until this new bundle is installed and the node restarts, and "+
			"the OLD certificate stays valid until you revoke it:\n\n"+
			"  billet ca revoke %s --cert %s --reason reissued\n\n",
			dir, cli.ShellArg(name), cli.ShellArg(filepath.Join(dir+".replaced", "node.crt")))
	}

	abs, err := filepath.Abs(dir)
	if err != nil {
		abs = dir
	}

	fmt.Fprintf(env.Stdout, "billet ca: wrote a bundle for node %q to %s\n\n", name, abs)
	fmt.Fprintf(env.Stdout, "  fingerprint  %s\n\n", wirecert.Fingerprint(mustSPKI(bundle)))
	fmt.Fprint(env.Stdout, "Copy that directory to the node, then point its config at the files:\n\n")
	fmt.Fprintf(env.Stdout, "  node:\n    tls:\n      cert: /etc/billet/tls/node.crt\n"+
		"      key:  /etc/billet/tls/node.key\n      ca:   /etc/billet/tls/ca.crt\n\n")
	fmt.Fprint(env.Stdout, "node.name comes from the certificate, so it does not have to be written.\n")
	fmt.Fprint(env.Stdout, "node.key is a private key: keep it 0600 and do not copy it anywhere else.\n")

	return nil
}

// mustSPKI is the bundle's public key bytes, or nothing if it cannot be read.
// Used only for display beside a bundle that has already been written.
func mustSPKI(b wirecert.Bundle) []byte {
	leaf, err := wirecert.LeafOf(b)
	if err != nil {
		return nil
	}

	return leaf.RawSubjectPublicKeyInfo
}

// recordIssued writes a directly-issued certificate into the admission trail.
func recordIssued(ctx context.Context, env cli.Env, cfgPath, name string, bundle wirecert.Bundle) error {
	leaf, err := wirecert.LeafOf(bundle)
	if err != nil {
		return fmt.Errorf("read back the certificate just issued to %s: %w", name, err)
	}

	a, closeDB, err := ControlPlaneAllocator(ctx, cfgPath)
	if err != nil {
		return err
	}

	defer closeDB()

	if err := recordIssuedCert(ctx, a, bundle, name, alloc.CertIssued); err != nil {
		return err
	}

	displaced, err := a.RecordIssued(ctx, name, wirecert.FingerprintOfCert(leaf), string(bundle.CertPEM))
	if err != nil {
		return err
	}

	// SAID OUT LOUD, because this is the one path that can quietly retire a
	// fingerprint an operator has already compared and trusted.
	if displaced != "" {
		fmt.Fprintf(env.Stdout, "\nNOTE: %s was already admitted as %s.\nThat key can no longer be used "+
			"under this name; revoke its certificate if the machine still holds it.\n",
			name, displaced)
	}

	return nil
}

func cmdCAShow(ctx context.Context, env cli.Env, args []string) error {
	fs := cli.NewFlagSet("billet ca show", env.Stdout)
	cfgPath := cli.AddConfigFlag(fs)

	if err := cli.Parse(fs, args); err != nil {
		return err
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}

	if cfg.Server == nil {
		return fmt.Errorf("%s has no server section, so it does not hold a certificate "+
			"authority", *cfgPath)
	}

	acc, err := hostauthority.Open(ctx, cfg.Server.IdentityDir, hostauthority.Intent{Create: true, Wait: hostauthority.Wait})
	if err != nil {
		return err
	}

	deployment, err := state.DeploymentID(cfg.Server.IdentityDir)
	if err != nil {
		return errors.Join(err, acc.Release())
	}

	ca, err := wirecert.LoadOrCreateCA(cfg.Server.IdentityDir, deployment)
	if err := errors.Join(err, acc.Release()); err != nil {
		return err
	}

	fmt.Fprintf(env.Stdout, "deployment  %s\nauthority   %s\nexpires     %s\nfingerprint %s\n",
		deployment, wirecert.CADir(cfg.Server.IdentityDir), ca.NotAfter().Format(time.RFC3339),
		ca.Fingerprint())

	if left, capping := ca.Capping(); capping {
		fmt.Fprintf(env.Stdout, "\nWARNING: this authority expires in %s, which is less than a certificate's\n",
			left.Round(24*time.Hour))
		fmt.Fprintf(env.Stdout, "full life, so every certificate it issues from now on is SHORTER than the\n")
		fmt.Fprintf(env.Stdout, "last — and when it expires, every node stops at once. Nothing will error\n")
		fmt.Fprintf(env.Stdout, "before that. Plan a rotation: issue a new authority, let nodes pick it up\n")
		fmt.Fprintf(env.Stdout, "through renewal while both are trusted, then retire the old one.\n")
	}

	fmt.Fprintf(env.Stdout, "\nGive the fingerprint to a node that is enrolling, so it can tell this control\n")
	fmt.Fprintf(env.Stdout, "plane from anything else that answers:\n\n")
	fmt.Fprintf(env.Stdout, "  billet node --enroll --ca-fingerprint %s%s\n",
		ca.Fingerprint(), enrollAddrFlag(cfg))

	if cfg.Server.BootstrapListen == "" && !nodeplane.LoopbackOnly(cfg.Server.Listen) {
		fmt.Fprintf(env.Stdout, "\nThis control plane serves no enrollment address, so that command has\n")
		fmt.Fprintf(env.Stdout, "nowhere to ask: its node wire requires a certificate an enrolling machine\n")
		fmt.Fprintf(env.Stdout, "does not have yet. Either issue the bundle here and copy it out of band:\n\n")
		fmt.Fprintf(env.Stdout, "  billet ca issue <node>\n\n")
		fmt.Fprintf(env.Stdout, "or set server.bootstrap_listen and restart.\n")
	}

	return nil
}
