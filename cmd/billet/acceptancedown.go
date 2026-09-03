package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The teardown, which is the half of `billet acceptance` that has to work when
// everything else did not.
//
// IT COMPOSES WHAT ALREADY EXISTS rather than reimplementing any of it: the seal
// and the wait are `billet drain`'s, the scale sets are `billet teardown`'s, and
// the cloud resources are `billet decommission`'s. Each of those has its own
// refusals, its own scoping and its own tests, and a second implementation here
// would be a second set of them — with the destructive half being the copy.
//
// THE ORDER IS THE SAFETY CONTENT, and it is the same order `billet local down`
// uses. Seal first, so nothing new is admitted while the rest runs. Wait, so what
// is already running finishes rather than being destroyed — an acceptance run
// that killed its own job would report a failure it caused. Then GitHub, so no
// runner registration outlives the compute it named. Then the cloud, because an
// instance is what bills.
//
// AND IT IS RUN AGAIN AND AGAIN. `down` is idempotent by construction — every
// step it composes already is — so an operator who lost a run, or a CI job whose
// teardown step runs `if: always()` after a teardown that already happened, is
// the ordinary case rather than a special one.

const (
	// acceptanceDrainWait bounds the drain. An acceptance job is a test job and
	// the run that started it has already stopped waiting for it by the time this
	// is reached; what this bounds is how long the teardown watches before it
	// reports that something is still running and moves on to say so.
	//
	// IT DOES NOT DESTROY WHAT IT DID NOT WAIT OUT. Passing that boundary makes
	// this command report and exit non-zero, not escalate — `billet force-destroy`
	// is the thing that ends running work, and it is an operator's to run.
	acceptanceDrainWait = 20 * time.Minute
)

func cmdAcceptanceDown(ctx context.Context, args []string) error {
	fs := newFlagSet("billet acceptance down")
	workspace := fs.String("workspace", "", "the workspace `billet acceptance up` created")
	wait := fs.Duration("wait", acceptanceDrainWait,
		"how long to let a running job finish before reporting that one is still there")
	keepWorkspace := fs.Bool("keep-workspace", false,
		"leave the workspace directory in place (the state directory holds this run's "+
			"deployment identity, so a later `down` can still be scoped to it)")

	if err := parse(fs, args); err != nil {
		return err
	}

	ws, err := requireAcceptanceWorkspace(*workspace)
	if err != nil {
		return err
	}

	// INVOKED DIRECTLY, SO NOTHING HAS PROVED THE FLEET CLEAR. `run` takes that
	// proof while its control plane is still up; an operator running `down` on its
	// own has a stopped one, and the barrier cannot be answered by a control plane
	// that is not there.
	if err := tearDownAcceptanceWithin(ctx, ws, *wait, false); err != nil {
		return err
	}

	if *keepWorkspace {
		return nil
	}

	return removeAcceptanceWorkspace(ws)
}

// tearDownAcceptance is `run`'s teardown, told whether the compute barrier
// already proved the fleet clear while the services were up.
func tearDownAcceptance(ctx context.Context, ws acceptanceWorkspace, computeProved bool) error {
	return tearDownAcceptanceWithin(ctx, ws, acceptanceDrainWait, computeProved)
}

