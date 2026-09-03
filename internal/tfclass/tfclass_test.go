package tfclass_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/tfclass"
)

// tablePath is the committed table the module also renders as an output.
const tablePath = "../../terraform/modules/billet/classification.json"

// moduleRoot is the tree every managed resource has to be accounted for in.
//
// THE DEPLOYMENT MODULE AND ITS CHILDREN, AND DELIBERATELY NOT ITS SIBLINGS.
// terraform/modules also holds converge-aws-ssm and converge-cloudflare, which
// say in their own first comment that they are optional and NOT part of the
// billet root module: they carry an operator's remote-access path to a host, run
// no jobs, hold no ledger, and are applied on their own. Classifying them would
// be claiming this table describes what a change to somebody's SSM activation
// costs a running deployment, which it does not.
//
// A plan for one of those pointed at tfclassify is REFUSED rather than passed —
// Classify errors on a resource it has never heard of — which is the right way
// round: the gate says it was not built for that plan instead of reporting it
// clean.
const moduleRoot = "../../terraform/modules/billet"

func load(t *testing.T) tfclass.Table {
	t.Helper()

	table, err := tfclass.Load(tablePath)
	if err != nil {
		t.Fatalf("load the classification table: %v", err)
	}

	return table
}

// EVERY MANAGED RESOURCE IN EVERY MODULE IS ACCOUNTED FOR.
//
// THIS IS THE VACUITY GUARD, and without it the whole gate is decoration: a
// resource nobody classified is one `Classify` refuses on — which is the safe
// direction at apply time and a terrible one at review time, because the first
// person to meet it is an operator mid-change rather than whoever added the
// resource. It is the same failure `tf-modules-check` refuses one level up,
// where a module directory with no discovered .tf would make every terraform
// gate pass without examining it.
//
// The .tf files are read directly rather than through terraform, because this
// has to hold on a machine with no provider plugins and no network — which is
// most machines, and all of CI's Go job.
func TestEveryManagedResourceIsClassified(t *testing.T) {
	t.Parallel()

	table := load(t)
	declared := declaredResources(t)

	if len(declared) == 0 {
		t.Fatal("no resources were discovered under " + moduleRoot + "; this test would pass " +
			"against an empty table and an empty module tree alike")
	}

	for key, where := range declared {
		if _, ok := table[key]; !ok {
			t.Errorf("%s (declared in %s) has no entry in classification.json; a plan that "+
				"touches it cannot be read", key, where)
		}
	}
}

// AND NOTHING IS CLASSIFIED THAT DOES NOT EXIST.
//
// The other direction, and it is not symmetry for its own sake: a stale entry is
// how the table goes on describing a resource somebody deleted, which reads as
// coverage. It also catches a typo in a key, which would otherwise show up only
// as the missing-entry failure above, naming the resource rather than the typo.
func TestTheClassificationTableDescribesNothingImaginary(t *testing.T) {
	t.Parallel()

	table := load(t)
	declared := declaredResources(t)

	for key := range table {
		if _, ok := declared[key]; !ok {
			t.Errorf("classification.json describes %s, which no module declares", key)
		}
	}
}

// A PLAN THAT STOPS A HOST IS REPORTED AS ONE, and the assertion is on the
// CLASS rather than on "something was found".
func TestReplacingTheControllerIsADrainingChange(t *testing.T) {
	t.Parallel()

	findings, err := tfclass.Classify(load(t), "", []byte(`{"format_version":"1.2","resource_changes":[
		{"address":"module.control_plane.aws_instance.control_plane","mode":"managed",
		 "type":"aws_instance","name":"control_plane",
		 "change":{"actions":["create","delete"]}}]}`))
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}

	if len(findings) != 1 {
		t.Fatalf("classified %d changes, want 1: %+v", len(findings), findings)
	}

	if findings[0].Class != tfclass.Draining {
		t.Errorf("replacing the controller is %s, want %s", findings[0].Class, tfclass.Draining)
	}

	if len(tfclass.Blocking(findings)) != 1 {
		t.Error("replacing the controller does not block, so an apply would stop the process " +
			"holding the ledger while it was dispatching work")
	}
}

