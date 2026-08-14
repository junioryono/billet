package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/provider"
	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/store/ceph"
)

// cmdImages works on the golden images microVM guests boot from.
//
// VERIFICATION IS THE ONLY OPERATION HERE THAT COULD NOT BE A SHELL SCRIPT, and it
// is the one that matters. Building an image is debootstrap and apt, which a script
// does well. Proving one WORKS means launching a real microVM the way billet
// launches one — the same provider, the same jail, the same metadata service — and
// then believing the guest rather than the host.
//
// THE HOST CANNOT SEE INSIDE A GUEST, which is the whole difficulty. Every host-side
// signal was green for the image that booted, read its registration, and ran nothing:
// the jailer exited 0, the API accepted every call, the VMM answered, the DHCP lease
// appeared. The only thing that knew otherwise was the guest, and the only way to ask
// it is to give it something to say and a place to say it.
func cmdImages(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: billet images verify <image@generation>")
	}

	switch args[0] {
	case "verify":
		return cmdImagesVerify(ctx, args[1:])
	case "due":
		return cmdImagesDue(ctx, args[1:])
	case "reap":
		return cmdImagesReap(ctx, args[1:])
	case "promote":
		return cmdImagesPromote(ctx, args[1:], true)
	case "unpromote":
		return cmdImagesPromote(ctx, args[1:], false)
	default:
		return fmt.Errorf("billet images: unknown subcommand %q", args[0])
	}
}

// cmdImagesDue reports whether the golden image is old enough to rebuild.
//
// THIS IS WHAT LETS EVERY NODE CARRY THE TIMER. A schedule on one machine is a
// schedule that stops when that machine does, and the thing it protects against --
// GitHub refusing jobs to a runner thirty days behind a release -- does not pause
// while a node is down. So the timer belongs on every node.
//
// THE CLUSTER LOCK ALONE DOES NOT MAKE THAT WORK, which is the part that is easy to
// get wrong. The lock stops two builds OVERLAPPING, and the second node then waits
// and rebuilds the same thing: with the timer's jitter, node B usually starts after
// node A has finished and released, and publishes a second identical generation. N
// nodes do N builds. This question is what turns them into one build with N-1
// machines standing by.
//
// AGE COMES FROM THE GENERATION'S NAME, which billet writes in UTC, rather than from
// the cluster's own timestamp, which `rbd snap ls` prints as a local-time string with
// no offset -- two nodes in different zones would otherwise disagree about the age of
// the same snapshot.
//
// EXIT 2 MEANS "NOTHING TO DO" RATHER THAN FAILURE. A node that finds a fresh
// generation has succeeded at its job, and a unit reporting failure every week on
// every machine but one teaches an operator to ignore it.
func cmdImagesDue(ctx context.Context, args []string) error {
	fs := newFlagSet("billet images due")
	cfgPath := addConfigFlag(fs)
	maxAge := fs.Duration("max-age", 6*24*time.Hour,
		"rebuild when the newest generation is older than this")

	rest, err := parseWithName(fs, args)
	if err != nil {
		return err
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}

	if cfg.Node == nil || cfg.Node.Ceph == nil {
		return errors.New("billet images due: this config names no ceph cluster, so there are " +
			"no published generations to date")
	}

	image := rest
	if image == "" {
		image = firecrackerTierImage(cfg)
	}

	if image == "" {
		return errors.New("billet images due: no image given and no firecracker tier names one")
	}

	store, err := ceph.New(*cfg.Node.Ceph)
	if err != nil {
		return err
	}

	newest, found, err := store.NewestGeneration(ctx, image)
	if err != nil {
		return err
	}

	if !found {
		fmt.Printf("no generation of %s has been published; a build is due\n", image)

		return nil
	}

	age := time.Since(newest.Built)
	if age < *maxAge {
		fmt.Printf("%s was published %s ago, which is inside %s; nothing to do\n",
			newest.Name, age.Round(time.Minute), *maxAge)

		return errNothingToBuild
	}

	fmt.Printf("the newest generation %s is %s old; a build is due\n",
		newest.Name, age.Round(time.Minute))

	return nil
}

// errNothingToBuild says a rebuild is not due. It is an ANSWER rather than a
// failure, which is why it carries a status of its own.
var errNothingToBuild = &exitError{code: 2, msg: "a recent generation already exists"}

