// Package tfclass reads a Terraform plan and says which of its changes billet's
// deployment has to be drained for.
//
// TERRAFORM ALREADY CLASSIFIES ITS OWN CHANGES — create, update, replace,
// destroy — and that is not the question an operator has. ADR-004 keeps live
// billet nodes outside Terraform on purpose, so a plan cannot know that these
// hosts are running somebody's build, and "1 to change, 1 to destroy" is
// therefore the same sentence whether the change is a tag or the instance
// holding the ledger.
//
// The missing half is committed beside the module as classification.json, and
// this joins the two. It is deliberately a separate reader rather than logic in
// HCL: an output cannot see a plan, and a `check` block cannot fail one for a
// reason it was not given.
//
// WHAT IT REFUSES IS THE POINT. `prevent_destroy` on the ledger volume already
// fails a plan that would destroy it, which covers exactly one resource; this
// covers the rest, and covers the class Terraform has no vocabulary for at all —
// a change that is perfectly ordinary except that a machine has to stop for it.
package tfclass

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
)

// Class is what a change to a resource costs a running deployment.
//
// ORDERED, and the order is what lets a plan's own actions be folded in: a
// destroy of a resource this table calls in_place is still a destroy, so the
// effective class is the WORSE of the two. Erring high costs an operator a drain
// they did not need; erring low costs somebody's build.
type Class string

const (
	// InPlace means nothing running is disturbed.
	InPlace Class = "in_place"
	// Replacement means the resource is recreated. No running job is on it, but
	// there is a window in which it does not exist.
	Replacement Class = "replacement"
	// Draining means a host that may be running jobs stops, or loses something a
	// running job depends on. billet's durable drain comes first — and a
	// Terraform timeout is not permission to terminate a job.
	Draining Class = "draining"
	// Destructive means data does not come back.
	Destructive Class = "destructive"
)

// severity orders the classes. A Class outside the set answers -1, which
// `worse` treats as unknown rather than as the mildest — an unrecognised class
// must never silently rank below draining.
func severity(c Class) int {
	switch c {
	case InPlace:
		return 0
	case Replacement:
		return 1
	case Draining:
		return 2
	case Destructive:
		return 3
	}

	return -1
}

// Valid reports whether c is a class this build understands.
func (c Class) Valid() bool { return severity(c) >= 0 }

// NeedsDrain reports whether acting on this class without draining first can end
// somebody's job or lose data.
func (c Class) NeedsDrain() bool { return c == Draining || c == Destructive }

// worse returns the higher-severity of two classes, and refuses to guess about
// one it does not recognise.
func worse(a, b Class) (Class, error) {
	sa, sb := severity(a), severity(b)
	if sa < 0 {
		return "", fmt.Errorf("tfclass: %q is not a class this build understands (%v)", a, Classes())
	}

	if sb < 0 {
		return "", fmt.Errorf("tfclass: %q is not a class this build understands (%v)", b, Classes())
	}

	if sb > sa {
		return b, nil
	}

	return a, nil
}

// Classes lists every class, mildest first, for a diagnostic.
func Classes() []Class { return []Class{InPlace, Replacement, Draining, Destructive} }

// Entry is what the committed table says about one resource.
type Entry struct {
	Class Class `json:"class"`
	// Reason is why, in the operator's terms rather than Terraform's.
	Reason string `json:"reason"`
	// Remedy is the billet command that makes the change safe, where one exists.
	Remedy string `json:"remedy,omitempty"`
}

// Table maps a resource's type.name to what changing it costs.
//
// KEYED BY type.name RATHER THAN BY FULL ADDRESS, because the same resource
// means the same thing in whichever module declares it — the node role exists in
// both fleet-ec2 and fleet-codebuild and is the identity a running instance or
// build carries either way. A full address would also make every entry depend on
// what the root happens to call its children, so moving a module would silently
// empty the table.
type Table map[string]Entry

