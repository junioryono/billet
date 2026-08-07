// Package docker runs jobs in containers.
//
// ISOLATION IS MATERIALLY WEAKER THAN A MICROVM. A container shares the host
// kernel, so this backend is for trying billet out and for developing it on a
// machine with no hypervisor — it is not the production story, and it must
// refuse untrusted pull-request work outright rather than warn about it.
//
// It exists now because it is the only backend this project can execute today.
// Firecracker needs Linux and /dev/kvm; tart needs Apple Silicon and a licence
// carve-out; ec2 needs an account and a network. Writing the whole launch path
// with nothing able to run it is how the launch path ends up wrong in ways no
// test notices.
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

// Launch starts one container running the job its JIT config names.
func (p *Provider) Launch(ctx context.Context, spec provider.Spec) (*provider.Instance, error) {
	if spec.Name == "" {
		return nil, errors.New("docker: a spec needs a name")
	}

	if spec.JITConfig == "" {
		return nil, fmt.Errorf("docker: %s has no JIT config, so nothing would register", spec.Name)
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

	args = append(args, spec.Image)

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

	// "No such container" is the idempotent case and docker reports it as a
	// failure. Matched on the ERROR, which carries stderr — where docker puts
	// that message. Narrow on purpose, so anything else falls through to a real
	// error rather than being swallowed.
	if strings.Contains(strings.ToLower(err.Error()), "no such container") {
		return nil
	}

	return fmt.Errorf("docker: destroy %s: %w", short(id), err)
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

// short trims a container id for logs. The full id is never interesting and the
// first twelve characters are what docker itself displays.
func short(id string) string {
	if len(id) > 12 {
		return id[:12]
	}

	return id
}