// firecrackerTierImage is the image this deployment's microVM tiers boot.
func firecrackerTierImage(cfg *config.Config) string {
	for i := range cfg.Tiers {
		if cfg.Tiers[i].Provider == config.ProviderFirecracker && cfg.Tiers[i].Image != "" {
			return cfg.Tiers[i].Image
		}
	}

	return ""
}

// cmdImagesVerify boots one microVM from an image and makes the guest prove it works.
func cmdImagesVerify(ctx context.Context, args []string) error {
	fs := newFlagSet("billet images verify")
	cfgPath := addConfigFlag(fs)
	wait := fs.Duration("wait", 3*time.Minute, "how long to give the guest to report back")
	record := fs.Bool("record", true,
		"on success, mark this generation verified so `@verified` resolves to it")

	rest, err := parseWithName(fs, args)
	if err != nil {
		return err
	}

	if rest == "" {
		return errors.New("usage: billet images verify <image@generation>")
	}

	// THE GENERATION IS REQUIRED, not defaulted, for the same reason the provider
	// refuses a bare image name: a clone of "the image" is a clone of whatever it
	// happens to be right now, and the point of a generation is that a job holds a
	// clone of something that cannot change underneath it. Verifying an unnamed
	// thing verifies nothing in particular.
	if !strings.Contains(rest, "@") {
		return fmt.Errorf("billet images verify: %q names no generation; verify a specific one, "+
			"like ubuntu-2404-x64@g20260814061844, because that is what a tier boots", rest)
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}

	// BOTH SECTIONS ARE OPTIONAL, and a remote microVM node is exactly the shape
	// that has one and not the other: it dials a control plane it does not run, so
	// it carries `node:` and no `server:` at all. Dereferencing either without
	// asking turned the ordinary case into a panic.
	if cfg.Node == nil {
		return errors.New("billet images verify: this config has no node section, so it " +
			"describes no machine that could boot a guest image")
	}

	if cfg.Node.Provider != config.ProviderFirecracker {
		return fmt.Errorf("billet images verify: this node's provider is %s, and only firecracker "+
			"boots a guest image; run this on a machine that runs microVMs", cfg.Node.Provider)
	}

	// THE SAME DERIVATION `billet node` USES, rather than a second one that agrees
	// with it by luck. The identity decides which jails this command can see, so a
	// verification that derived it differently would either see nothing of its own
	// or, worse, claim someone else's.
	deployment, err := verifyDeploymentID(cfg)
	if err != nil {
		return err
	}

	// AN IDENTITY OF ITS OWN, AND THIS IS NOT TIDINESS.
	//
	// A probe's lease is invented here and the allocator has never heard of it. Run
	// under the node's identity, the node daemon's sweep — which lists every
	// instance this deployment owns and destroys any whose lease it cannot account
	// for — finds the probe, correctly concludes it is an orphan, and kills it. On a
	// five-minute tick against a boot-to-report window of a minute or so, that lands
	// inside the window often enough to matter, and what an operator sees is the
	// weekly gate reporting that a perfectly good image "does NOT work".
	//
	// A separate identity puts the probe outside what that sweep will touch: a jail
	// whose owner marker is another billet's is "not ours to report and emphatically
	// not ours to destroy". This command cleans up its own, below and on the way out.
	prov, err := newProvider(cfg, probeDeployment(deployment))
	if err != nil {
		return err
	}

	lease, err := probeLeaseID()
	if err != nil {
		return err
	}

	// AND ANYTHING AN EARLIER RUN LEFT BEHIND GOES FIRST. The probe's name is the
	// same on this host every time, so this is one idempotent Destroy of one name
	// rather than a sweep of whatever happened to be there — which could not be made
	// safe, because the provider deliberately reports jails with no owner marker and
	// those are indistinguishable from a real node launch caught mid-creation.
	if err := destroyProbe(ctx, prov, provider.InstanceName(lease)); err != nil {
		return err
	}

	if err := verifyGuestImage(ctx, prov, cfg.Node.Firecracker.Bridge, rest, lease, *wait); err != nil {
		return err
	}

	// RECORDED, WHICH IS WHAT MAKES THIS A PROMOTION RATHER THAN A REPORT.
	//
	// Without it the whole schedule ends in a sentence on a terminal nobody is
	// watching: build, verify, print "point a tier at it when you are ready", and
	// then nothing happens. A fleet goes on booting whatever generation somebody last
	// typed into a config file, while a verified image is published every week beside
	// it. That is the state this command existed in until now.
	//
	// A tier naming `@verified` takes it up from here with no config edit and no
	// restart, and one that pins a generation is unaffected.
	if *record {
		store, err := ceph.New(*cfg.Node.Ceph)
		if err != nil {
			return err
		}

		if err := store.MarkVerified(ctx, rest, time.Now()); err != nil {
			return err
		}

		fmt.Printf("\nrecorded %s as verified; a tier naming @%s will boot it\n",
			rest, ceph.Verified)
	}

	return nil
}

