package ec2

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/scripts"
)

// Where the payload puts the build on the builder. build-ami.sh finds the tree at
// the paths it has in billet's repository, under amiRepoDir, and reads its
// settings from amiBuildEnv.
//
// UNDER /var/lib AND ROOT'S ALONE, NOT UNDER /opt. GitHub's configure-system.sh
// makes everything under /opt writable by everyone, on the builder that is its
// target, and root goes on running the build's scripts after it; restoring the
// modes afterwards would not restore what another account wrote in between.
const (
	amiBuildDir  = "/var/lib/billet-ami"
	amiRepoDir   = amiBuildDir + "/repo"
	amiBuildEnv  = amiBuildDir + "/build.env"
	amiLayerPath = amiBuildDir + "/ec2-layer.sh"
	amiUnitPath  = "/etc/systemd/system/billet-ami-build.service"
	amiUnitName  = "billet-ami-build.service"
)

// The build's verdict, framed on the console as the verifier's report is.
const (
	buildBegin = "billet-build-begin"
	buildEnd   = "billet-build-end"
)

// The keys of a build's verdict beside the verdict and the step.
const (
	buildTemplateStepKey = "template_step"
	buildComponentsKey   = "components"
)

// bootstrapStep is the part a failure of the user-data bootstrap names.
const bootstrapStep = "bootstrap"

// buildSteps are the parts of the build a verdict may name: the bootstrap's, and
// the labels build-ami.sh assigns to `at`, which a test holds to the script.
var buildSteps = []string{
	bootstrapStep, "start", "resume", "template", "components", "environment", "layer", "cleanup", "done",
}

// DefaultBuilderType is the builder's shape when nobody says: Nitro on both
// architectures, because billet reads the build's verdict off the live console,
// and the sixteen vCPUs GitHub's template was first run on here.
func DefaultBuilderType(arch string) string {
	if arch == "arm64" {
		return "c7g.4xlarge"
	}

	return "c7i.4xlarge"
}

// maxBuildPollFailures is how many polls in a row may fail before the wait gives
// up. Each poll's calls are already retried by the client; what this survives is
// the orchestrator's network going away for a few minutes in a build of hours, which
// would otherwise terminate a builder that was doing nothing wrong.
const maxBuildPollFailures = 5

// buildPoll is how often the build's state and console are read. The build takes
// hours and its success verdict dwells for two minutes, so this reads it several
// times before the builder powers off.
const buildPoll = 20 * time.Second