// A TAG CHANGE IS NOT, which is the half that keeps the gate usable. A
// classifier that blocked everything would be turned off within a week.
func TestATagChangeBlocksNothing(t *testing.T) {
	t.Parallel()

	findings, err := tfclass.Classify(load(t), "", []byte(`{"format_version":"1.2","resource_changes":[
		{"address":"module.control_plane.aws_cloudwatch_metric_alarm.control_plane_recover",
		 "mode":"managed","type":"aws_cloudwatch_metric_alarm","name":"control_plane_recover",
		 "change":{"actions":["update"]}}]}`))
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}

	if len(findings) != 1 || findings[0].Class != tfclass.InPlace {
		t.Fatalf("an alarm update classified as %+v, want a single in_place finding", findings)
	}

	if len(tfclass.Blocking(findings)) != 0 {
		t.Error("an in-place change blocked the plan")
	}
}

// THE PLAN'S OWN ACTIONS CAN ONLY MAKE A CLASS WORSE, NEVER BETTER.
//
// The table is a claim about the worst ORDINARY change to a resource, and
// somebody destroying it is not bound by that claim. Without this fold, deleting
// the alarm — classified in_place, correctly, because changing it disturbs
// nothing — would be reported as needing no attention at all.
func TestDestroyingAnInPlaceResourceIsStillDestructive(t *testing.T) {
	t.Parallel()

	findings, err := tfclass.Classify(load(t), "", []byte(`{"format_version":"1.2","resource_changes":[
		{"address":"module.control_plane.aws_cloudwatch_metric_alarm.control_plane_recover",
		 "mode":"managed","type":"aws_cloudwatch_metric_alarm","name":"control_plane_recover",
		 "change":{"actions":["delete"]}}]}`))
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}

	if len(findings) != 1 || findings[0].Class != tfclass.Destructive {
		t.Fatalf("destroying an in_place resource classified as %+v, want destructive", findings)
	}
}

// A FIRST APPLY IS NOT A DRAIN, WHICH IS THE GATE'S OWN HAPPY PATH.
//
// Every entry describes changing, replacing or destroying a resource that
// EXISTS, because that is the only way one can disturb something running. A
// plan that creates the controller for the first time has no controller to stop
// — and a gate that refused it would be refused by everyone standing the module
// up, which is the failure ADR-005 names: the next thing anybody does is delete
// the check.
func TestAFirstApplyNeedsNoDrain(t *testing.T) {
	t.Parallel()

	findings, err := tfclass.Classify(load(t), "", []byte(`{"format_version":"1.2","resource_changes":[
		{"address":"module.control_plane.aws_instance.control_plane","mode":"managed",
		 "type":"aws_instance","name":"control_plane","change":{"actions":["create"]}},
		{"address":"module.control_plane.aws_ebs_volume.ledger","mode":"managed",
		 "type":"aws_ebs_volume","name":"ledger","change":{"actions":["create"]}}]}`))
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}

	if len(findings) != 2 {
		t.Fatalf("classified %d changes, want 2", len(findings))
	}

	for _, f := range findings {
		if f.Class != tfclass.InPlace {
			t.Errorf("creating %s for the first time is %s, want %s — there is nothing there "+
				"yet to disturb", f.Address, f.Class, tfclass.InPlace)
		}
	}

	if len(tfclass.Blocking(findings)) != 0 {
		t.Error("a first apply was blocked, so nobody could ever stand this module up")
	}
}

// AND A REPLACEMENT IS NOT EXEMPTED BY THE CREATE IN IT.
//
// The rule above is `only creates`, not `contains a create`. A replace is
// ["create","delete"] — the case that actually stops a running host — so a
// containment check would exempt every replacement in the module, which is the
// entire class this gate exists for.
func TestAReplacementIsNotExemptedByItsCreate(t *testing.T) {
	t.Parallel()

	findings, err := tfclass.Classify(load(t), "", []byte(`{"format_version":"1.2","resource_changes":[
		{"address":"module.control_plane.aws_instance.control_plane","mode":"managed",
		 "type":"aws_instance","name":"control_plane",
		 "change":{"actions":["create","delete"]}}]}`))
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}

	if len(findings) != 1 || findings[0].Class != tfclass.Draining {
		t.Fatalf("replacing the controller classified as %+v, want draining", findings)
	}
}

