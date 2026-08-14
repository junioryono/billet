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

	prov, err := newProvider(cfg, deployment)
	if err != nil {
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

	srv, addr, err := listenForGuestReport(ctx, bridge, report)
	if err != nil {
		return err
	}

	defer func() {
		if err := srv.Close(); err != nil {
			fmt.Printf("warning: the report listener did not close: %v\n", err)
		}
	}()

	// A SECRET THIS RUN INVENTED, so that a report proves THIS guest read THIS
	// registration. A fixed string would be satisfied by a stale process, an earlier
	// microVM somebody forgot to destroy, or a cached anything.
	secret, err := verificationSecret()
	if err != nil {
		return err
	}

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
		Command: []string{"/bin/sh", "-c",
			probe + `; curl -sf --max-time 20 --data-binary "$(` + probe + `)" http://` + addr + `/report`},
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
	ctx context.Context, bridge string, report chan<- string,
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

	if !strings.Contains(body, "runner=2.") {
		failures = append(failures, "the actions runner binary did not report a version, so a "+
			"job would fail at startup (a debootstrap rootfs missing libicu looks exactly "+
			"like this)")
	}

	if !strings.Contains(body, "docker=2") && !strings.Contains(body, "docker=1") {
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