// commentKey is the table's own prose, which is documentation rather than a
// resource. It is skipped on load rather than being allowed to look like an
// entry for a resource called `_comment`.
const commentKey = "_comment"

// Load reads the committed table.
//
// IT VALIDATES EVERY ENTRY RATHER THAN THE ONES A PLAN HAPPENS TO TOUCH. A class
// nobody recognises, or an entry with no reason, is a table somebody edited
// without finishing — and finding that out only when a plan reaches that
// resource means finding out during the apply it was meant to gate.
func Load(path string) (Table, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("tfclass: read the classification table: %w", err)
	}

	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("tfclass: parse %s: %w", path, err)
	}

	out := make(Table, len(doc))

	for key, value := range doc {
		if key == commentKey {
			continue
		}

		var e Entry
		if err := json.Unmarshal(value, &e); err != nil {
			return nil, fmt.Errorf("tfclass: parse the entry for %s: %w", key, err)
		}

		if !e.Class.Valid() {
			return nil, fmt.Errorf("tfclass: %s is classified %q, which is not one of %v",
				key, e.Class, Classes())
		}

		if strings.TrimSpace(e.Reason) == "" {
			return nil, fmt.Errorf("tfclass: %s has no reason; the class alone tells an "+
				"operator what to do and never why", key)
		}

		out[key] = e
	}

	if len(out) == 0 {
		return nil, errors.New("tfclass: the classification table is empty, so every plan " +
			"would be reported as holding nothing worth draining for")
	}

	return out, nil
}

// Plan is the subset of `terraform show -json` this reads.
//
// DECLARED NARROWLY ON PURPOSE. The plan format is somebody else's and carries
// far more than this needs; decoding only these fields means a format that grows
// cannot change what this concludes.
//
// BUT NARROW DECODING IS NOT PERMISSION TO ACCEPT ANYTHING. `terraform show
// -json` renders STATE as well as a plan, and a state file — or `{}`, or a
// future format this build does not understand — decodes into this struct
// perfectly, with zero changes, and would be reported as a plan that needs no
// drain. A gate that answers "safe" about input it never understood is worse
// than no gate, so the three fields below exist to refuse rather than to be
// read.
type Plan struct {
	// FormatVersion is the plan format's own version. Terraform documents that a
	// consumer must reject an unsupported MAJOR.
	FormatVersion string `json:"format_version"`
	// ResourceChanges is a POINTER so absent and empty can be told apart. An
	// empty array is an ordinary no-op plan; an absent key means this is not a
	// plan at all, which is exactly what a state file looks like here.
	ResourceChanges *[]ResourceChange `json:"resource_changes"`
	// Errored says the plan itself failed. Terraform emits one, and classifying a
	// plan Terraform could not finish is answering a question nobody has.
	Errored bool `json:"errored"`
}

// supportedPlanFormatMajor is the plan-format major this build reads.
//
// PINNED TO A MAJOR RATHER THAN AN EXACT VERSION, because that is the
// compatibility promise Terraform makes: a minor adds fields, and this decodes
// only four of them. A major is a format that may have changed what an action
// name or a mode MEANS, and a gate that guessed at one would be interpreting a
// document by its old rules.
const supportedPlanFormatMajor = "1"

// ResourceChange is one planned change.
type ResourceChange struct {
	Address string `json:"address"`
	// ModuleAddress is empty for a resource in the root module, and
	// `module.<name>` (nested with dots) otherwise. It is what Scope compares.
	ModuleAddress string `json:"module_address"`
	Mode          string `json:"mode"`
	Type          string `json:"type"`
	Name          string `json:"name"`
	Change        struct {
		Actions []string `json:"actions"`
	} `json:"change"`
}

// Key is the classification key for this change.
func (r ResourceChange) Key() string { return r.Type + "." + r.Name }