// AND A RESOURCE THE TABLE HAS NEVER HEARD OF IS AN ERROR, NOT A PASS.
//
// This is the rule the vacuity guard above exists to keep from ever firing in
// production, and it has to be the behaviour anyway: defaulting an unknown
// resource to in_place makes the one resource nobody classified the one a plan
// says nothing about.
func TestAnUnclassifiedResourceIsRefused(t *testing.T) {
	t.Parallel()

	_, err := tfclass.Classify(load(t), "", []byte(`{"format_version":"1.2","resource_changes":[
		{"address":"aws_efs_file_system.somebodys_idea","mode":"managed",
		 "type":"aws_efs_file_system","name":"somebodys_idea",
		 "change":{"actions":["create"]}}]}`))
	if err == nil {
		t.Fatal("a resource with no classification was classified anyway")
	}

	if !strings.Contains(err.Error(), "aws_efs_file_system.somebodys_idea") {
		t.Errorf("the refusal does not name the resource an operator has to add: %v", err)
	}
}

// NO-OPS AND DATA READS ARE NOT CHANGES.
//
// A plan carries a no-op for every unchanged resource and a read for every data
// source, so without this a report would restate the entire configuration and
// the lines that matter would be unfindable in it.
func TestUnchangedResourcesAndDataReadsAreNotReported(t *testing.T) {
	t.Parallel()

	findings, err := tfclass.Classify(load(t), "", []byte(`{"format_version":"1.2","resource_changes":[
		{"address":"module.control_plane.aws_instance.control_plane","mode":"managed",
		 "type":"aws_instance","name":"control_plane","change":{"actions":["no-op"]}},
		{"address":"data.aws_ami.ubuntu","mode":"data","type":"aws_ami","name":"ubuntu",
		 "change":{"actions":["read"]}}]}`))
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}

	if len(findings) != 0 {
		t.Fatalf("an unchanged resource and a data read were reported as changes: %+v", findings)
	}
}

// WORST FIRST, because the first line is the one an operator acts on.
func TestFindingsAreOrderedWorstFirst(t *testing.T) {
	t.Parallel()

	findings, err := tfclass.Classify(load(t), "", []byte(`{"format_version":"1.2","resource_changes":[
		{"address":"a.aws_cloudwatch_metric_alarm.control_plane_recover","mode":"managed",
		 "type":"aws_cloudwatch_metric_alarm","name":"control_plane_recover",
		 "change":{"actions":["update"]}},
		{"address":"b.aws_ebs_volume.ledger","mode":"managed",
		 "type":"aws_ebs_volume","name":"ledger","change":{"actions":["delete"]}},
		{"address":"c.aws_instance.control_plane","mode":"managed",
		 "type":"aws_instance","name":"control_plane","change":{"actions":["create","delete"]}}]}`))
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}

	want := []tfclass.Class{tfclass.Destructive, tfclass.Draining, tfclass.InPlace}
	if len(findings) != len(want) {
		t.Fatalf("classified %d changes, want %d", len(findings), len(want))
	}

	for i, class := range want {
		if findings[i].Class != class {
			t.Errorf("finding %d is %s, want %s: %v", i, findings[i].Class, class, findings)
		}
	}
}

