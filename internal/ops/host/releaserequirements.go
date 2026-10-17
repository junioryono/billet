package host

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/junioryono/billet/deploy"
	"github.com/junioryono/billet/internal/cli"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/lifeops"
)

// `billet release requirements` says what this build needs of the node unit
// that will run it: the runtime directories it writes into (#168). A host
// upgrade asks the staged candidate before anything stops and compares the
// answer with the unit systemd has loaded, because a rollout replaces the
// binary and never the units, and a binary that needs a directory its unit does
// not make keeps running without the evidence the next converge waits for.

// requirementsSchema is the answer's schema number.
const requirementsSchema = 1

// unitRequirements is the answer.
type unitRequirements struct {
	Schema int `json:"schema"`
	// RuntimeDirectories are entries the node unit must declare, relative to
	// /run as systemd reads them.
	RuntimeDirectories []string `json:"runtime_directories"`
}

// releaseRequirements answers for the configuration at --config: a host with
// no node section requires nothing of a node unit.
func releaseRequirements(env cli.Env, args []string) error {
	flags := cli.NewFlagSet("billet release requirements", env.Stdout)
	configPath := cli.AddConfigFlag(flags)
	asJSON := flags.Bool("json", false, "print the answer as JSON (the only form)")

	if err := cli.Parse(flags, args); err != nil {
		return err
	}

	if !*asJSON {
		return errors.New("release requirements answers as JSON; pass --json")
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}

	answer := unitRequirements{Schema: requirementsSchema, RuntimeDirectories: []string{}}

	if cfg.Node != nil {
		answer.RuntimeDirectories = slices.Clone(deploy.NodeRuntimeDirectories)
	}

	return answerObject(env, answer)
}

// missingRuntimeDirectories is what the loaded node unit lacks of what the
// candidate requires, read from systemd's rendering of RuntimeDirectory (its
// entries separated by spaces); none is a unit that meets every requirement.
func missingRuntimeDirectories(req unitRequirements, runtime string) []string {
	declared := strings.Fields(runtime)

	var missing []string

	for _, entry := range req.RuntimeDirectories {
		if !slices.Contains(declared, entry) {
			missing = append(missing, "RuntimeDirectory="+entry)
		}
	}

	return missing
}

// candidateVintage says what a candidate binary positively is from its answer
// to the converge guard's dry run, as processVintageOf does for the managed
// binary: an unknown command is a release before the guard, any completed
// answer one that carries it, anything else could-not-tell.
func candidateVintage(ctx context.Context, staged string) (processVintage, string) {
	stdout, stderr, rc, err := runBounded(ctx, staged, "converge-guard", "prepare", "--dry-run", "--json")

	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return vintageUnknown, "the candidate did not answer the preparation's dry run within its bound"
	case errors.Is(err, errOutputOverflow):
		return vintageUnknown, "the candidate's answer to the preparation's dry run overflowed its bound"
	case rc < 0:
		return vintageUnknown, "the candidate could not be run: " + err.Error()
	}

	if rc != 0 && unknownCommandPattern.Match(append(append([]byte{}, stdout...), stderr...)) {
		return vintagePreR, ""
	}

	return vintageR, ""
}

// checkUnitRequirements asks the staged candidate what it needs of the node
// unit and refuses a loaded unit that does not provide it, naming the
// preparation. A candidate that predates the question says so positively (its
// usage error names the command it does not know) and requires nothing; any
// other failure to answer is an error, which ends the transaction before
// anything stopped. The string is what was found, for the transaction's report.
func checkUnitRequirements(ctx context.Context, staged, configPath string, insp *lifeops.Inspector) (string, error) {
	stdout, stderr, rc, err := runBounded(ctx, staged, "release", "requirements", "--config", configPath, "--json")

	// ONLY A COMPLETED RUN IS JUDGED: a run the bound ended, one whose output
	// overflowed, or one that could not be run has no answer.
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "", errors.New("the candidate did not say what it requires of the node unit within its bound")
	case errors.Is(err, errOutputOverflow):
		return "", errors.New("the candidate's answer to what it requires of the node unit overflowed its bound")
	case rc < 0:
		return "", fmt.Errorf("the candidate could not be asked what it requires of the node unit: %w", err)
	}

	var req unitRequirements

	if rc != 0 {
		both := append(append([]byte{}, stdout...), stderr...)
		if !bytes.Contains(both, []byte(`unknown release command "requirements"`)) && !unknownCommandPattern.Match(both) {
			return "", fmt.Errorf("the candidate refused to say what it requires of the node unit (exit %d): %s", rc,
				strings.TrimSpace(string(both)))
		}

		// A CANDIDATE FROM BEFORE THE QUESTION is not therefore one that needs
		// nothing: every release that publishes its registration record and
		// predates this command needs the record's directory too (#168). Such
		// a release carries the converge guard, so the guard's dry run is what
		// tells the two apart.
		vintage, why := candidateVintage(ctx, staged)

		switch vintage {
		case vintagePreR:
			return "the candidate is a release before the converge guard and requires nothing of a node unit", nil
		case vintageR:
			req = unitRequirements{Schema: requirementsSchema, RuntimeDirectories: slices.Clone(deploy.NodeRuntimeDirectories)}
		default:
			return "", fmt.Errorf("the candidate predates the unit requirements, and whether it publishes a "+
				"registration record cannot be told: %s", why)
		}
	} else {
		dec := json.NewDecoder(bytes.NewReader(stdout))
		dec.DisallowUnknownFields()

		if err := dec.Decode(&req); err != nil || req.Schema != requirementsSchema || req.RuntimeDirectories == nil {
			return "", fmt.Errorf("the candidate's answer to what it requires of the node unit is not one this build "+
				"reads (schema %d): %v", req.Schema, err)
		}
	}

	if len(req.RuntimeDirectories) == 0 {
		return "the candidate requires nothing of a node unit here", nil
	}

	props, err := insp.UnitProperties(ctx, nodeUnit, "LoadState", "RuntimeDirectory")
	if err != nil {
		return "", fmt.Errorf("ask systemd about %s: %w", nodeUnit, err)
	}

	// AN UNANSWERED PROPERTY IS NOT AN EMPTY ONE: only an answer says what the
	// unit declares.
	for _, name := range []string{"LoadState", "RuntimeDirectory"} {
		if values, ok := props[name]; !ok || len(values) != 1 {
			return "", fmt.Errorf("systemd answered no %s for %s, so whether it meets the candidate's requirements "+
				"cannot be told", name, nodeUnit)
		}
	}

	switch load := firstProp(props, "LoadState"); load {
	case "loaded":
	case "not-found", "masked":
		return fmt.Sprintf("%s is %s, so no node runs here to hold to the candidate's requirements", nodeUnit, load), nil
	default:
		return "", fmt.Errorf("%s is LoadState=%s, so whether it meets the candidate's requirements cannot be told",
			nodeUnit, load)
	}

	if missing := missingRuntimeDirectories(req, firstProp(props, "RuntimeDirectory")); len(missing) > 0 {
		return "", fmt.Errorf("the candidate needs %s to provide %s, and the loaded unit does not; a rollout installs "+
			"no units, so install the ones this release ships first (converge the host role, or install the package), "+
			"and the rollout takes this host when it next asks", nodeUnit, strings.Join(missing, " and "))
	}

	return "the loaded node unit provides everything the candidate requires", nil
}
