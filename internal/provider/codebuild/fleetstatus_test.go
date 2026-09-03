package codebuild

import (
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/config"
)

// A FLEET PENDING DELETION SERVES BUILDS, AND BILLET USED TO REFUSE IT WHILE PRINTING
// AWS'S OWN SENTENCE SAYING IT DOES NOT.
//
// MEASURED, and it is why this test exists rather than a reading of the API reference:
// `DeleteFleet` moves a reserved fleet to PENDING_DELETION with the status note "Fleets
// are deleted after all instances have run for 24 hours. Fleets are available to build
// projects while they are pending deletion." — and a real macOS Xcode job then ran to
// green on a fleet in exactly that state. `billet check` called it fatal and quoted
// that note in the same line, so the diagnostic contradicted itself and refused a
// deployment that works. That is the failure ADR-005 names: the next thing anybody does
// is delete the check.
//
// IT IS STILL A WARNING, because the state is temporary. An operator whose fleet is
// pending deletion has working capacity and a deadline, and needs to be told the second
// half.
func TestAFleetPendingDeletionIsAWarningRatherThanARefusal(t *testing.T) {
	cfg := testConfig()
	cfg.EnvironmentType = config.CodeBuildMacARM

	const note = "Fleets are deleted after all instances have run for 24 hours. " +
		"Fleets are available to build projects while they are pending deletion."

	r := FleetReport{
		Name:            "billet-accept-mac",
		Status:          "PENDING_DELETION",
		StatusNote:      note,
		EnvironmentType: string(config.CodeBuildMacARM),
	}

	fatal, warnings := r.Problems(cfg)

	// NO FATAL AT ALL, rather than "no fatal with this wording". Matching on the
	// sentence lets a differently phrased refusal through while the test still claims
	// the fleet is accepted.
	if len(fatal) != 0 {
		t.Errorf("a fleet pending deletion is refused, but AWS says it serves builds and "+
			"a real job has run on one: %q", fatal)
	}

	// AND IT IS NOT SILENT. Dropping the status check altogether would also pass the
	// assertion above, which is the mutation this half exists to kill.
	var said bool

	for _, w := range warnings {
		if strings.Contains(w, "PENDING_DELETION") && strings.Contains(w, "capacity disappears") {
			said = true
		}
	}

	if !said {
		t.Errorf("nothing warns that the capacity is scheduled to go away:\nfatal=%q\nwarnings=%q",
			fatal, warnings)
	}
}

// AND A FLEET THAT GENUINELY CANNOT SERVE IS STILL REFUSED.
//
// The direction that matters in the other sense: relaxing PENDING_DELETION must not
// relax the states that really do mean no capacity, or the check stops being one.
func TestAFleetThatCannotServeIsStillRefused(t *testing.T) {
	cfg := testConfig()
	cfg.EnvironmentType = config.CodeBuildMacARM

	for _, status := range []string{"CREATE_FAILED", "UPDATE_ROLLBACK_FAILED", "DELETING"} {
		r := FleetReport{
			Name:            "billet-accept-mac",
			Status:          status,
			StatusNote:      "no",
			EnvironmentType: string(config.CodeBuildMacARM),
		}

		fatal, _ := r.Problems(cfg)

		var refused bool

		for _, f := range fatal {
			if strings.Contains(f, "cannot serve builds") {
				refused = true
			}
		}

		if !refused {
			t.Errorf("fleet status %s is accepted; a tier on it would advertise capacity "+
				"and fail every job", status)
		}
	}
}

// AN ACTIVE FLEET CAN HAVE NO MACHINE BEHIND IT, AND THE STATUS CODE DOES NOT SAY SO.
//
// MEASURED 2026-09-02: a fresh MAC_ARM fleet reported ACTIVE nineteen seconds after
// CreateFleet and then sat with `status.context = INSUFFICIENT_CAPACITY` for the best
// part of an hour while every build dispatched to it queued until the queued ceiling
// failed it — and `billet check` printed "capacity 1" and no warning, because it read
// the status code alone. A warning rather than a refusal: the fleet is correctly
// configured and the state is AWS's and temporary.
func TestAFleetAWSCannotFindCapacityForIsWarnedAbout(t *testing.T) {
	cfg := testConfig()
	cfg.EnvironmentType = config.CodeBuildMacARM

	r := FleetReport{
		Name:            "billet-accept-mac3",
		Status:          "ACTIVE",
		StatusContext:   "INSUFFICIENT_CAPACITY",
		StatusNote:      "We currently do not have sufficient capacity for the instance type you requested. Please try again later.",
		EnvironmentType: string(config.CodeBuildMacARM),
	}

	fatal, warnings := r.Problems(cfg)

	if len(fatal) != 0 {
		t.Errorf("a correctly configured fleet AWS is short of capacity for was refused: %q", fatal)
	}

	var said bool

	for _, w := range warnings {
		if strings.Contains(w, "INSUFFICIENT_CAPACITY") && strings.Contains(w, "queues until") {
			said = true
		}
	}

	if !said {
		t.Errorf("nothing warns that the fleet has no instance behind it:\nwarnings=%q", warnings)
	}

	// AND AN ACTIVE FLEET WITH NO CONTEXT SAYS NOTHING OF THE KIND, or the warning
	// fires on every healthy fleet and is learned to be ignored.
	r.StatusContext, r.StatusNote = "", ""

	_, warnings = r.Problems(cfg)
	for _, w := range warnings {
		if strings.Contains(w, "queues until") {
			t.Errorf("a healthy fleet was warned about capacity it has: %q", w)
		}
	}
}