// A DOCUMENT THAT IS NOT A PLAN IS REFUSED, NOT REPORTED AS CLEAN.
//
// THIS IS THE FINDING THAT MATTERS MOST IN THIS FILE. `terraform show -json`
// renders STATE as well as a plan, and state has no `resource_changes` at all —
// so it decodes into the same struct with zero changes and reads exactly like a
// plan that does nothing. So does `{}`, and so does the output of a `show`
// somebody pointed at the wrong file. Every one of those would have exited 0
// with "this plan changes nothing billet has to be drained for", which is a gate
// answering about a document it never understood.
func TestSomethingThatIsNotAPlanIsRefused(t *testing.T) {
	t.Parallel()

	for name, body := range map[string]string{
		"an empty object":       `{}`,
		"no format version":     `{"resource_changes":[]}`,
		"terraform state":       `{"format_version":"1.0","values":{"root_module":{}}}`,
		"an unsupported major":  `{"format_version":"2.0","resource_changes":[]}`,
		"a plan that errored":   `{"format_version":"1.2","resource_changes":[],"errored":true}`,
		"not json at all":       `no`,
		"a null resource array": `{"format_version":"1.2","resource_changes":null}`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if _, err := tfclass.Classify(load(t), "", []byte(body)); err == nil {
				t.Fatalf("%s was accepted as a plan and would have been reported as needing "+
					"no drain", name)
			}
		})
	}
}

// ...AND AN ORDINARY NO-OP PLAN STILL PASSES, which is the other direction and
// the one that keeps the refusal above from being a refusal of everything.
//
// An EMPTY resource_changes is a real plan that does nothing. An ABSENT one is
// a state file. Collapsing the two is what the refusal above exists to prevent,
// so a test that only checked the refusal would pass against a build that
// rejected every plan.
func TestAPlanThatChangesNothingIsAccepted(t *testing.T) {
	t.Parallel()

	findings, err := tfclass.Classify(load(t), "",
		[]byte(`{"format_version":"1.2","resource_changes":[]}`))
	if err != nil {
		t.Fatalf("an ordinary no-op plan was refused: %v", err)
	}

	if len(findings) != 0 {
		t.Fatalf("a no-op plan produced %d findings", len(findings))
	}
}

// AN ACTION NAME THIS BUILD DOES NOT KNOW IS REFUSED.
//
// What a change COSTS is derived from these names, so one that is not recognised
// contributes nothing and the finding falls back to whatever the table alone
// said. A future Terraform spelling a destroy differently would be reported as
// an in-place change — the exact direction that loses data.
func TestAnUnknownActionIsRefused(t *testing.T) {
	t.Parallel()

	_, err := tfclass.Classify(load(t), "", []byte(`{"format_version":"1.2","resource_changes":[
		{"address":"aws_ebs_volume.ledger","mode":"managed","type":"aws_ebs_volume",
		 "name":"ledger","change":{"actions":["forget"]}}]}`))
	if err == nil {
		t.Fatal("an action name this build does not understand was priced anyway")
	}

	if !strings.Contains(err.Error(), "forget") {
		t.Errorf("the refusal does not name the action: %v", err)
	}
}

// AND A MODE IT DOES NOT KNOW IS REFUSED, rather than skipped as though it could
// not matter.
func TestAnUnknownModeIsRefused(t *testing.T) {
	t.Parallel()

	_, err := tfclass.Classify(load(t), "", []byte(`{"format_version":"1.2","resource_changes":[
		{"address":"something.new","mode":"ephemeral","type":"something","name":"new",
		 "change":{"actions":["create","delete"]}}]}`))
	if err == nil {
		t.Fatal("a resource mode this build does not understand was skipped silently")
	}
}