// Scope bounds which of a plan's resources this table describes.
//
// THE TABLE IS KEYED BY type.name, AND THOSE NAMES ARE NOT UNIQUE IN SOMEBODY
// ELSE'S ROOT. `aws_iam_role.node` means "the identity a billet node runs as"
// here and could mean anything in a consumer's own configuration — so a plan for
// a root that merely CONTAINS billet would have an unrelated resource inherit
// billet's classification, and the generic names are exactly the ones a
// collision is likely on.
//
// An empty Scope means the plan IS the billet module — the documented invocation,
// where every managed resource is billet's and must be classified. A non-empty
// one names the module address billet was called at (`module.billet`), and
// anything outside it is reported as OUT OF SCOPE rather than silently skipped:
// a resource this table has nothing to say about is a fact the operator needs,
// not one to omit.
type Scope string

// covers reports whether a change belongs to the module this table describes.
func (s Scope) covers(moduleAddress string) bool {
	if s == "" {
		return true
	}

	return moduleAddress == string(s) || strings.HasPrefix(moduleAddress, string(s)+".")
}

// Finding is one classified change.
type Finding struct {
	Address string
	Actions []string
	Class   Class
	Entry   Entry
	// OutOfScope marks a change in a module this table does not describe. It is
	// REPORTED rather than dropped, and never blocks: billet has nothing to say
	// about somebody else's resource, and saying nothing at all about it would
	// leave a plan reader believing the report covered the whole plan.
	OutOfScope bool
}

// Classify reads a plan and returns everything it will actually do, worst first.
//
// NO-OPS AND READS ARE NOT CHANGES and are dropped, so a report names what an
// apply would do rather than restating the whole state. DATA SOURCES ARE DROPPED
// too: a `read` is not an action on infrastructure, and classifying one would
// require an entry for every lookup.
//
// AN UNCLASSIFIED RESOURCE IS AN ERROR, NEVER A DEFAULT. Treating it as in_place
// would make a resource somebody forgot to classify the one a plan says nothing
// about — a gate that passes precisely where it has never been taught to look.
func Classify(table Table, scope Scope, planJSON []byte) ([]Finding, error) {
	plan, err := decodePlan(planJSON)
	if err != nil {
		return nil, err
	}

	var findings []Finding

	for _, rc := range *plan.ResourceChanges {
		switch rc.Mode {
		case "managed":
		case "data":
			// A data source is READ, never acted on. Skipped rather than
			// classified, or the table would need an entry for every lookup.
			continue
		default:
			// A THIRD MODE IS A FORMAT THIS BUILD DOES NOT UNDERSTAND, and
			// skipping it would be deciding that whatever it is cannot matter.
			return nil, fmt.Errorf("tfclass: %s has mode %q, which this build does not "+
				"understand; it will not report on a plan it cannot read", rc.Address, rc.Mode)
		}

		if err := checkActions(rc.Address, rc.Change.Actions); err != nil {
			return nil, err
		}

		if !acts(rc.Change.Actions) {
			continue
		}

		// OUT OF SCOPE IS REPORTED, NOT SKIPPED, and it is decided BEFORE the
		// table is consulted — otherwise a consumer's own `aws_iam_role.node`
		// would be handed billet's classification of a name that means something
		// entirely different in their configuration.
		if !scope.covers(rc.ModuleAddress) {
			findings = append(findings, Finding{
				Address:    rc.Address,
				Actions:    rc.Change.Actions,
				Class:      InPlace,
				OutOfScope: true,
			})

			continue
		}

		entry, ok := table[rc.Key()]
		if !ok {
			return nil, fmt.Errorf("tfclass: the plan changes %s, which the classification "+
				"table does not describe. Add it to classification.json: a plan reader silent "+
				"about a resource it has never heard of is a gate that passes without "+
				"examining it", rc.Address)
		}

		class, err := effective(entry.Class, rc.Change.Actions)
		if err != nil {
			return nil, fmt.Errorf("tfclass: %s: %w", rc.Address, err)
		}

		findings = append(findings, Finding{
			Address: rc.Address,
			Actions: rc.Change.Actions,
			Class:   class,
			Entry:   entry,
		})
	}

	// Worst first, then by address, so a report is stable and the line an
	// operator has to act on is the first one.
	sort.SliceStable(findings, func(i, j int) bool {
		si, sj := severity(findings[i].Class), severity(findings[j].Class)
		if si != sj {
			return si > sj
		}

		return findings[i].Address < findings[j].Address
	})

	return findings, nil
}