func tearDownAcceptanceWithin(
	ctx context.Context, ws acceptanceWorkspace, wait time.Duration, computeProved bool,
) error {
	fmt.Printf("\nTearing down deployment %s.\n", ws.DeploymentID)

	var problems []error

	// 1. SEAL AND WAIT. Every later step assumes nothing new is starting, and a
	// seal is what makes that true — but only for ADMISSION, so the wait is what
	// covers the work already in flight.
	if err := drainAcceptance(ctx, ws, wait); err != nil {
		problems = append(problems, err)
	}

	// 2. THE SCALE SETS, BEFORE THE COMPUTE. A runner registration that outlived
	// its instance is one GitHub will route a job to, and that job then queues for
	// twenty-four hours against a runner that does not exist. Deleting the scale
	// set first means nothing is routed to what is about to be destroyed.
	//
	// SCOPED BY THE DERIVED CONFIG, whose tier labels carry this run's prefix — so
	// `--all` is every scale set THIS RUN owns and none that anything else does.
	if err := runBilletSubcommand(ctx, cmdTeardown, "teardown",
		"--config", ws.ConfigPath, "--all", "--yes"); err != nil {
		problems = append(problems, err)
	}

	// 3. THE CLOUD RESOURCES, scoped by the deployment identity this workspace
	// minted — which is what makes it impossible for this to reach another
	// deployment's instances, whatever the config says.
	//
	// A CONFIG WITH NO CLOUD BACKEND IS NOT AN ERROR HERE. `decommission` refuses
	// a config with no ec2 node, correctly, because there is nothing for it to
	// remove — and an acceptance run against a docker or firecracker deployment is
	// an ordinary thing to do.
	if err := decommissionAcceptance(ctx, ws); err != nil {
		problems = append(problems, err)
	}

	// 4. AND THE SWEEP, which is the step that turns "the commands ran" into "there
	// is nothing left". Everything above can succeed while something survives —
	// a destroy that raced a launch, an instance whose lease had already gone —
	// so the teardown ends by ASKING rather than by assuming.
	if err := sweepAcceptance(ctx, ws, computeProved); err != nil {
		problems = append(problems, err)
	}

	if len(problems) > 0 {
		return fmt.Errorf("the acceptance teardown did not finish cleanly: %w",
			errors.Join(problems...))
	}

	fmt.Printf("Torn down: nothing carrying %s remains.\n", ws.DeploymentID)

	return nil
}

// drainAcceptance seals and waits, through the same code `billet drain --wait`
// runs.
func drainAcceptance(ctx context.Context, ws acceptanceWorkspace, wait time.Duration) error {
	db, cfg, err := openLedgerForAdmission(ctx, ws.ConfigPath)
	if err != nil {
		return fmt.Errorf("open this run's ledger: %w", err)
	}

	defer db.Close()

	current, err := db.Admission(ctx)
	if err != nil {
		return fmt.Errorf("read admission: %w", err)
	}

	sealed, err := takeTheSeal(ctx, db, current, "billet acceptance down")
	if err != nil {
		return fmt.Errorf("seal this run: %w", err)
	}

	// WITHOUT THE COMPUTE PROOF, and the reason is that the control plane has
	// already been stopped by the time `run` reaches here — so nothing is left to
	// observe a barrier request, and waiting for one would time out on every
	// clean run. The LEDGER barrier still holds, and the sweep below is what
	// covers the class the ledger cannot see. `evidence` records whatever the
	// barrier did manage to establish while the plane was up.
	return waitForQuiet(ctx, db, cfg, sealed.Generation, waitOptions{
		timeout:      wait,
		withoutProof: true,
	})
}

// decommissionAcceptance removes the cloud resources this run created, and
// tolerates a config that has none.
func decommissionAcceptance(ctx context.Context, ws acceptanceWorkspace) error {
	err := runBilletSubcommand(ctx, cmdDecommission, "decommission",
		"--config", ws.ConfigPath, "--yes")
	if err == nil {
		return nil
	}

	// A DEPLOYMENT WITH NO CLOUD BACKEND HAS NOTHING TO DECOMMISSION, and saying
	// so is not a failure of the teardown. Matched on the refusal `decommission`
	// itself writes rather than on a type, because it has none — which is worth
	// naming as the weakness it is: a reworded refusal would turn this into a
	// teardown that reports a problem it does not have. It is the safe direction.
	if isNoCloudBackend(err) {
		fmt.Printf("decommission: this config has no cloud backend, so there is nothing " +
			"outside Terraform for it to remove\n")

		return nil
	}

	return err
}

func isNoCloudBackend(err error) bool {
	if err == nil {
		return false
	}

	msg := err.Error()

	// BOTH FRAGMENTS, so a different failure that happens to quote one of them —
	// a wrapped error naming the command, say — is not read as "nothing to do"
	// and silently swallowed. A teardown that swallowed a real failure is worse
	// than one that reports a problem it does not have.
	return strings.Contains(msg, "decommission tears down the ec2 backend") &&
		strings.Contains(msg, "there is nothing for it to remove")
}