// A CONSUMER'S OWN RESOURCE DOES NOT INHERIT BILLET'S CLASSIFICATION.
//
// The table is keyed by type.name, and those names are not unique in somebody
// else's root: `aws_iam_role.node` means "the identity a billet node runs as"
// here and could mean anything at all in a consumer's configuration. Without a
// scope, a plan for a root that merely CONTAINS billet would hand billet's
// classification to an unrelated resource that happens to share a generic name.
//
// Reported rather than dropped, because a report that silently omitted them
// would leave a reader believing it had covered the whole plan.
func TestAResourceOutsideTheBilletModuleIsNotClassified(t *testing.T) {
	t.Parallel()

	plan := []byte(`{"format_version":"1.2","resource_changes":[
		{"address":"module.billet.module.fleet.aws_iam_role.node","module_address":"module.billet.module.fleet",
		 "mode":"managed","type":"aws_iam_role","name":"node","change":{"actions":["create","delete"]}},
		{"address":"module.their_app.aws_iam_role.node","module_address":"module.their_app",
		 "mode":"managed","type":"aws_iam_role","name":"node","change":{"actions":["create","delete"]}}]}`)

	findings, err := tfclass.Classify(load(t), "module.billet", plan)
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}

	if len(findings) != 2 {
		t.Fatalf("classified %d changes, want both reported: %+v", len(findings), findings)
	}

	var ours, theirs *tfclass.Finding

	for i := range findings {
		if findings[i].OutOfScope {
			theirs = &findings[i]
		} else {
			ours = &findings[i]
		}
	}

	if ours == nil || theirs == nil {
		// RETURNED EXPLICITLY. t.Fatalf ends the goroutine running the test, but
		// staticcheck reads the fall-through as the nil dereference it would be
		// anywhere else, and it is right about the shape.
		t.Fatalf("one of the two was not reported at all: %+v", findings)

		return
	}

	if ours.Class != tfclass.Draining {
		t.Errorf("billet's own node role is %s, want %s", ours.Class, tfclass.Draining)
	}

	if theirs.Address != "module.their_app.aws_iam_role.node" {
		t.Errorf("the out-of-scope finding is %s", theirs.Address)
	}

	// AND IT NEVER BLOCKS. billet has nothing to say about a resource in
	// somebody else's module, and blocking on one would be this gate asserting
	// authority over a configuration it has never seen.
	blocking := tfclass.Blocking(findings)
	if len(blocking) != 1 || blocking[0].Address != ours.Address {
		t.Errorf("Blocking() is %+v, want only billet's own change", blocking)
	}
}

// A TABLE THAT CANNOT BE READ IS AN ERROR RATHER THAN AN EMPTY ONE, because an
// empty table reports every plan as holding nothing worth draining for.
func TestAnEmptyTableIsRefused(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "classification.json")
	if err := os.WriteFile(path, []byte(`{"_comment": ["nothing here"]}`), 0o600); err != nil {
		t.Fatalf("write the fixture: %v", err)
	}

	if _, err := tfclass.Load(path); err == nil {
		t.Fatal("a table holding only its own prose loaded as a usable classification")
	}
}

// AN ENTRY WITH NO REASON IS REFUSED, because the class alone tells an operator
// what to do and never why — and "why" is what decides whether they believe it.
func TestAnEntryWithoutAReasonIsRefused(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "classification.json")
	body := `{"aws_instance.control_plane": {"class": "draining", "reason": "  "}}`

	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write the fixture: %v", err)
	}

	if _, err := tfclass.Load(path); err == nil {
		t.Fatal("an entry with no reason loaded")
	}
}

// AND A CLASS THIS BUILD DOES NOT UNDERSTAND IS REFUSED AT LOAD, not silently
// ranked below draining when a plan reaches it.
func TestAnUnknownClassIsRefused(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "classification.json")
	body := `{"aws_instance.control_plane": {"class": "probably_fine", "reason": "who knows"}}`

	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write the fixture: %v", err)
	}

	if _, err := tfclass.Load(path); err == nil {
		t.Fatal("a class this build does not understand loaded")
	}
}

// resourceRe matches a managed resource declaration at the start of a line,
// which is how every one in this tree is written and what `terraform fmt`
// guarantees.
var resourceRe = regexp.MustCompile(`(?m)^resource "([a-z0-9_]+)" "([a-z0-9_]+)"`)

// declaredResources walks the module tree and returns every type.name it
// declares, mapped to the file it was found in.
func declaredResources(t *testing.T) map[string]string {
	t.Helper()

	out := map[string]string{}

	err := filepath.WalkDir(moduleRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if d.IsDir() || filepath.Ext(path) != ".tf" {
			return nil
		}

		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}

		for _, m := range resourceRe.FindAllStringSubmatch(string(body), -1) {
			out[m[1]+"."+m[2]] = path
		}

		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", moduleRoot, err)
	}

	return out
}