// componentVersion is what the completer reports a version as, and what may
// follow a component's name in the verdict and the tag.
var componentVersion = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z.+:~_-]{0,63}$`)

// scopeFor is GitHub's template's name for an AMI of arch, as the runner and
// differences.tsv spell it, and the plan that scope runs.
func scopeFor(arch string) (string, string, error) {
	switch arch {
	case "x64":
		return "ami-x64", "runner-images/plan.json", nil
	case "arm64":
		return "ami-arm64", "runner-images/plan-arm64.json", nil
	default:
		return "", "", fmt.Errorf("ec2: GitHub publishes no template for architecture %q", arch)
	}
}

// checkAccept is the accepted components, sorted and without duplicates, or an
// error naming one this billet does not install.
//
// REFUSED BEFORE ANYTHING IS BOUGHT, and never dropped: a name billet does not
// know is a deployment that believes it accepted something the image will not
// carry.
func checkAccept(accept []string) ([]string, error) {
	out := make([]string, 0, len(accept))

	for _, name := range accept {
		if !config.IsImageComponent(name) {
			return nil, fmt.Errorf("ec2: %q is not an image component this billet installs", name)
		}

		if !slices.Contains(out, name) {
			out = append(out, name)
		}
	}

	slices.Sort(out)

	return out, nil
}

// buildEnvFile is build.env, which build-ami.sh sources: the nonce, the scope and
// the accepted components, each checked here before it is written into shell.
func buildEnvFile(spec BuildSpec) (string, error) {
	if !verifyNonce.MatchString(spec.nonce) {
		return "", fmt.Errorf("ec2: %q is not a build nonce", spec.nonce)
	}

	scope, _, err := scopeFor(spec.Arch)
	if err != nil {
		return "", err
	}

	accept, err := checkAccept(spec.Accept)
	if err != nil {
		return "", err
	}

	return "BILLET_AMI_NONCE=" + spec.nonce + "\n" +
		"BILLET_AMI_SCOPE=" + scope + "\n" +
		"BILLET_AMI_ACCEPT='" + strings.Join(accept, " ") + "'\n", nil
}

// bootstrapScript is the builder's user data: fetch the payload, check it, unpack
// it and start the build unit.
//
// THE UNIT IS STARTED, NOT ENABLED. cloud-init runs user data on the first boot
// only, and the build is enabled for the one boot after its planned reboot by the
// step that reboots; any other boot runs nothing, so an unplanned restart ends at
// billet's deadline rather than in a build resumed from nowhere (ADR-022).
//
// NO `set -x`: the presigned URL is a credential, and a trace writes it to the
// console and to cloud-init's log.
func bootstrapScript(spec BuildSpec) (string, error) {
	if spec.payloadURL == "" || !runnerDigestPattern.MatchString(spec.payloadSHA256) {
		return "", fmt.Errorf("ec2: the build's payload must be staged before its bootstrap " +
			"is rendered")
	}

	// SINGLE-QUOTED, AND REFUSED IF IT COULD END THE QUOTE. The URL is billet's own
	// signature over a key billet chose, and is checked anyway: it reaches a root
	// shell.
	if strings.ContainsAny(spec.payloadURL, "'\n\\") {
		return "", fmt.Errorf("ec2: the payload URL is not one this build will quote")
	}

	if !verifyNonce.MatchString(spec.nonce) {
		return "", fmt.Errorf("ec2: %q is not a build nonce", spec.nonce)
	}

	var b strings.Builder

	b.WriteString("#!/bin/sh\nset -eu\n")
	b.WriteString("export HOME=/root\n")

	// A BOOTSTRAP THAT FAILS SAYS SO, with this build's nonce, in the verdict
	// build-ami.sh would have printed, so a payload that did not arrive ends the
	// wait at once rather than at billet's deadline. It never powers off.
	b.WriteString("billet_fail() {\n")
	b.WriteString("  while :; do\n")
	b.WriteString("    printf '%s\\n' '" + buildBegin + " " + spec.nonce + "' '" + reportVerdictKey +
		"=" + verdictFail + "' '" + reportStepKey + "=" + bootstrapStep + "' '" +
		buildTemplateStepKey + "=' '" + buildComponentsKey + "=' '" + buildEnd + " " +
		spec.nonce + "' >/dev/console 2>/dev/null || true\n")
	b.WriteString("    sleep 15\n")
	b.WriteString("  done\n")
	b.WriteString("}\n")
	b.WriteString("trap 'billet_status=$?; trap - EXIT; [ \"$billet_status\" -eq 0 ] || billet_fail' EXIT\n")

	// ONE BUILD PER MACHINE. A finished build cleans cloud-init before it announces
	// its success, so a reboot during the announcement runs this again with the
	// same nonce, and after the stop billet cannot see what it said: a stopped
	// instance's console reads empty (measured 2026-10-26, us-west-2, with and
	// without Latest, for a minute after the stop). So a machine whose image
	// records a finished build fails here, at once and on the console billet is
	// still reading, and a base image that is already a billet AMI is refused the
	// same way.
	b.WriteString("if [ -e " + componentsFile + " ]; then\n")
	b.WriteString("  echo 'this machine already records a finished billet build, and a build " +
		"runs once' >&2\n")
	b.WriteString("  exit 1\n")
	b.WriteString("fi\n")

	// THE DISK IS THERE BEFORE ANYTHING TRIES TO USE IT.
	//
	// billet asks for a larger root than the base image declares, and growing an
	// EBS volume does not grow the filesystem on it. On Ubuntu cloud images
	// cloud-init's growpart and resizefs modules do that in the `init` stage,
	// before user data runs in `final` -- but that is an ordering somebody else
	// owns, and this script cannot see whether it happened except by looking.
	//
	// df -P FOR THE POSIX FORMAT, because the default wraps a long device name
	// onto its own line and `NR==2` would then read the wrong record. A value that
	// is not a number fails rather than passes: `[ "$x" -lt N ]` exits 2 on
	// garbage, and `set -e` is suppressed for an `if` condition.
	b.WriteString("billet_free_kib=$(df -Pk / | awk 'NR == 2 { print $4 }')\n")
	b.WriteString("case \"${billet_free_kib:-}\" in\n")
	b.WriteString("  ''|*[!0-9]*)\n")
	b.WriteString("    echo \"df reported '${billet_free_kib:-}' free on the builder root, " +
		"which is not a number; billet cannot tell whether this image fits\" >&2\n")
	b.WriteString("    exit 1 ;;\n")
	b.WriteString("esac\n")
	b.WriteString("if [ \"$billet_free_kib\" -lt " +
		strconv.Itoa(minBuilderFreeGiB*1024*1024) + " ]; then\n")
	b.WriteString("  echo \"the builder root has ${billet_free_kib}KiB free and this image " +
		"needs " + strconv.Itoa(minBuilderFreeGiB) + "GiB; the volume was requested larger, so " +
		"either cloud-init did not grow the filesystem onto it (growpart, resizefs) or " +
		"--builder-disk is too small\" >&2\n")
	b.WriteString("  exit 1\n")
	b.WriteString("fi\n")

	// THE DIGEST IS CHECKED BEFORE ANYTHING IS EXTRACTED. A presigned URL is a
	// credential rather than a statement about content: it says who may read the
	// object, not that the object is the one billet uploaded.
	b.WriteString("curl -fsSL --retry 5 --retry-all-errors -o " + payloadPath +
		" '" + spec.payloadURL + "'\n")
	b.WriteString("echo '" + spec.payloadSHA256 + "  " + payloadPath +
		"' | sha256sum -c - >/dev/null\n")

	// EXTRACTED FROM /, because the archive carries the absolute paths the build
	// names. tar refuses a member that escapes the destination, and an existing
	// directory keeps its own mode rather than taking one from the archive.
	b.WriteString("tar --no-overwrite-dir -xzf " + payloadPath + " -C /\n")
	b.WriteString("rm -f " + payloadPath + "\n")

	// THE BUILD'S SETTINGS NAME THIS BUILD, or the unit is never started: build-ami.sh
	// can announce a failure only once it has read the nonce from them, so a
	// payload whose settings do not is refused here, where this bootstrap's own
	// verdict says so.
	b.WriteString("grep -qx 'BILLET_AMI_NONCE=" + spec.nonce + "' " + amiBuildEnv + "\n")
	b.WriteString("systemctl daemon-reload\n")
	b.WriteString("systemctl start --no-block " + amiUnitName + "\n")

	return b.String(), nil
}

// newBuildSchema is what billet will read out of a build's verdict.
func newBuildSchema() reportSchema {
	s := newReportSchema(buildBegin, buildEnd)

	for _, key := range []string{reportVerdictKey, reportStepKey, buildTemplateStepKey,
		buildComponentsKey} {
		s.keys[key] = struct{}{}
	}

	for _, step := range buildSteps {
		s.steps[step] = struct{}{}
	}

	return s
}

// awaitBuild waits for the builder's verdict and for the builder to power itself
// off, and returns the components the build reported installing.
//
// THE VERDICT FIRST, THEN THE STOP. A stopped builder is imaged only when billet
// has read this build's success verdict on its console while it ran: a stop on
// its own is a cost scheduler, a host failure or a base image's own policy as
// readily as a finished build. A failure verdict ends the wait at once and names
// where the build failed, from billet's own plan rather than the builder's words.
func (p *Provider) awaitBuild(ctx context.Context, id string, spec BuildSpec) (string, error) {
	schema := newBuildSchema()

	var (
		verdict  map[string]string
		latest   = true
		failures int
	)

	for {
		// A POLL FAILS IF EITHER OF ITS READS DOES, and only a poll whose reads all
		// answered resets the count.
		var pollErr error

		st, err := p.describeRaw(ctx, id)
		if err != nil {
			pollErr, st = err, builderState{}
		}

		switch st.state {
		case "stopped":
			// STOPPED BY WHOM. Exact equality, including rejecting empty: AWS marks
			// the reason code optional, so "" is not "the guest did it".
			if st.reason != "Client.InstanceInitiatedShutdown" {
				return "", fmt.Errorf("ec2: the builder %s stopped for reason %q rather than "+
					"Client.InstanceInitiatedShutdown, so this is not its build finishing and "+
					"whatever is on its disk is not a finished image", id, st.reason)
			}

			// THE CONSOLE IS NOT READ AGAIN HERE: a stopped instance's reads empty, so
			// a later failure is kept from happening rather than looked for (the
			// bootstrap's ONE BUILD PER MACHINE).
			if verdict == nil {
				return "", fmt.Errorf("ec2: the builder %s powered itself off without billet "+
					"having read its success verdict on the console, so billet cannot tell "+
					"whether the build finished and does not image it", id)
			}

			return verdict[buildComponentsKey], nil

		case "terminated", "shutting-down":
			return "", fmt.Errorf("ec2: the builder %s was terminated before it finished, so no "+
				"image was made", id)

		case "running":
			out, err := p.consoleOutput(ctx, id, latest)

			switch {
			case err == nil:
				report, found := parseReport(out, spec.nonce, schema)
				if found {
					// A FAILURE IS A FAILURE WHENEVER IT IS SAID, a success already
					// read included: a reboot during the success's dwell runs the
					// bootstrap again, and what it says next is about this builder.
					if report[reportVerdictKey] != verdictOK {
						return "", buildFailure(id, spec.Arch, report)
					}

					// THE SUCCESS IS READ ONCE. The dwell repeats it until the builder
					// powers off, and a repeat a kernel line broke is not a reason to
					// terminate a build that finished.
					if verdict != nil {
						break
					}

					// A SUCCESS IS WHOLE OR IT IS NOT ONE. The parser drops a line it
					// will not render, so a components line that was too long or not
					// printable arrives as no line, and "none" is not what it said.
					components, ok := report[buildComponentsKey]
					if !ok || report[reportStepKey] != "done" {
						return "", fmt.Errorf("ec2: the builder %s announced success without "+
							"finishing its report, so billet cannot tell what it built", id)
					}

					if err := checkBuildComponents(components, spec.Accept); err != nil {
						return "", fmt.Errorf("ec2: the builder %s: %w", id, err)
					}

					// EXACTLY WHAT WAS OWED, read from billet's own plan rather than
					// taken on the build's word: an accepted component the template
					// offers and the build left out is a deployment short of what it
					// accepted.
					if got := componentNames(components); !slices.Equal(got, spec.owed) {
						return "", fmt.Errorf("ec2: the builder %s reported the components %q and "+
							"this build owes %q", id, got, spec.owed)
					}

					verdict = report
				}

			case latest && refusedLatestConsole(err):
				return "", fmt.Errorf("ec2: the builder %s's shape does not serve the live serial "+
					"console, which billet reads the build's verdict from; build on a Nitro "+
					"shape: %w", id, err)

			case isInstanceNotFound(err):

			default:
				pollErr = err
			}
		}

		if pollErr == nil {
			failures = 0
		} else {
			failures++
			if failures >= maxBuildPollFailures {
				return "", pollErr
			}

			p.log.Warn("could not ask how the build is doing; asking again",
				"instance", id, "error", pollErr)
		}

		// WHAT THE WAIT HAD PROVED WHEN IT ENDED, and nothing more: whether the
		// builder is still running is not known here, and BuildImage terminates it.
		if err := p.sleep(ctx, buildPoll); err != nil {
			if verdict != nil {
				return "", fmt.Errorf("ec2: the builder %s announced its build's success and billet "+
					"had not seen it power itself off when the wait ended, so no image was made; "+
					"billet terminates it: %w", id, err)
			}

			// A LONGER --timeout ANSWERS A DEADLINE, not a cancel.
			advice := "rerun the build and read"
			if errors.Is(err, context.DeadlineExceeded) {
				advice = "rerun the build with a longer --timeout and read"
			}

			return "", fmt.Errorf("ec2: billet had read no verdict from the builder %s when the wait "+
				"ended; billet terminates it, so %s its console or its journal (journalctl -u %s) "+
				"while it runs: %w", id, advice, amiUnitName, err)
		}
	}
}

// buildFailure is the error a failure verdict becomes: the part of the build that
// failed, and for a template step its name from billet's own plan.
func buildFailure(id, arch string, report map[string]string) error {
	where := report[reportStepKey]

	if n := report[buildTemplateStepKey]; n != "" {
		if name, ok := planStepName(arch, n); ok {
			where += ", at step " + n + " of GitHub's template (" + name + ")"
		}
	}

	// THE BOOTSTRAP IS NO STEP OF THE UNIT, which never started: what it said is in
	// cloud-init's output, and what it refuses is a short list.
	if where == bootstrapStep {
		return fmt.Errorf("ec2: the builder %s reported that its build failed in %s, before "+
			"the build started: its root had too little free space, the payload could not be "+
			"fetched or was not the one staged, its settings named another build, or the "+
			"machine already records a finished billet build; billet terminates it, so rerun "+
			"the build to see which in cloud-init's output (/var/log/cloud-init-output.log)",
			id, where)
	}

	return fmt.Errorf("ec2: the builder %s reported that its build failed in %s; billet "+
		"terminates it, so rerun the build to see the step's own output in its journal "+
		"(journalctl -u %s)", id, where, amiUnitName)
}

// componentInstallers is GitHub's template step that installs each component,
// the one the completer runs; a test holds it to check-guest-component.sh's own.
var componentInstallers = map[string]string{
	"android-sdk":    "install-android-sdk.sh",
	"codeql":         "install-codeql-bundle.sh",
	"firefox":        "install-firefox.sh",
	"google-chrome":  "install-google-chrome.sh",
	"microsoft-edge": "install-microsoft-edge.sh",
}

// offeredComponents is the accepted components arch's template installs, sorted:
// what a build of arch owes the deployment.
func offeredComponents(arch string, accept []string) ([]string, error) {
	ids, err := planStepIDs(arch)
	if err != nil {
		return nil, err
	}

	out := []string{}

	for _, name := range accept {
		installer, ok := componentInstallers[name]
		if !ok {
			return nil, fmt.Errorf("ec2: billet knows no installer for the component %s", name)
		}

		if slices.Contains(ids, installer) {
			out = append(out, name)
		}
	}

	slices.Sort(out)

	return out, nil
}

// componentNames is the names in a components list, sorted.
func componentNames(value string) []string {
	out := []string{}

	if value == "" {
		return out
	}

	for _, item := range strings.Split(value, ",") {
		name, _, _ := strings.Cut(item, ":")
		out = append(out, name)
	}

	slices.Sort(out)

	return out
}

// planStepIDs is every step id of arch's plan, in order.
func planStepIDs(arch string) ([]string, error) {
	_, plan, err := scopeFor(arch)
	if err != nil {
		return nil, err
	}

	files, err := scripts.AMIBuild()
	if err != nil {
		return nil, fmt.Errorf("ec2: read the build's scripts: %w", err)
	}

	for _, f := range files {
		if f.Path != plan {
			continue
		}

		var steps []struct {
			ID string `json:"id"`
		}

		if err := json.Unmarshal(f.Body, &steps); err != nil {
			return nil, fmt.Errorf("ec2: read %s: %w", plan, err)
		}

		ids := make([]string, 0, len(steps))
		for _, s := range steps {
			ids = append(ids, s.ID)
		}

		return ids, nil
	}

	return nil, fmt.Errorf("ec2: the build carries no %s", plan)
}

// planStepName is the id of step n (counted from 1) of arch's plan.
func planStepName(arch, n string) (string, bool) {
	i, err := strconv.Atoi(n)
	if err != nil || i < 1 {
		return "", false
	}

	ids, err := planStepIDs(arch)
	if err != nil || i > len(ids) {
		return "", false
	}

	return ids[i-1], true
}

// checkBuildComponents is a reported components list, `name:version` joined by
// commas, held to what was accepted: each name accepted and at most once, each
// version one the completer could have read.
//
// A SUBSET, NOT THE SET. An accepted component this architecture's template does
// not install is left out by the build and said in its journal; one billet did
// not accept is a build that installed something the deployment never agreed to.
//
// THE ERRORS NAME NOTHING THE MACHINE WROTE, only a name billet already holds.
func checkBuildComponents(value string, accept []string) error {
	if value == "" {
		return nil
	}

	var seen []string

	for _, item := range strings.Split(value, ",") {
		name, version, ok := strings.Cut(item, ":")
		if !ok || !componentVersion.MatchString(version) {
			return errors.New("the reported components are not a list of name:version")
		}

		if !slices.Contains(accept, name) {
			return errors.New("the reported components name one that was not accepted")
		}

		if slices.Contains(seen, name) {
			return fmt.Errorf("the reported components name %s twice", name)
		}

		seen = append(seen, name)
	}

	return nil
}
