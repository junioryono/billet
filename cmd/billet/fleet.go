package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/junioryono/billet/internal/cli"
	"github.com/junioryono/billet/internal/releasesource"
	"github.com/junioryono/billet/internal/version"
)

// cmdFleet converges a fleet from an operator's machine with the same
// implementation actions/converge-fleet runs in CI.
func cmdFleet(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: billet fleet converge -inventory <file> [flags]")
	}

	if args[0] == "converge" {
		return cmdFleetConverge(ctx, args[1:])
	}

	return fmt.Errorf("unknown fleet command %q; try converge", args[0])
}

// fleetOptions are the action's inputs, spelled as flags.
type fleetOptions struct {
	inventory, playbook, knownHosts, limit, extraVars, holder string
	sshKeyFile, appKeyFile, environmentFile                   string
	check, noProve                                            bool
	ref, source                                               string
}

func cmdFleetConverge(ctx context.Context, args []string) error {
	var o fleetOptions

	flags := cli.NewFlagSet("billet fleet converge", os.Stdout)
	flags.StringVar(&o.inventory, "inventory", "", "the inventory file (required)")
	flags.StringVar(&o.playbook, "playbook", "", "the playbook to run; the collection's junioryono.billet.fleet when empty")
	flags.StringVar(&o.knownHosts, "known-hosts", "", "a file of pinned host keys, appended to ~/.ssh/known_hosts")
	flags.StringVar(&o.limit, "limit", "", "an Ansible host pattern to limit the run to")
	flags.StringVar(&o.extraVars, "extra-vars", "", "a JSON object of non-secret extra variables")
	flags.StringVar(&o.holder, "holder", "", "the converge-guard holder name; derived from this user and machine when empty")
	flags.StringVar(&o.sshKeyFile, "ssh-key", "", "a private key file to connect with; the SSH agent and ~/.ssh/config when empty")
	flags.StringVar(&o.appKeyFile, "github-app-key", "", "the GitHub App private key file the host role installs")
	flags.StringVar(&o.environmentFile, "environment-file", "", "NAME=value lines put into the playbook's environment "+
		"(connector tokens), never onto a command line")
	flags.BoolVar(&o.check, "check", false, "a dry run: --check --diff, and no idempotence proof")
	flags.BoolVar(&o.noProve, "no-prove-idempotent", false, "skip the second pass that requires changed=0")
	flags.StringVar(&o.ref, "ref", "", "the billet release whose collection converges the fleet; this binary's own when empty")
	flags.StringVar(&o.source, "source", "", "a billet checkout to converge from instead of a release (development)")

	if err := cli.Parse(flags, args); err != nil {
		return err
	}

	if o.inventory == "" {
		return errors.New("billet fleet converge: -inventory is required")
	}

	if o.ref != "" && o.source != "" {
		return errors.New("billet fleet converge: -ref and -source name two collections; give one")
	}

	src, ref, err := fleetSource(ctx, o)
	if err != nil {
		return err
	}

	return runFleetConverge(ctx, src, ref, o)
}

// fleetRepository is where a release's source is fetched from; a var so a test
// can serve a local repository.
var fleetRepository = "https://github.com/" + releasesource.DefaultRepo + ".git"

// fleetSource is the checkout whose action scripts and collection converge the
// fleet, and the ref it names.
//
// A RELEASE, NEVER A MOVING REF. The collection is what reconfigures and
// restarts every host, so the laptop converges with the release this binary is
// (or the one -ref names), exactly as the action's `uses:` ref pins CI. A
// development build has no release to match and must say which it wants.
func fleetSource(ctx context.Context, o fleetOptions) (string, string, error) {
	if o.source != "" {
		src, err := filepath.Abs(o.source)
		if err != nil {
			return "", "", err
		}

		if err := checkFleetSource(src); err != nil {
			return "", "", err
		}

		return src, "source " + src, nil
	}

	ref := o.ref
	if ref == "" {
		ref = version.Version()
		if !version.IsRelease(ref) {
			return "", "", fmt.Errorf("billet fleet converge: this billet is %s, not a release, so it has no "+
				"collection to match; pass -ref vX.Y.Z (the release the fleet runs) or -source <checkout>", ref)
		}
	}

	if !version.IsRelease(ref) {
		return "", "", fmt.Errorf("billet fleet converge: -ref %q is not a release; it must be vX.Y.Z", ref)
	}

	cache, err := os.UserCacheDir()
	if err != nil {
		return "", "", fmt.Errorf("billet fleet converge: no cache directory to keep %s in: %w", ref, err)
	}

	src, err := fetchFleetSource(ctx, filepath.Join(cache, "billet", "fleet"), ref)
	if err != nil {
		return "", "", err
	}

	return src, ref, nil
}