// errAcceptanceResidue is what a sweep that FOUND something reports, as opposed
// to one that could not look.
//
// TWO ANSWERS, AND A CALLER ACTS ON THEM DIFFERENTLY. "This run left an instance
// running" is a bill and a fleet to clean up by hand; "billet could not ask AWS"
// is a run that establishes nothing either way — and reporting the second as the
// first sends an operator hunting for compute that may not exist, while reporting
// it as success is the failure this whole command is built around.
var errAcceptanceResidue = errors.New("this acceptance run left resources behind")

// errAcceptanceUnswept is the THIRD answer: billet did not look.
//
// SEPARATE FROM BOTH, and it is the one an earlier version of this collapsed. The
// provider sweep goes through `billet decommission`, which knows only the ec2
// backend — so on a docker, firecracker, tart or codebuild deployment it refuses
// without asking anything, and reading that refusal as "nothing is there" would
// print a clean sweep about a host whose node crashed after launching a
// container. It is non-zero for the same reason a failed read is: a sweep that
// did not look establishes nothing.
var errAcceptanceUnswept = errors.New("the provider was not swept")

// cmdAcceptanceSweep is the last question, on its own, so a caller can ask it
// without destroying anything.
//
// A SUBCOMMAND RATHER THAN A grep IN A WORKFLOW. The first version of the CI job
// matched `billet decommission`'s prose for a phrase meaning "there is nothing
// here" — which makes a reworded diagnostic silently turn a red job green, and
// puts the one decision that has to be right in a place no test can reach. The
// answer is decided here, in Go, and the workflow reads an exit status.
func cmdAcceptanceSweep(ctx context.Context, args []string) error {
	fs := newFlagSet("billet acceptance sweep")
	workspace := fs.String("workspace", "", "the workspace `billet acceptance up` created")

	if err := parse(fs, args); err != nil {
		return err
	}

	ws, err := requireAcceptanceWorkspace(*workspace)
	if err != nil {
		return err
	}

	// A STANDALONE SWEEP HAS NO BARRIER BEHIND IT. `run` proves the fleet clear
	// while the control plane is still up and hands that fact to the teardown;
	// invoked on its own, this has only the provider inventory — which for a
	// backend `decommission` does not know is nothing at all, and says so.
	return sweepAcceptance(ctx, ws, false)
}

// sweepAcceptance is the final ASK: does anything still carry this deployment.
//
// THE LEDGER IS NOT THE INSTRUMENT. Everything above may have succeeded while an
// instance survives — a destroy that raced a launch, or compute whose lease had
// already gone, which is the class the ledger has never been able to see. So this
// asks the provider, through the same inventory `decommission` uses, and FAILS if
// it finds anything.
func sweepAcceptance(ctx context.Context, ws acceptanceWorkspace, computeProved bool) error {
	// THROUGH `decommission` WITHOUT --yes, which reports what it FINDS and
	// deletes nothing. Running the report as the last step means the answer comes
	// from the same code that would have removed it, rather than from a second
	// implementation that could disagree about what belongs to this deployment.
	err := runBilletSubcommand(ctx, cmdDecommission, "decommission", "--config", ws.ConfigPath)

	switch {
	case isNoCloudBackend(err) && computeProved:
		// THE BARRIER ALREADY ASKED, AND IT ASKS EVERY BACKEND. `decommission`
		// knows only ec2, but alloc.ComputeClear puts the question to each HOST
		// through its own provider — which is the stronger instrument, and the one
		// that can see compute whose lease has already gone. A run that got a
		// clearance while its control plane was up has been swept by something
		// better than this.
		fmt.Printf("sweep: the compute barrier proved every host clear while this run's "+
			"control plane was up; nothing carrying %s remains\n", ws.DeploymentID)

		return nil

	case isNoCloudBackend(err):
		// THE PROVIDER WAS NEVER ASKED, AND THAT IS NOT "NOTHING IS THERE".
		//
		// `decommission` only knows the ec2 backend, so on a docker, firecracker,
		// tart or codebuild deployment it refuses without looking — and an earlier
		// version of this read that refusal as a clean sweep. It is the exact
		// could-not-tell/no collapse billet removes everywhere else, and here it
		// would print "nothing remains" about a host whose node crashed after
		// launching a container.
		//
		// REPORTED AS UNSWEPT AND NON-ZERO. The ledger barrier in `down` still
		// ran, so this is not "anything could be out there" — it is billet saying
		// the only instrument that can see compute whose lease has gone was not
		// available for this backend.
		return fmt.Errorf("%w: the provider inventory sweep only covers the ec2 backend, and "+
			"this deployment does not use it — so nothing here establishes that %s left no "+
			"compute behind. `billet drain --wait` against a RUNNING control plane is the "+
			"instrument that would (its compute barrier asks each host); this teardown has "+
			"already stopped one. Check the host's provider by hand",
			errAcceptanceUnswept, ws.DeploymentID)

	case err == nil:
		fmt.Printf("sweep: nothing carrying %s remains\n", ws.DeploymentID)

		return nil

	case isLiveCompute(err):
		// SOMETHING IS STILL THERE. `decommission` refuses to proceed past live
		// instances rather than terminating them, which is right — a running
		// instance may be serving a job — and for a sweep that refusal IS the
		// answer.
		return fmt.Errorf("%w: %w", errAcceptanceResidue, err)

	default:
		return fmt.Errorf("the sweep could not complete, so nothing here establishes whether "+
			"this run left anything behind: %w", err)
	}
}