// A CONTEXT THAT MERELY REPEATS THE STATUS CODE IS NOT A CAPACITY PROBLEM.
//
// MEASURED 2026-09-02: a fleet pending deletion reports `status.context =
// PENDING_DELETION` beside `statusCode = PENDING_DELETION`, and the capacity warning
// keyed on "any context at all" printed "is PENDING_DELETION but AWS reports
// PENDING_DELETION … until an instance is provisioned every build queues" about a
// fleet whose warm Mac had run a job eleven seconds after dispatch. The deletion
// warning beside it is the true one and stays.
func TestAContextThatRepeatsTheStatusCodeIsNotACapacityWarning(t *testing.T) {
	cfg := testConfig()
	cfg.EnvironmentType = config.CodeBuildMacARM

	r := FleetReport{
		Name:            "billet-accept-mac3",
		Status:          "PENDING_DELETION",
		StatusContext:   "PENDING_DELETION",
		StatusNote:      "Fleets are deleted after all instances have run for 24 hours. Fleets are available to build projects while they are pending deletion.",
		EnvironmentType: string(config.CodeBuildMacARM),
		BaseCapacity:    1,
	}

	_, warnings := r.Problems(cfg)

	var deletion bool

	for _, w := range warnings {
		if strings.Contains(w, "queues until") {
			t.Errorf("a fleet whose context only repeats its status was warned about capacity: %q", w)
		}

		if strings.Contains(w, "capacity disappears") {
			deletion = true
		}
	}

	if !deletion {
		t.Errorf("the deletion warning went with it: %q", warnings)
	}
}

// A DECLARED macOS LIMIT ABOVE THE FLEET'S CAPACITY IS REFUSED.
//
// MEASURED 2026-09-02, docs/aws-acceptance.md: base capacity 1, macos_vm_limit 2,
// two concurrent jobs through one runs-on. billet did what the config asked —
// escrowed both, started two builds — and the second sat QUEUED behind the busy
// Mac while GitHub withdrew and requeued its assignment. MAC_ARM offers no
// on-demand overflow (`Fleet on-demand overflow behavior is not supported for
// MAC_ARM`), so nothing on the AWS side absorbs the excess: it is capacity
// advertised to GitHub that no Mac exists to run.
func TestADeclaredMacOSLimitAboveTheFleetIsRefused(t *testing.T) {
	r := FleetReport{Name: "billet-accept-mac3", BaseCapacity: 1}

	fatal, warnings := r.ConcurrencyProblems(2)

	if len(warnings) != 0 {
		t.Errorf("a limit the fleet cannot serve was merely warned about: %q", warnings)
	}

	// THE DIAGNOSTIC, NOT THE SHAPE: it has to name the declared number, the
	// fleet's, and the field to change.
	var said bool

	for _, f := range fatal {
		if strings.Contains(f, "macos_vm_limit 2") && strings.Contains(f, "runs 1 build(s)") &&
			strings.Contains(f, "set nodes[].macos_vm_limit to 1") {
			said = true
		}
	}

	if !said {
		t.Errorf("the refusal does not name the two numbers and the field: %q", fatal)
	}

	// A LIMIT THE FLEET CAN SERVE SAYS NOTHING, in either direction, or the
	// refusal fires on every correct deployment and is learned to be ignored.
	for _, declared := range []int{1, 0, -1} {
		fatal, warnings := r.ConcurrencyProblems(declared)
		if len(fatal) != 0 || len(warnings) != 0 {
			t.Errorf("declared %d against capacity 1 produced fatal=%q warnings=%q",
				declared, fatal, warnings)
		}
	}

	// AND A FLEET BILLET COULD NOT READ A CAPACITY FOR SAYS SO rather than passing:
	// CreateFleet refuses a base capacity below one, so a zero is a description
	// that omitted the field, and "could not tell" is not "fine".
	fatal, warnings = (FleetReport{Name: "x"}).ConcurrencyProblems(2)
	if len(fatal) != 0 {
		t.Errorf("an unknown fleet capacity was refused against zero: %q", fatal)
	}

	var unchecked bool

	for _, w := range warnings {
		if strings.Contains(w, "no base capacity") && strings.Contains(w, "macos_vm_limit 2") {
			unchecked = true
		}
	}

	if !unchecked {
		t.Errorf("an unknown fleet capacity passed silently: warnings=%q", warnings)
	}
}