// fleetSourceMarker names the ref a cached checkout holds; it is written last,
// so a checkout interrupted before it is fetched again.
const fleetSourceMarker = ".billet-fleet-ref"

// fetchFleetSource returns root/<ref>, cloning the tag there when it is not
// already complete.
func fetchFleetSource(ctx context.Context, root, ref string) (string, error) {
	dir := filepath.Join(root, ref)

	if marker, err := os.ReadFile(filepath.Join(dir, fleetSourceMarker)); err == nil &&
		strings.TrimSpace(string(marker)) == ref {
		return dir, checkFleetSource(dir)
	}

	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", fmt.Errorf("billet fleet converge: create %s: %w", root, err)
	}

	staging, err := os.MkdirTemp(root, ref+".fetch-")
	if err != nil {
		return "", fmt.Errorf("billet fleet converge: stage %s: %w", ref, err)
	}
	defer os.RemoveAll(staging)

	// `--` BEFORE THE URL: the ref is a checked vX.Y.Z and the URL a constant,
	// but a leading dash in either must never reach git as an option.
	clone := exec.CommandContext(ctx, "git", "clone", "--quiet", "--depth", "1", "--branch", ref,
		"--", fleetRepository, filepath.Join(staging, "src"))
	clone.Stdout, clone.Stderr = os.Stderr, os.Stderr

	if err := clone.Run(); err != nil {
		return "", fmt.Errorf("billet fleet converge: fetch billet %s from %s: %w", ref, fleetRepository, err)
	}

	fetched := filepath.Join(staging, "src")
	if err := checkFleetSource(fetched); err != nil {
		return "", err
	}

	if err := os.WriteFile(filepath.Join(fetched, fleetSourceMarker), []byte(ref+"\n"), 0o600); err != nil {
		return "", err
	}

	if err := os.RemoveAll(dir); err != nil {
		return "", fmt.Errorf("billet fleet converge: replace the incomplete %s: %w", dir, err)
	}

	if err := os.Rename(fetched, dir); err != nil {
		return "", fmt.Errorf("billet fleet converge: install %s: %w", dir, err)
	}

	return dir, nil
}

// fleetScripts are the action's steps this command runs, in its order. The
// WARP enrolment is left out: an operator's machine reaches the fleet on its
// own network, as the action's `reach: none` does.
var (
	fleetSteps   = []string{"prepare.sh", "install-ansible.sh", "converge.sh"}
	fleetFinally = []string{"release-guard.sh", "cleanup.sh"}
)

// checkFleetSource refuses a directory that is not a billet checkout carrying
// the action and the collection together.
func checkFleetSource(dir string) error {
	for _, need := range append(append([]string{"ansible_collections/junioryono/billet/galaxy.yml"},
		prefixed("actions/converge-fleet/", fleetSteps)...), prefixed("actions/converge-fleet/", fleetFinally)...) {
		if _, err := os.Stat(filepath.Join(dir, need)); err != nil {
			return fmt.Errorf("billet fleet converge: %s is not a billet checkout with the converge action: %w", dir, err)
		}
	}

	return nil
}

func prefixed(prefix string, names []string) []string {
	out := make([]string, 0, len(names))
	for _, name := range names {
		out = append(out, prefix+name)
	}

	return out
}