// verifyGuestImage launches one microVM and waits for the guest to report on itself.
func verifyGuestImage(
	ctx context.Context, prov provider.Provider, bridge, image, lease string, wait time.Duration,
) error {
	// A LISTENER ON THIS MACHINE, because the assertion has to be made BY THE GUEST.
	// Anything the host can check on its own was already green for an image that ran
	// no job at all.
	report := make(chan string, 1)

	// THE SECRET IS MINTED BEFORE THE LISTENER, because the listener uses it to
	// decide what is even worth keeping.
	secret, err := verificationSecret()
	if err != nil {
		return err
	}

	srv, addr, serveErr, err := listenForGuestReport(ctx, bridge, secret, report)
	if err != nil {
		return err
	}

	defer func() {
		if err := srv.Close(); err != nil {
			fmt.Printf("warning: the report listener did not close: %v\n", err)
		}
	}()

	name := provider.InstanceName(lease)

	// WHAT THE GUEST IS ASKED TO SAY, and each part answers a way the image can be
	// broken while looking fine:
	//
	//   whoami      the agent dropped to the unprivileged account rather than
	//               running somebody's CI as root
	//   jit         the registration reached the command's environment intact,
	//               which is the whole delivery path in one value
	//   runner      the actions runner binary EXECUTES — a debootstrap rootfs is
	//               exactly where a .NET binary fails for a missing libicu, and
	//               that surfaces as every job failing to start
	//   docker      the daemon is up on this kernel, which check-config.sh can only
	//               predict, and a container actually ran
	probe := strings.Join([]string{
		`echo "whoami=$(whoami)"`,
		`echo "jit=$ACTIONS_RUNNER_INPUT_JITCONFIG"`,
		`echo "runner=$(cd /home/runner/runner && ./bin/Runner.Listener --version 2>&1 | head -1)"`,
		`echo "docker=$(docker info --format '{{.ServerVersion}} storage={{.Driver}} ` +
			`cgroups={{.CgroupVersion}}' 2>&1 | head -1)"`,
		`echo "container=$(docker run --rm hello-world 2>&1 | grep -ci 'working correctly' || echo 0)"`,
	}, "; ")

	spec := provider.Spec{
		Name:   name,
		Image:  image,
		VCPU:   2,
		Memory: 2 * config.GiB,
		// RUN ONCE AND POSTED, rather than run for a console that does not exist and
		// then run again to be sent: billet passes no console= to a guest, so the
		// first copy's output went nowhere while doubling the time to report and
		// pulling the test container twice.
		Command: []string{"/bin/sh", "-c",
			`report=$(` + probe + `); curl -sf --max-time 20 --data-binary "$report" http://` +
				addr + `/report`},
		Trust:     provider.TrustTrusted,
		JITConfig: secret,
	}

	fmt.Printf("verifying %s\n", image)

	if _, err := prov.Launch(ctx, spec); err != nil {
		// A LAUNCH ERROR IS NOT PROOF THAT NOTHING STARTED, which the provider says
		// in as many words: a cancelled context can kill this process after the work
		// was accepted, and the backend's own unwind can itself fail. So the probe is
		// reconciled rather than assumed away — otherwise a failed verification is
		// also a leaked uid, device name and cloned disk.
		return errors.Join(
			fmt.Errorf("billet images verify: %s did not launch: %w", image, err),
			destroyProbe(ctx, prov, name),
		)
	}

	verdict := awaitGuestReport(ctx, report, serveErr, image, secret, wait)

	// CLEANUP IS PART OF THE RESULT, NOT A WARNING BESIDE IT. As a bare defer this
	// printed a line and returned success, so the weekly job would announce a
	// verified image while probes accumulated — each holding a uid, a device name
	// and a cloned disk, and each invisible to the node's sweep by design.
	return errors.Join(verdict, destroyProbe(ctx, prov, name))
}