// isLiveCompute recognises `decommission`'s refusal to walk past running
// instances.
//
// MATCHED ON ITS SENTENCE, and that is a weakness worth naming rather than
// hiding: the command returns an unwrapped error, so there is no type to ask.
// What makes it survivable is which way it fails — a reworded refusal falls
// through to the default branch above and is reported as "could not establish",
// which is a red sweep either way. It is the safe direction, and the fix is a
// sentinel in decommission.go the day something else needs one.
func isLiveCompute(err error) bool {
	return err != nil && strings.Contains(err.Error(), "instance(s) are still live")
}

// runBilletSubcommand invokes another billet command in this process.
//
// IN-PROCESS RATHER THAN AS A CHILD, which is the opposite of what `run` does
// with the services, and for the opposite reason: what is under test there is a
// process an operator starts, and what is needed here is the exact refusals and
// scoping those commands already implement. Shelling out would add an argv
// quoting surface and a second way for the teardown to lose an error.
func runBilletSubcommand(
	ctx context.Context, fn func(context.Context, []string) error, name string, args ...string,
) error {
	fmt.Printf("\n$ billet %s %v\n", name, args)

	if err := fn(ctx, args); err != nil {
		return fmt.Errorf("billet %s: %w", name, err)
	}

	return nil
}

// removeAcceptanceWorkspace deletes the workspace once the teardown proved there
// is nothing left to scope to it.
//
// AFTER THE TEARDOWN AND NEVER BEFORE. The state directory holds the deployment
// identity every destroy is scoped by, so removing it first would leave any
// surviving compute owned by an identity nothing on the machine can name — which
// is the one state this whole command exists to avoid.
func removeAcceptanceWorkspace(ws acceptanceWorkspace) error {
	dir := filepath.Dir(ws.ConfigPath)

	// PROVED TO BE A WORKSPACE BEFORE IT IS REMOVED. `down` takes a path from an
	// operator and this deletes a directory tree; the record is what says the
	// directory is one billet made. requireAcceptanceWorkspace already read it,
	// and this re-reads rather than trusting that, because between the two the
	// path could have been replaced.
	current, found, err := readAcceptanceWorkspace(dir)
	if err != nil || !found {
		return fmt.Errorf("%s no longer holds an acceptance record, so it will not be "+
			"removed: it may not be the directory this run created", dir)
	}

	// AND BILLET MUST HAVE CREATED IT. `up` refuses to adopt a directory that
	// already held anything, so a workspace it merely found is one an operator
	// made — and removing it takes whatever else they put there afterwards. The
	// record is where that distinction is kept, because by now the directory
	// itself cannot say.
	if !current.Created {
		fmt.Printf("workspace %s was not created by billet, so it is left in place\n", dir)

		return nil
	}

	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("remove the workspace: %w", err)
	}

	fmt.Printf("workspace %s removed\n", dir)

	return nil
}