// runFleetConverge runs the action's scripts from src in the action's order,
// carrying GITHUB_ENV and GITHUB_PATH between them as a runner does, and
// releases the guard and cleans up however the converge ends.
func runFleetConverge(ctx context.Context, src, ref string, o fleetOptions) error {
	secrets := map[string]string{}

	for name, file := range map[string]string{
		"BILLET_SSH_PRIVATE_KEY":        o.sshKeyFile,
		"BILLET_GITHUB_APP_PRIVATE_KEY": o.appKeyFile,
		"BILLET_ENVIRONMENT":            o.environmentFile,
	} {
		if file == "" {
			continue
		}

		body, err := os.ReadFile(file)
		if err != nil {
			return fmt.Errorf("billet fleet converge: read %s: %w", file, err)
		}

		secrets[name] = string(body)
	}

	holder := o.holder
	if holder == "" {
		holder = fleetDefaultHolder()
	}

	if err := checkHolder(holder); err != nil {
		return fmt.Errorf("billet fleet converge: %w", err)
	}

	temp, err := os.MkdirTemp("", "billet-fleet-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(temp)

	env := fleetBaseEnv(os.Environ())
	basePath := env["PATH"]
	files := map[string]string{
		"GITHUB_ENV":    filepath.Join(temp, "github-env"),
		"GITHUB_PATH":   filepath.Join(temp, "github-path"),
		"GITHUB_OUTPUT": filepath.Join(temp, "github-output"),
	}

	for name, path := range files {
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			return err
		}

		env[name] = path
	}

	mode := "converge"
	if o.check {
		mode = "check"
	}

	prove := "true"
	if o.noProve {
		prove = "false"
	}

	actionPath := filepath.Join(src, "actions", "converge-fleet")
	env["RUNNER_TEMP"] = temp
	env["GITHUB_ACTION_PATH"] = actionPath
	env["BILLET_ACTION_REF"] = ref
	env["BILLET_CONVERGE_GUARD_HOLDER"] = holder

	inputs := map[string]string{
		"BILLET_MODE":             mode,
		"BILLET_REACH":            "none",
		"BILLET_INVENTORY":        o.inventory,
		"BILLET_PLAYBOOK":         o.playbook,
		"BILLET_KNOWN_HOSTS":      o.knownHosts,
		"BILLET_EXTRA_VARS":       o.extraVars,
		"BILLET_LIMIT":            o.limit,
		"BILLET_PROVE_IDEMPOTENT": prove,
	}

	// THE SECRETS REACH converge.sh ALONE, as the action passes them to that one
	// step: it writes each to a 0600 file under RUNNER_TEMP and unsets it, and no
	// other step has a use for them.
	step := func(name string, extra ...map[string]string) error {
		stepEnv := mergeEnv(env, extra...)
		// The script is one of fleetSteps, inside a checkout checkFleetSource proved.
		cmd := exec.CommandContext(ctx, "bash", filepath.Join(actionPath, name)) //nolint:gosec // G204: a fixed step name in a verified checkout
		cmd.Env = envList(stepEnv)
		cmd.Stdin = nil
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr

		runErr := cmd.Run()

		if err := applyRunnerFiles(env, files["GITHUB_ENV"], files["GITHUB_PATH"], basePath); err != nil {
			return errors.Join(runErr, err)
		}

		if runErr != nil {
			return fmt.Errorf("%s: %w", name, runErr)
		}

		return nil
	}

	var converged error

	for _, name := range fleetSteps {
		extra := []map[string]string{inputs}
		if name == "converge.sh" {
			extra = append(extra, secrets)
		}

		if converged = step(name, extra...); converged != nil {
			break
		}
	}

	var finished error

	for _, name := range fleetFinally {
		// A CANCELLED CONVERGE STILL RELEASES ITS GUARD, which is why this runs on
		// a context of its own: the guard names this run, and a held guard stops
		// the next converge and every rollout on those hosts.
		finalCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Minute)
		cmd := exec.CommandContext(finalCtx, "bash", filepath.Join(actionPath, name)) //nolint:gosec // G204: a fixed step name in a verified checkout
		cmd.Env = envList(mergeEnv(env, inputs))
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr

		if err := cmd.Run(); err != nil {
			finished = errors.Join(finished, fmt.Errorf("%s: %w", name, err))
		}

		cancel()
	}

	if converged != nil {
		return errors.Join(fmt.Errorf("billet fleet converge: %w", converged), finished)
	}

	if finished != nil {
		return fmt.Errorf("billet fleet converge: the converge passed, but %w", finished)
	}

	return nil
}

