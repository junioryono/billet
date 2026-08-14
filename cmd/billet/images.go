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
	default:
		return fmt.Errorf("billet images: unknown subcommand %q", args[0])
	}
}

// cmdImagesVerify boots one microVM from an image and makes the guest prove it works.
func cmdImagesVerify(ctx context.Context, args []string) error {
	fs := newFlagSet("billet images verify")
	cfgPath := addConfigFlag(fs)
	wait := fs.Duration("wait", 3*time.Minute, "how long to give the guest to report back")

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

	if cfg.Node.Provider != config.ProviderFirecracker {
		return fmt.Errorf("billet images verify: this node's provider is %s, and only firecracker "+
			"boots a guest image; run this on a machine that runs microVMs", cfg.Node.Provider)
	}

	deployment, err := state.DeploymentID(cfg.Server.StateDir)
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

	// AND ANYTHING AN EARLIER RUN LEFT BEHIND GOES FIRST. The separate identity is
	// what keeps the node's sweep off these, which also means nothing else will ever
	// reap one — so a run that was killed between Launch and Destroy would otherwise
	// hold a uid, a device name and a cloned disk until somebody noticed.
	if err := reapEarlierProbes(ctx, prov); err != nil {
		return err
	}

	return verifyGuestImage(ctx, prov, cfg.Node.Firecracker.Bridge, rest, *wait)
}

// verifyGuestImage launches one microVM and waits for the guest to report on itself.
func verifyGuestImage(
	ctx context.Context, prov provider.Provider, bridge, image string, wait time.Duration,
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

	srv, addr, err := listenForGuestReport(ctx, bridge, secret, report)
	if err != nil {
		return err
	}

	defer func() {
		if err := srv.Close(); err != nil {
			fmt.Printf("warning: the report listener did not close: %v\n", err)
		}
	}()

	lease, err := probeLeaseID()
	if err != nil {
		return err
	}

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
		return fmt.Errorf("billet images verify: %s did not launch: %w", image, err)
	}

	// DESTROYED WHATEVER HAPPENS, including on the failure path, because a
	// verification that leaves a microVM behind holds a uid, a device name and a
	// cloned disk — and the next verification draws the same lowest-free name.
	defer func() {
		if err := prov.Destroy(context.WithoutCancel(ctx), name); err != nil {
			fmt.Printf("warning: the probe microVM was not cleaned up: %v\n", err)
		}
	}()

	select {
	case body := <-report:
		return checkGuestReport(body, secret)

	case <-time.After(wait):
		return fmt.Errorf("billet images verify: %s booted and never reported back within %s, "+
			"so it cannot be shown to run a job; boot it by hand with a console "+
			"(console=ttyS0 systemd.journald.forward_to_console=1) to read the agent's own "+
			"account of itself", image, wait)

	case <-ctx.Done():
		return ctx.Err()
	}
}

// listenForGuestReport serves the address the guest posts its report to.
func listenForGuestReport(
	ctx context.Context, bridge, secret string, report chan<- string,
) (*http.Server, string, error) {
	// ON THE BRIDGE'S OWN ADDRESS AND AN ARBITRARY PORT. The guest reaches this
	// over the bridge, so loopback would be a listener it cannot see — and binding
	// every interface would expose a port that accepts an unauthenticated report to
	// networks that have no business reaching it. A fixed port would collide with
	// whatever else the operator runs.
	host, err := hostAddrOnBridge(bridge, 0)
	if err != nil {
		return nil, "", err
	}

	var lc net.ListenConfig

	ln, err := lc.Listen(ctx, "tcp", host)
	if err != nil {
		return nil, "", fmt.Errorf("billet images verify: listen for the guest's report on %s: %w",
			host, err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/report", func(w http.ResponseWriter, r *http.Request) {
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

	go func() {
		//nolint:errcheck // the caller closes this and reports what the guest said
		_ = srv.Serve(ln)
	}()

	return srv, ln.Addr().String(), nil
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

// reapEarlierProbes destroys any microVM a previous verification left behind.
//
// These are only ever this command's own: the provider is built with an identity
// nothing else uses, so List reports exactly the probes of earlier runs and never a
// real job's guest.
func reapEarlierProbes(ctx context.Context, prov provider.Provider) error {
	leftovers, err := prov.List(ctx)
	if err != nil {
		return fmt.Errorf("billet images verify: look for microVMs an earlier verification left "+
			"behind: %w", err)
	}

	for _, inst := range leftovers {
		fmt.Printf("cleaning up %s, left by an earlier verification\n", inst.Name)

		if err := prov.Destroy(ctx, inst.ID); err != nil {
			return fmt.Errorf("billet images verify: an earlier verification left %s behind and "+
				"it could not be removed; it holds a uid, a device name and a cloned disk: %w",
				inst.Name, err)
		}
	}

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