// awaitGuestReport waits for the guest to say something, or for a reason it cannot.
func awaitGuestReport(
	ctx context.Context, report <-chan string, serveErr <-chan error,
	image, secret string, wait time.Duration,
) error {
	timer := time.NewTimer(wait)
	defer timer.Stop()

	select {
	case body := <-report:
		return checkGuestReport(body, secret)

	case err := <-serveErr:
		// THE LISTENER DIED, WHICH IS NOT THE IMAGE'S FAULT. Without this the wait
		// runs to its deadline and the verdict blames a guest that was reporting to
		// a socket nobody was reading.
		return fmt.Errorf("billet images verify: the listener the guest reports to failed, so "+
			"nothing here can say whether %s works: %w", image, err)

	case <-timer.C:
		return fmt.Errorf("billet images verify: %s booted and never reported back within %s, "+
			"so it cannot be shown to run a job; boot it by hand with a console "+
			"(console=ttyS0 systemd.journald.forward_to_console=1) to read the agent's own "+
			"account of itself", image, wait)

	case <-ctx.Done():
		return ctx.Err()
	}
}

// destroyProbe removes a verification's microVM, asking first whether there is one.
//
// A BOUNDED CONTEXT OF ITS OWN. Cleanup has to outlive a cancelled or expired parent
// — that is the whole reason it does not inherit one — but WithoutCancel alone
// removes the deadline as well, so a teardown that wedges would hang a scheduled job
// until systemd killed it two hours later.
func destroyProbe(ctx context.Context, prov provider.Provider, name string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	defer cancel()

	// DESTROYED WITHOUT ASKING FIRST, because "there is no jail" is not "there is
	// nothing left". The firecracker backend handles exactly that state: it releases
	// the uid and device-name claims and discards the root disk for a launch that got
	// far enough to take them and then lost its jail. A Find-then-skip returned
	// success over precisely that residue, and since the probe is deliberately not
	// owned by the node, nothing else would ever collect it.
	//
	// Destroy is idempotent, so this is also what clears anything an earlier run
	// left: the probe's name is the same on this host every time.
	if err := prov.Destroy(ctx, name); err != nil {
		return fmt.Errorf("billet images verify: the probe %s was not cleaned up and holds a "+
			"uid, a device name and a cloned disk; nothing else will reap it, because it is "+
			"deliberately not owned by the node: %w", name, err)
	}

	return nil
}

// listenForGuestReport serves the address the guest posts its report to.
func listenForGuestReport(
	ctx context.Context, bridge, secret string, report chan<- string,
) (*http.Server, string, <-chan error, error) {
	// ON THE BRIDGE'S OWN ADDRESS AND AN ARBITRARY PORT. The guest reaches this
	// over the bridge, so loopback would be a listener it cannot see — and binding
	// every interface would expose a port that accepts an unauthenticated report to
	// networks that have no business reaching it. A fixed port would collide with
	// whatever else the operator runs.
	host, err := hostAddrOnBridge(bridge, 0)
	if err != nil {
		return nil, "", nil, err
	}

	var lc net.ListenConfig

	ln, err := lc.Listen(ctx, "tcp", host)
	if err != nil {
		return nil, "", nil, fmt.Errorf("billet images verify: listen for the guest's report on "+
			"%s: %w", host, err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/report", func(w http.ResponseWriter, r *http.Request) {
		// A REPORT IS A POST. Anything else on this bridge poking the port is not
		// the guest, and reading a body from it is work done on somebody else's
		// behalf.
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)

			return
		}

		// BOUNDED, because this is a report from a guest running somebody's image
		// and the alternative is letting it decide how much this process allocates.
		body, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
		if err != nil {
			http.Error(w, "could not read the report", http.StatusBadRequest)

			return
		}

		// THE SECRET IS CHECKED HERE, NOT ONLY IN THE VERDICT, because the channel
		// holds ONE report and the first arrival wins it. The bridge this listens on
		// is carrying other guests running somebody's CI, so a stray POST — or a
		// scanner — would take the slot, the real report would be dropped by the
		// non-blocking send, and the verdict would be a confident FAIL saying the
		// registration never arrived. Wrong, and misdiagnosed.
		//
		// A false PASS is not reachable either way: the secret travels only in MMDS
		// V2, which is session-token gated and per-interface, so nothing else on the
		// bridge can learn it.
		if !strings.Contains(string(body), "jit="+secret) {
			w.WriteHeader(http.StatusNoContent)

			return
		}

		select {
		case report <- string(body):
		default:
		}

		w.WriteHeader(http.StatusNoContent)
	})

	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}

	// SERVE'S FAILURE IS REPORTED RATHER THAN DISCARDED. Swallowed, a listener that
	// died on startup left the verification waiting out its full deadline and then
	// blaming the image for not reporting to a socket nobody was reading.
	failed := make(chan error, 1)

	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			failed <- err
		}
	}()

	return srv, ln.Addr().String(), failed, nil
}