// A FLEET THAT MAY SCALE PAST ITS BASE IS A WARNING UP TO ITS MAXIMUM, AND A
// REFUSAL PAST IT. The maximum is what AWS may grow to rather than what the fleet
// holds, so every build past the base still queues while it scales — which the
// warning says — but an operator who measured that scaling is not refused. The
// warning is its OWN sentence: a first version appended "the fleet may scale to 3,
// so this is accepted" to a message that had just said "set macos_vm_limit to 1".
func TestADeclaredMacOSLimitInsideAScalingFleetsMaximumIsAWarning(t *testing.T) {
	r := FleetReport{Name: "macs", BaseCapacity: 1, MaxCapacity: 3}

	fatal, warnings := r.ConcurrencyProblems(3)
	if len(fatal) != 0 {
		t.Errorf("a limit inside the scaling maximum was refused: %q", fatal)
	}

	var said bool

	for _, w := range warnings {
		if strings.Contains(w, "may scale to 3") && strings.Contains(w, "holds 1 build(s)") &&
			strings.Contains(w, "queues while it scales") {
			said = true
		}

		if strings.Contains(w, "set nodes[].macos_vm_limit") {
			t.Errorf("the scaling warning tells the operator to lower a limit it just accepted: %q", w)
		}
	}

	if !said {
		t.Errorf("nothing warns that builds past the base capacity queue while the fleet scales: %q", warnings)
	}

	// PAST THE MAXIMUM IS REFUSED, AND THE REMEDY NAMES BOTH ACCEPTED VALUES: a
	// refusal that said "set it to 1" contradicted the branch above, which accepts
	// 3.
	fatal, _ = r.ConcurrencyProblems(4)

	var both bool

	for _, f := range fatal {
		if strings.Contains(f, "set nodes[].macos_vm_limit to 1, or to at most 3") {
			both = true
		}
	}

	if !both {
		t.Errorf("a limit past the scaling maximum was not refused naming both the base and the maximum: %q", fatal)
	}
}

// AND THE SCALING MAXIMUM REACHES THE REPORT FROM THE WIRE, or the warning branch
// above judges a field nothing ever fills and every scaling fleet is refused.
func TestDescribeFleetCarriesTheScalingMaximum(t *testing.T) {
	f := newFakeAWS(t)
	f.fleetMaxCapacity = 3

	p := newTestProvider(t, f, func(c *config.CodeBuildConfig) {
		c.EnvironmentType = config.CodeBuildMacARM
		c.PrivilegedMode = false
		c.FleetARN = "arn:aws:codebuild:us-west-2:123456789012:fleet/macs:00000000-0000-0000-0000-000000000000"
	})

	report, _, err := p.DescribeFleet(t.Context())
	if err != nil {
		t.Fatalf("DescribeFleet: %v", err)
	}

	if report.BaseCapacity != 2 || report.MaxCapacity != 3 {
		t.Errorf("base %d max %d, want base 2 max 3", report.BaseCapacity, report.MaxCapacity)
	}
}

// AND THE CONTEXT REACHES THE REPORT FROM THE WIRE. A Problems test proves the rule;
// this proves DescribeFleet decodes the field it judges, which a test on the report
// alone would leave uncovered — the first version of the report had no such field.
func TestDescribeFleetCarriesTheStatusContext(t *testing.T) {
	f := newFakeAWS(t)
	f.fleetContext = "INSUFFICIENT_CAPACITY"

	p := newTestProvider(t, f, func(c *config.CodeBuildConfig) {
		c.EnvironmentType = config.CodeBuildMacARM
		c.PrivilegedMode = false // a Mac is the machine; there is no container to privilege
		c.FleetARN = "arn:aws:codebuild:us-west-2:123456789012:fleet/macs:00000000-0000-0000-0000-000000000000"
	})

	report, has, err := p.DescribeFleet(t.Context())
	if err != nil {
		t.Fatalf("DescribeFleet: %v", err)
	}

	if !has {
		t.Fatal("a configured fleet was reported absent")
	}

	if report.StatusContext != "INSUFFICIENT_CAPACITY" {
		t.Errorf("status context = %q, want INSUFFICIENT_CAPACITY", report.StatusContext)
	}

	if !strings.Contains(report.StatusNote, "sufficient capacity") {
		t.Errorf("the status note did not travel with the context: %q", report.StatusNote)
	}
}