// fleetBaseEnv is this process's environment without what would make the
// scripts believe they run inside a GitHub job: a runner's own paths, and the
// action's inputs, which this command sets itself.
func fleetBaseEnv(environ []string) map[string]string {
	env := map[string]string{}

	for _, kv := range environ {
		name, value, ok := strings.Cut(kv, "=")
		if !ok || strings.HasPrefix(name, "GITHUB_") || strings.HasPrefix(name, "RUNNER_") ||
			slices.Contains(fleetInputNames, name) {
			continue
		}

		env[name] = value
	}

	return env
}

// fleetInputNames are the action's input variables; an operator's shell
// exporting one must not decide a run this command configures.
var fleetInputNames = []string{
	"BILLET_MODE", "BILLET_REACH", "BILLET_INVENTORY", "BILLET_PLAYBOOK", "BILLET_KNOWN_HOSTS",
	"BILLET_EXTRA_VARS", "BILLET_LIMIT", "BILLET_PROVE_IDEMPOTENT", "BILLET_SSH_PRIVATE_KEY",
	"BILLET_GITHUB_APP_PRIVATE_KEY", "BILLET_ENVIRONMENT", "BILLET_ACTION_REF",
}

// applyRunnerFiles folds what a step appended to GITHUB_ENV and GITHUB_PATH
// into env, as a runner does before the next step. Both files are re-read
// whole, which is idempotent because a later line wins as it does in Actions.
func applyRunnerFiles(env map[string]string, envFile, pathFile, base string) error {
	body, err := os.ReadFile(envFile)
	if err != nil {
		return err
	}

	assigned, err := parseGitHubEnv(string(body))
	if err != nil {
		return err
	}

	for name, value := range assigned {
		env[name] = value
	}

	paths, err := os.ReadFile(pathFile)
	if err != nil {
		return err
	}

	// PREPENDED IN ORDER, each new line before the last, which is how the runner
	// builds PATH from GITHUB_PATH; the base PATH is this process's own.
	entries := []string{}

	for line := range strings.SplitSeq(string(paths), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			entries = append([]string{line}, entries...)
		}
	}

	env["PATH"] = strings.Join(append(entries, base), string(os.PathListSeparator))

	return nil
}

var githubEnvName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// parseGitHubEnv reads a GITHUB_ENV file: NAME=value lines and the
// NAME<<DELIMITER multi-line form.
func parseGitHubEnv(body string) (map[string]string, error) {
	out := map[string]string{}
	scanner := bufio.NewScanner(strings.NewReader(body))
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}

		if name, delimiter, ok := strings.Cut(line, "<<"); ok && githubEnvName.MatchString(name) {
			var lines []string

			closed := false

			for scanner.Scan() {
				if scanner.Text() == delimiter {
					closed = true

					break
				}

				lines = append(lines, scanner.Text())
			}

			if !closed {
				return nil, fmt.Errorf("GITHUB_ENV: %s's value never reaches its delimiter", name)
			}

			out[name] = strings.Join(lines, "\n")

			continue
		}

		name, value, ok := strings.Cut(line, "=")
		if !ok || !githubEnvName.MatchString(name) {
			return nil, fmt.Errorf("GITHUB_ENV: %q is not NAME=value", line)
		}

		out[name] = value
	}

	return out, scanner.Err()
}

// fleetDefaultHolder names an operator's converge by who and where, and when,
// so two converges from one machine never share a guard.
func fleetDefaultHolder() string {
	who := "operator"
	if u, err := user.Current(); err == nil && u.Username != "" {
		who = u.Username
	}

	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown-host"
	}

	return holderSafe("fleet-"+who+"-"+host) + "-" + strconv.FormatInt(time.Now().Unix(), 10)
}

var holderUnsafe = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// holderSafe keeps a name to the characters every guard parser admits.
func holderSafe(s string) string {
	return holderUnsafe.ReplaceAllString(s, "-")
}

func mergeEnv(base map[string]string, extra ...map[string]string) map[string]string {
	out := make(map[string]string, len(base))
	for k, v := range base {
		out[k] = v
	}

	for _, m := range extra {
		for k, v := range m {
			out[k] = v
		}
	}

	return out
}

func envList(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}

	slices.Sort(out)

	return out
}