// decodePlan reads a plan and refuses anything that is not one.
//
// EVERY REFUSAL HERE IS A DOCUMENT THAT WOULD OTHERWISE HAVE BEEN REPORTED AS
// NEEDING NO DRAIN. `terraform show -json` renders state as well as a plan, and
// a state file has no `resource_changes` at all — so it decodes into Plan with
// zero changes and reads exactly like a plan that does nothing. So does `{}`,
// and so does the output of a `show` somebody pointed at the wrong file.
func decodePlan(planJSON []byte) (Plan, error) {
	var plan Plan
	if err := json.Unmarshal(planJSON, &plan); err != nil {
		return Plan{}, fmt.Errorf("tfclass: parse the plan: %w", err)
	}

	if plan.FormatVersion == "" {
		return Plan{}, errors.New("tfclass: this document carries no format_version, so it " +
			"is not `terraform show -json` output at all")
	}

	if major, _, _ := strings.Cut(plan.FormatVersion, "."); major != supportedPlanFormatMajor {
		return Plan{}, fmt.Errorf("tfclass: plan format version %s is not one this build "+
			"reads (it reads %s.x). A new major may have changed what an action or a mode "+
			"MEANS, and reporting on it under the old rules would be a gate answering about "+
			"a document it misread", plan.FormatVersion, supportedPlanFormatMajor)
	}

	if plan.Errored {
		return Plan{}, errors.New("tfclass: this plan is marked errored, so it does not " +
			"describe what an apply would do; fix the plan and re-run `terraform show -json`")
	}

	// ABSENT IS NOT EMPTY. An empty array is an ordinary no-op plan and is fine;
	// an absent key is a state file, which is what `terraform show -json` prints
	// when it is given no plan file at all.
	if plan.ResourceChanges == nil {
		return Plan{}, errors.New("tfclass: this document has no resource_changes, so it is " +
			"terraform STATE rather than a plan. `terraform show -json <planfile>` — with the " +
			"file `terraform plan -out` wrote — is what produces one")
	}

	return plan, nil
}

// acts reports whether a change does anything, and refuses an action name this
// build has never heard of.
//
// `["no-op"]` is Terraform saying nothing happens, and a plan carries one for
// every unchanged resource. AN UNKNOWN ACTION IS AN ERROR rather than something
// to fall through as harmless: `impliedBy` decides what a plan costs from these
// names, so a name it does not recognise contributes nothing and the finding is
// downgraded to whatever the table alone said. A future Terraform that spelled a
// destroy differently would be reported as an in-place change.
func acts(actions []string) bool {
	if len(actions) == 0 {
		return false
	}

	for _, a := range actions {
		if a != "no-op" {
			return true
		}
	}

	return false
}

// knownActions is the complete set Terraform documents for a resource change.
var knownActions = []string{"no-op", "create", "read", "update", "delete"}

// checkActions refuses a combination this build cannot price.
func checkActions(address string, actions []string) error {
	for _, a := range actions {
		if !slices.Contains(knownActions, a) {
			return fmt.Errorf("tfclass: %s carries action %q, which this build does not "+
				"understand (it knows %v). What a change COSTS is derived from these names, "+
				"so an unrecognised one would be priced as though it did nothing",
				address, a, knownActions)
		}
	}

	return nil
}

