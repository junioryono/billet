// Package docker runs jobs in containers.
//
// ISOLATION IS MATERIALLY WEAKER THAN A MICROVM. A container shares the host
// kernel, so this backend is for trying billet out and for developing it on a
// machine with no hypervisor — it is not the production story, and it must
// refuse untrusted pull-request work outright rather than warn about it.
//
// It exists because it was the only backend this project could execute at all,
// and it remains the only one that runs on the machine billet is running on.
// Firecracker needs Linux and /dev/kvm; tart needs Apple Silicon and a licence
// carve-out; ec2 exists now but launches somewhere else and needs an account.
// Writing the whole launch path with nothing able to run it is how the launch
// path ends up wrong in ways no test notices.
package docker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/provider"
)

// Instance is provider.Instance, aliased so this file does not repeat the
// package name on every line.
type Instance = provider.Instance

// ownerLabel carries the DEPLOYMENT identity of the billet that started a
// container — not its node name, which defaults to the hostname and is therefore
// shared by every billet installation on one machine. See state.DeploymentID.
//
// This label is what List filters on, and List feeds a loop that destroys, so a
// label two installations can both carry is a way for one to destroy the other's
// live jobs.
//
// ownerLabel marks every container billet started, so orphans left by a crash
// can be found without guessing from names.
const ownerLabel = "sh.billet.owner"

// jitEnvVar is what the GitHub runner reads its single-use registration from.
// The name is the runner's, not billet's, and it is what makes the stock image
// usable unmodified.
const jitEnvVar = "ACTIONS_RUNNER_INPUT_JITCONFIG"

// Provider launches containers through the docker CLI.
//
// The CLI rather than the Docker SDK on purpose: this backend is a convenience,
// and billet ships as one static binary. Pulling in a large client library for a
// trial-only path is a poor trade, and the CLI is the interface an operator can
// reproduce by hand when something goes wrong.
type Provider struct {
	log *slog.Logger
	// docker is the binary to invoke, overridable so tests can substitute a stub
	// and so an operator with podman can point at it.
	docker string
	// owner tags containers so `docker ps` and orphan cleanup can find them.
	owner string
}

// Option configures a Provider.
type Option func(*Provider)

// WithLogger sets the logger. The default is slog.Default().
func WithLogger(log *slog.Logger) Option {
	return func(p *Provider) { p.log = log }
}

// WithBinary sets the container CLI to invoke, e.g. "podman".
func WithBinary(path string) Option {
	return func(p *Provider) { p.docker = path }
}

// New builds a docker provider. owner names this billet deployment and is
// written onto every container it starts.
func New(owner string, opts ...Option) *Provider {
	p := &Provider{log: slog.Default(), docker: "docker", owner: owner}

	for _, opt := range opts {
		opt(p)
	}

	return p
}

// Kind reports the backend this is.
func (p *Provider) Kind() config.ProviderKind { return config.ProviderDocker }

// Accepts refuses anything that is not established as trusted.
//
// A container shares the host kernel, so this backend is for trials and
// development rather than for code billet cannot vouch for. UNKNOWN is refused
// alongside untrusted: a caller who has not classified a job has not established
// it is safe to run here, and treating the zero value as probably-fine is how a
// refusal gets bypassed by omission rather than by decision.
func (p *Provider) Accepts(trust provider.TrustClass) error {
	if trust == provider.TrustTrusted {
		return nil
	}

	return fmt.Errorf(
		"docker: refusing to run %s work in a container: this backend shares the host kernel "+
			"and is for trials and development, not for code billet cannot vouch for", trust)
}