// checkGuestReport turns what the guest said into a verdict.
func checkGuestReport(body, secret string) error {
	fmt.Println()

	for _, line := range strings.Split(strings.TrimSpace(body), "\n") {
		fmt.Println("  " + strings.TrimSpace(line))
	}

	fmt.Println()

	var failures []string

	// THE REGISTRATION FIRST, because it is the one that proves the report is about
	// this launch. Everything else could be true of any guest on the host.
	if !strings.Contains(body, "jit="+secret) {
		failures = append(failures, "the registration did not reach the command's environment, "+
			"so a real job would start no runner")
	}

	if !strings.Contains(body, "whoami=runner") {
		failures = append(failures, "the command did not run as the unprivileged runner account")
	}

	if !hasVersion(body, "runner=") {
		failures = append(failures, "the actions runner binary did not report a version, so a "+
			"job would fail at startup (a debootstrap rootfs missing libicu looks exactly "+
			"like this)")
	}

	if !hasVersion(body, "docker=") {
		failures = append(failures, "the docker daemon did not answer on this kernel")
	}

	if strings.Contains(body, "container=0") {
		failures = append(failures, "docker did not run a container (this one needs egress to a "+
			"registry, so it can also mean the bridge has none)")
	}

	if len(failures) > 0 {
		return fmt.Errorf("billet images verify: this image cannot run a job:\n  - %s",
			strings.Join(failures, "\n  - "))
	}

	fmt.Println("this image boots, takes its registration from the metadata service, runs the")
	fmt.Println("actions runner and runs a container.")

	return nil
}

// hasVersion reports whether a field of the guest's report starts with a digit.
//
// NOT A PREFIX MATCH ON TODAY'S MAJOR. `runner=2.` and `docker=2` were both true of
// the versions in front of me and would turn the first runner 3.x or Docker 30.x
// into a weekly gate that fails until somebody edits this file — a false alarm about
// a healthy image, which is the failure this whole command exists to avoid producing.
//
// What actually distinguishes a working answer from a broken one is that the program
// ANSWERED: a missing binary or a missing shared library leaves the field empty or
// carrying an error message, and neither begins with a digit.
func hasVersion(body, field string) bool {
	_, after, found := strings.Cut(body, field)
	if !found {
		return false
	}

	value, _, _ := strings.Cut(after, "\n")

	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}

	return value[0] >= '0' && value[0] <= '9'
}

// verifyDeploymentID is the identity this machine's microVMs are owned by.
//
// THE SAME RULE `billet node` FOLLOWS, deliberately reusing it rather than agreeing
// with it by luck: the certificate outranks the config file, a `server:` section
// answers when there is no certificate, and a node that has neither can only be
// speaking for its own state directory.
//
// It decides which jails this command can see, so a verification that derived it
// differently would either find nothing of its own or claim somebody else's.
func verifyDeploymentID(cfg *config.Config) (string, error) {
	// A bundle is proof issued BY the control plane. Loading it is best-effort here:
	// a machine that has not enrolled yet can still verify an image, and refusing on
	// that would make this command need a control plane to check a local artifact.
	bundle, err := nodeBundle(cfg)
	if err != nil {
		// A machine that has not enrolled yet can still verify an image, and refusing
		// here would make a local check depend on a control plane. The next rule
		// answers instead.
		bundle = nil
	}

	deployment, err := nodeDeploymentID(cfg, bundle)
	if err != nil {
		return "", err
	}

	if deployment != "" {
		return deployment, nil
	}

	// Its own directory is the only answer available — the same fallback the node
	// documents at node.state_dir.
	return state.DeploymentID(cfg.Node.StateDir)
}