// effective is what this particular change costs, given what the table says
// about the resource and what the plan says it is doing to it.
//
// A PURE CREATE IS ALWAYS in_place, WHATEVER THE TABLE SAYS, and this is not a
// softening — it is what the table means. Every entry describes changing,
// replacing or destroying a resource that EXISTS, because that is the only way a
// resource can disturb something running. A first apply is all creates: there is
// no controller to stop, no job on a security group that has never existed, and
// no data in a bucket nobody has written to. Without this, standing the module
// up for the first time would be refused as needing a drain of a deployment that
// is not there — a gate that fails on its own happy path is one nobody keeps.
//
// Everything else takes the WORSE of the two, so the table can never understate
// a plan: a destroy is destructive even where changing the resource is not.
func effective(declared Class, actions []string) (Class, error) {
	if !declared.Valid() {
		return "", fmt.Errorf("tfclass: %q is not a class this build understands (%v)",
			declared, Classes())
	}

	if onlyCreates(actions) {
		return InPlace, nil
	}

	return worse(declared, impliedBy(actions))
}

// onlyCreates reports whether the plan is bringing this resource into existence
// and doing nothing else to it.
//
// `["create"]` is the whole set that qualifies. A create PAIRED with a delete is
// a replacement, which is precisely the case that can stop a running host, so
// checking "contains create" rather than "is only create" would exempt every
// replacement in the module.
func onlyCreates(actions []string) bool {
	if len(actions) != 1 {
		return false
	}

	return actions[0] == "create"
}

// impliedBy is what the plan's own actions cost, independent of the table.
//
// A DESTROY IS DESTRUCTIVE WHATEVER THE TABLE SAYS, and a replace is at least a
// replacement. This is what stops the table from being able to UNDERSTATE a
// plan: an entry is a claim about the worst ordinary change to a resource, and
// somebody destroying it is not bound by that claim.
func impliedBy(actions []string) Class {
	var creates, deletes bool

	for _, a := range actions {
		switch a {
		case "delete":
			deletes = true
		case "create":
			creates = true
		}
	}

	switch {
	case deletes && !creates:
		// A destroy with no create after it removes the resource for good.
		return Destructive
	case deletes && creates:
		// A replace. The resource comes back, so this is not data loss on its
		// own — the table is what knows whether a host stops for it.
		return Replacement
	default:
		return InPlace
	}
}

// Blocking returns the findings that must not be applied without draining first.
//
// AN OUT-OF-SCOPE CHANGE NEVER BLOCKS. billet has nothing to say about a
// resource in somebody else's module, and blocking on one would be this gate
// asserting authority over a configuration it has never seen.
func Blocking(findings []Finding) []Finding {
	var out []Finding

	for _, f := range findings {
		if !f.OutOfScope && f.Class.NeedsDrain() {
			out = append(out, f)
		}
	}

	return out
}

// Report renders findings for a person.
func Report(findings []Finding) string {
	if len(findings) == 0 {
		return "This plan changes nothing billet has to be drained for.\n"
	}

	var b strings.Builder

	for _, f := range findings {
		if f.OutOfScope {
			// SAID, NOT OMITTED. A report that silently dropped these would leave
			// a reader believing it had covered the whole plan.
			fmt.Fprintf(&b, "%-12s %s (%s)\n", "not billet's", f.Address,
				strings.Join(f.Actions, "+"))
			fmt.Fprintf(&b, "             outside the billet module; this table says nothing "+
				"about it\n")

			continue
		}

		fmt.Fprintf(&b, "%-12s %s (%s)\n", f.Class, f.Address, strings.Join(f.Actions, "+"))
		fmt.Fprintf(&b, "             %s\n", f.Entry.Reason)

		if f.Entry.Remedy != "" {
			fmt.Fprintf(&b, "             run first: %s\n", f.Entry.Remedy)
		}
	}

	return b.String()
}