// Launch starts one container running the job its JIT config names.
func (p *Provider) Launch(ctx context.Context, spec provider.Spec) (*provider.Instance, error) {
	if spec.Name == "" {
		return nil, errors.New("docker: a spec needs a name")
	}

	if spec.JITConfig == "" {
		return nil, fmt.Errorf("docker: %s has no JIT config, so nothing would register", spec.Name)
	}

	// Checked again here, not only via Accepts. A caller is expected to ask first
	// so a refusal costs no runner registration, but a backend that only refuses
	// when asked politely is not a boundary.
	if err := p.Accepts(spec.Trust); err != nil {
		return nil, fmt.Errorf("%w (job %s)", err, spec.Name)
	}

	// REFUSED, not defaulted. Every stock runner image's default command is a
	// shell, so a spec with no command produces a container that starts, exits at
	// once, and reports success the whole way — billet logs a started runner and
	// the job stays queued until GitHub gives up on it.
	//
	// Refusing turns that into a launch failure, which the caller already handles
	// by handing the capacity back. Defaulting to ./run.sh instead would be a
	// guess that happens to suit one image and silently breaks any other.
	if len(spec.Command) == 0 {
		return nil, fmt.Errorf(
			"docker: %s has no command, so the image's default would run instead of the "+
				"runner and the container would exit immediately", spec.Name)
	}

	// THE CREDENTIAL GOES IN A FILE, NOT IN ARGV.
	//
	// `docker run -e VAR=value` puts the value in this process's command line,
	// where every user on the host can read it out of ps. The JIT config is a
	// live runner registration until it is consumed, so that is a credential
	// disclosure with a working-looking implementation — the same class of
	// mistake as the App key argv residual already documented in CLAUDE.md.
	//
	// --env-file keeps it off the command line entirely. The file is 0600 and
	// removed as soon as docker has read it.
	envFile, cleanup, err := p.writeEnvFile(spec)
	if err != nil {
		return nil, err
	}

	defer cleanup()

	args := []string{
		"run", "--detach",
		"--name", spec.Name,
		"--label", ownerLabel + "=" + p.owner,
		"--env-file", envFile,
	}

	if spec.VCPU > 0 {
		args = append(args, "--cpus", strconv.Itoa(spec.VCPU))
	}

	if spec.Memory > 0 {
		args = append(args, "--memory", strconv.FormatInt(int64(spec.Memory), 10))
	}

	if spec.SHM > 0 {
		// Sized per tier because the default 64MB breaks Chromium and a Postgres
		// service container in ways that surface as unrelated crashes.
		args = append(args, "--shm-size", strconv.FormatInt(int64(spec.SHM), 10))
	}

	// AND THE COMMAND, because the image's default one is a shell.
	//
	// Found on the first job billet was ever given. The container started, the JIT
	// config was in its environment, `docker run` reported success and billet
	// logged "started a runner" — and eighteen seconds later the container had
	// exited 0 with the job still queued, because the image's default CMD is
	// /bin/bash and a detached bash with no TTY has nothing to do.
	//
	// Every signal available to billet said the launch worked. The runner never
	// started, so it never took the job, so GitHub kept it queued and eventually
	// would have reassigned it — the failure mode is a tier that looks healthy and
	// silently runs nothing, which is precisely what --dry-run could not have
	// caught.
	args = append(args, spec.Image)
	args = append(args, spec.Command...)

	stdout, err := p.run(ctx, args...)
	if err != nil {
		return nil, fmt.Errorf("docker: launch %s: %w", spec.Name, err)
	}

	// STDOUT ONLY, and the separation is the whole point. `docker run --detach`
	// prints the container id to stdout and everything else — "Unable to find
	// image locally", pull progress, layer digests — to stderr. Reading a
	// combined buffer worked perfectly against a cached image and returned
	// "Unable to fi" as the container id the first time a pull was needed. A
	// real docker run found that; the stub never could.
	id := strings.TrimSpace(stdout)
	if id == "" {
		return nil, fmt.Errorf("docker: launch %s reported no container id", spec.Name)
	}

	p.log.Info("launched container", "runner", spec.Name, "container", short(id), "image", spec.Image)

	return &provider.Instance{ID: id, Name: spec.Name}, nil
}

// Destroy removes a container, whether or not it is still running.
//
// Idempotent: an id that is already gone is success. Teardown runs on paths that
// have already failed once, and erroring there turns recoverable state into
// stuck state.
func (p *Provider) Destroy(ctx context.Context, id string) error {
	if id == "" {
		return errors.New("docker: destroy needs a container id")
	}

	// --force kills it if running; --volumes takes the anonymous volumes with
	// it, which are otherwise the thing that quietly fills a disk over weeks.
	_, err := p.run(ctx, "rm", "--force", "--volumes", id)
	if err == nil {
		p.log.Info("destroyed container", "container", short(id))

		return nil
	}

	// "Already gone" is the idempotent case and the CLI reports it as a failure,
	// so it has to be recognised from the message. That is fragile, and it is
	// narrow on purpose: anything unrecognised falls through to a real error
	// rather than being swallowed as success.
	//
	// Both docker's and podman's phrasings are matched, because podman is an
	// explicitly supported binary and phrases it differently. A locale or version
	// that phrases it a third way fails LOUDLY, which is the right direction to
	// be wrong in — a swallowed teardown failure is a container nobody knows is
	// running.
	if isAlreadyGone(err) {
		return nil
	}

	return fmt.Errorf("docker: destroy %s: %w", short(id), err)
}

// Find reports the container with that name, and whether there was one.
//
// Filtered by billet's own label as well as the name, so a container somebody
// else happened to name the same way is not adopted — and adoption is the
// dangerous direction here, because the caller may go on to destroy it.
//
// The name filter is a SUBSTRING match in docker, not an exact one, so the
// results are compared exactly afterwards. Without that, a lookup for
// `billet-abc` would happily return `billet-abcdef`.
func (p *Provider) Find(ctx context.Context, name string) (*Instance, bool, error) {
	found, err := p.list(ctx, "name="+name)
	if err != nil {
		return nil, false, err
	}

	for _, inst := range found {
		if inst.Name == name {
			return inst, true, nil
		}
	}

	return nil, false, nil
}

// List reports every container billet started here, running or not.
//
// Stopped ones count. A container that exited still holds its name, its
// anonymous volumes and its disk, and it still blocks a relaunch under the same
// name — so reconciliation has to see it.
func (p *Provider) List(ctx context.Context) ([]*Instance, error) {
	return p.list(ctx)
}