// cmdImagesPromote and cmdImagesUnpromote are the manual half of promotion.
//
// THE AUTOMATIC PATH IS `verify --record`, and this exists for the two moments it
// cannot cover: adopting a generation that was verified before this recorded
// anything, and taking one back. Rollback is the important one — it is what somebody
// reaches for at the moment a bad image is in front of every job, so it is one
// command against the cluster rather than an edit on every node.
func cmdImagesPromote(ctx context.Context, args []string, verified bool) error {
	name := "billet images promote"
	if !verified {
		name = "billet images unpromote"
	}

	fs := newFlagSet(name)
	cfgPath := addConfigFlag(fs)

	rest, err := parseWithName(fs, args)
	if err != nil {
		return err
	}

	if rest == "" {
		return fmt.Errorf("usage: %s <image@generation>", name)
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}

	if cfg.Node == nil || cfg.Node.Ceph == nil {
		return errors.New(name + ": this config names no ceph cluster")
	}

	store, err := ceph.New(*cfg.Node.Ceph)
	if err != nil {
		return err
	}

	if verified {
		if err := store.MarkVerified(ctx, rest, time.Now()); err != nil {
			return err
		}

		fmt.Printf("%s is verified; a tier naming @%s will boot it\n", rest, ceph.Verified)

		return nil
	}

	if err := store.UnmarkVerified(ctx, rest); err != nil {
		return err
	}

	newest, found, err := store.NewestVerified(ctx, rest)
	if err != nil {
		return err
	}

	// SAYING WHAT HAPPENS NEXT, because withdrawing a verification is done in a hurry
	// and the question immediately after it is "so what boots now".
	if found {
		fmt.Printf("%s is no longer verified; @%s now resolves to %s\n",
			rest, ceph.Verified, newest.Name)

		return nil
	}

	fmt.Printf("%s is no longer verified, and NO generation is — a tier naming @%s now has "+
		"nothing to boot\n", rest, ceph.Verified)

	return nil
}

// cmdImagesReap removes generations nothing needs.
//
// SAFE FOR RUNNING JOBS BY CONSTRUCTION, which is the measurement the whole design
// rests on: clone v2 removes a snapshot with a live child, returns 0, and the child
// stays usable. So this never has to ask whether a generation is in use — retention
// is only about what might still be BOOTED, and there is no liveness check here to
// get wrong.
//
// THE PLAN AND THE ACTION SHARE ONE FUNCTION. A `--dry-run` computed by different
// code than the operation is a preview that eventually stops describing it, which
// for an irreversible command against a cluster is the property most worth having.
func cmdImagesReap(ctx context.Context, args []string) error {
	fs := newFlagSet("billet images reap")
	cfgPath := addConfigFlag(fs)
	keep := fs.Int("keep", 3, "how many VERIFIED generations to leave, newest first")
	dryRun := fs.Bool("dry-run", false, "print what would be removed and remove nothing")

	rest, err := parseWithName(fs, args)
	if err != nil {
		return err
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}

	if cfg.Node == nil || cfg.Node.Ceph == nil {
		return errors.New("billet images reap: this config names no ceph cluster")
	}

	image := rest
	if image == "" {
		image = firecrackerTierImage(cfg)
	}

	if image == "" {
		return errors.New("billet images reap: no image given and no firecracker tier names one")
	}

	store, err := ceph.New(*cfg.Node.Ceph)
	if err != nil {
		return err
	}

	all, err := store.Generations(ctx, image)
	if err != nil {
		return err
	}

	verified, err := store.VerifiedGenerations(ctx, image)
	if err != nil {
		return err
	}

	// EVERY TIER'S IMAGE, not just the one being reaped: a deployment can pin
	// several, and a generation kept for one tier is kept.
	pinned := make([]string, 0, len(cfg.Tiers))
	for i := range cfg.Tiers {
		if cfg.Tiers[i].Image != "" {
			pinned = append(pinned, cfg.Tiers[i].Image)
		}
	}

	plan := ceph.PlanReap(all, verified, ceph.Retention{Keep: *keep, Pinned: pinned})

	for _, item := range plan {
		if item.Reason != "" {
			fmt.Printf("  keep    %s  (%s)\n", item.Generation.Name, item.Reason)
		}
	}

	removing := 0

	for _, item := range plan {
		if item.Reason == "" {
			removing++

			fmt.Printf("  remove  %s\n", item.Generation.Name)
		}
	}

	if removing == 0 {
		fmt.Println("nothing to reap")

		return nil
	}

	if *dryRun {
		fmt.Printf("\n%d generation(s) would be removed; this was a dry run\n", removing)

		return nil
	}

	removed, err := store.Reap(ctx, image, plan)

	fmt.Printf("\nremoved %d generation(s)\n", len(removed))

	return err
}