// list runs `docker ps` with billet's owner label plus any extra filters.
func (p *Provider) list(ctx context.Context, filters ...string) ([]*Instance, error) {
	args := make([]string, 0, 6+2*len(filters))
	args = append(args,
		"ps", "--all",
		"--filter", "label="+ownerLabel+"="+p.owner,
		// Tab-separated rather than JSON: the format is billet's own, so there is
		// nothing to parse defensively, and a name cannot contain a tab.
		"--format", "{{.ID}}\t{{.Names}}\t{{.State}}",
	)

	for _, f := range filters {
		args = append(args, "--filter", f)
	}

	out, err := p.run(ctx, args...)
	if err != nil {
		return nil, fmt.Errorf("docker: list billet containers: %w", err)
	}

	var instances []*Instance

	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}

		// SplitN with an exact count, not Cut. Cut proves a tab EXISTS; it does not
		// prove there is only one, so a wrapper printing an extra column used to
		// land inside the name — and a name with a suffix on it still parses as
		// billet's, so the lease lookup misses and the sweep destroys a live job.
		fields := strings.Split(line, "\t")
		if len(fields) != 3 || fields[0] == "" || fields[1] == "" {
			// REPORTED, not skipped. Neither a container id nor a name can contain
			// a tab, so this line is not something billet knows how to read — a
			// docker wrapper script, a podman version that formats differently, a
			// warning on stdout. Skipping it would let List report success while
			// omitting a billet container, and the caller's whole purpose is to act
			// on what is missing from that list.
			return nil, fmt.Errorf("docker: cannot read %q as an id, a name and a state; "+
				"is `docker` a wrapper that prints extra output?", line)
		}

		id, name, state := fields[0], fields[1], fields[2]

		instances = append(instances, &Instance{
			ID:   id,
			Name: name,
			// docker reports created/running/paused/restarting/removing/exited/dead.
			//
			// CREATED IS NOT RUNNING, and it counted as running until this was
			// pointed out. A container that exists but was never started will never
			// be started now — the process that would have done it is gone — so
			// adopting it means heartbeating its lease forever for a job that cannot
			// begin. It is the one state that looks alive and is not.
			//
			// An unrecognised state still counts as running: the caller destroys
			// what is not, and a state billet has never heard of is not evidence
			// that a job is over.
			Running: state != "created" && state != "exited" &&
				state != "dead" && state != "removing",
		})
	}

	return instances, nil
}

// writeEnvFile puts the runner's environment somewhere docker can read it and
// other processes cannot.
func (p *Provider) writeEnvFile(spec provider.Spec) (string, func(), error) {
	dir, err := os.MkdirTemp("", "billet-jit-")
	if err != nil {
		return "", nil, fmt.Errorf("docker: create env directory: %w", err)
	}

	cleanup := func() {
		if err := os.RemoveAll(dir); err != nil {
			// Worth saying loudly: what is left behind is a live credential.
			p.log.Error("could not remove the JIT config file; it holds a runner registration",
				"path", dir, "error", err)
		}
	}

	path := filepath.Join(dir, "runner.env")

	// 0600 before anything is written to it. MkdirTemp already gives 0700 on the
	// directory, and both matter: the file is the credential and the directory is
	// what stops a listing.
	body := jitEnvVar + "=" + spec.JITConfig + "\n"

	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		cleanup()

		return "", nil, fmt.Errorf("docker: write env file: %w", err)
	}

	return path, cleanup, nil
}

// run invokes the container CLI and returns its STDOUT.
//
// The streams are kept apart deliberately. docker writes values to stdout and
// narration to stderr, so a combined buffer silently corrupts any value that is
// read back — `docker run --detach` returns the container id, and the first
// time an image needs pulling the combined output starts "Unable to find image
// locally" instead. Stderr still reaches the caller, in the error, because a
// failure that says only "exit status 125" sends the reader nowhere useful.
func (p *Provider) run(ctx context.Context, args ...string) (string, error) {
	// #nosec G204 -- the binary is operator configuration and every argument is
	// built here from typed config, never from job or workflow input. There is no
	// shell: exec.CommandContext passes argv directly, so nothing is interpreted.
	cmd := exec.CommandContext(ctx, p.docker, args...)

	var stdout, stderr bytes.Buffer

	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("%s %s: %w: %s",
			p.docker, args[0], err, strings.TrimSpace(stderr.String()))
	}

	return stdout.String(), nil
}

// isAlreadyGone reports whether a removal failed because there was nothing to
// remove.
//
// Message matching, because neither docker nor podman distinguishes this by exit
// code. Kept to the exact phrasings each is known to use rather than something
// permissive: matching loosely would swallow real failures, and a teardown
// failure that reads as success leaves a container running that nothing is
// tracking.
func isAlreadyGone(err error) bool {
	msg := strings.ToLower(err.Error())

	for _, phrase := range []string{
		"no such container",            // docker
		"no such object",               // docker, some versions
		"not find container",           // podman
		"no container with name or id", // podman
	} {
		if strings.Contains(msg, phrase) {
			return true
		}
	}

	return false
}

// short trims a container id for logs. The full id is never interesting and the
// first twelve characters are what docker itself displays.
func short(id string) string {
	if len(id) > 12 {
		return id[:12]
	}

	return id
}
