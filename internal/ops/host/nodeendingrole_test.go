package host

import (
	"fmt"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// readRoleTaskMaps reads a host-role task file as the generic mappings Ansible
// sees, with every block's tasks flattened in order beside the block.
func readRoleTaskMaps(t *testing.T, file string) []map[string]any {
	t.Helper()

	body, err := os.ReadFile("../../../ansible_collections/junioryono/billet/roles/host/tasks/" + file)
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}

	var tasks []map[string]any
	if err := yaml.Unmarshal(body, &tasks); err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}

	var flat []map[string]any

	var walk func([]map[string]any, string)

	walk = func(list []map[string]any, inherited string) {
		for _, task := range list {
			task[inheritedWhen] = inherited
			flat = append(flat, task)

			if block, ok := task["block"].([]any); ok {
				var inner []map[string]any

				for _, b := range block {
					if m, ok := b.(map[string]any); ok {
						inner = append(inner, m)
					}
				}

				walk(inner, strings.TrimSpace(inherited+"\n"+whenText(task)))
			}
		}
	}
	walk(tasks, "")

	return flat
}

// inheritedWhen is the key under which a flattened task carries the conditions
// of every block around it.
const inheritedWhen = "billet-test-inherited-when"

// effectiveWhen is a task's conditions with those of every block around it.
func effectiveWhen(task map[string]any) string {
	return strings.TrimSpace(fmt.Sprint(task[inheritedWhen]) + "\n" + whenText(task))
}

// taskNamed is the index of the task named, failing the test when it is absent.
func taskNamed(t *testing.T, file string, tasks []map[string]any, name string) int {
	t.Helper()

	i := slices.IndexFunc(tasks, func(task map[string]any) bool { return task["name"] == name })
	if i < 0 {
		t.Fatalf("%s has no task %q", file, name)
	}

	return i
}

// whenText is a task's conditions, joined.
func whenText(task map[string]any) string {
	switch when := task["when"].(type) {
	case string:
		return when
	case []any:
		parts := make([]string, 0, len(when))
		for _, c := range when {
			parts = append(parts, fmt.Sprint(c))
		}

		return strings.Join(parts, "\n")
	case nil:
		return ""
	default:
		return fmt.Sprint(when)
	}
}

// varValue is one of a task's vars, nil when it has none.
func varValue(task map[string]any, name string) any {
	vars, ok := task["vars"].(map[string]any)
	if !ok {
		return nil
	}

	return vars[name]
}

// varText is one of a task's vars as text, empty when it is not text.
func varText(task map[string]any, name string) string {
	if s, ok := varValue(task, name).(string); ok {
		return s
	}

	return ""
}

// normalized is text with every run of whitespace one space, so a folded
// expression compares as the expression it is.
func normalized(text string) string { return strings.Join(strings.Fields(text), " ") }

// standingInvocation is the invocation a caller asks about: none for a node
// with no process to stop, the one read before the stop otherwise.
func standingInvocation(before, noProcess string) string {
	return "{{ '' if " + noProcess + " else (" + before + ".stdout_lines | select('match', '^InvocationID=') | " +
		"first)[13:] }}"
}

// EVERY NODE STOP THE ROLE MUST PROVE DRAINED IS JUDGED AFTER IT, BY THE
// INVOCATION READ BEFORE IT, AND A NODE WITH NO PROCESS TO STOP IS JUDGED TOO
// (#127). The scenario tests drive the restart and the shared task file; these
// callers run only inside a whole converge, so their order, their guards and
// the questions they ask are held here, each to its exact expression: a check
// moved above its stop, guarded more narrowly than it, asked about the wrong
// invocation or without the drain its request asked for would let guests be
// orphaned.
func TestEveryDrainingStopTheRoleProvesIsJudgedByItsEnding(t *testing.T) {
	t.Parallel()

	for _, site := range []struct {
		file, record, stop, check, before string
		// noProcess is the condition under which the check is a standing one.
		noProcess string
		// drain is the drain the check requires, as text; empty for true.
		drain string
		// when is the check's own conditions; empty for the stop's.
		when []string
	}{
		{
			file:      "network.yml",
			record:    "Record the billet node's invocation before the network drain",
			stop:      "Drain billet compute before changing guest networking",
			check:     "Prove the network drain by the node's own ending",
			before:    "billet_network_node_before",
			noProcess: "'MainPID=0' in billet_network_node_before.stdout_lines",
		},
		{
			file:      "services.yml",
			record:    "Record the billet node's invocation before the transaction's stop",
			stop:      "Gracefully stop the billet node before changing its guest contract",
			check:     "Prove the transaction's node stop by the node's own ending",
			before:    "billet_transaction_node_before",
			noProcess: "'MainPID=0' in billet_transaction_node_before.stdout_lines",
			drain: "{{ not billet_node_should_run or (billet_candidate_config_compare.rc | default(2)) != 0 or " +
				"billet_node_drain_owed_dir | default('') | length > 0 }}",
		},
		{
			file:   "services.yml",
			record: "Record the invocation of a node that may not run here before its stop",
			stop:   "Stop and disable the billet node unless it may run here",
			check:  "Prove the stop of a node that may not run here drained",
			before: "billet_node_unwanted_before",
			noProcess: "(billet_node_unwanted_state.stdout | default('') in ['inactive', 'failed'] or 'MainPID=0' in " +
				"billet_node_unwanted_before.stdout_lines)",
			when: []string{
				"not billet_node_should_run",
				"not ansible_check_mode",
				"billet_node_unwanted_state.stdout | default('') not in ['inactive', 'failed'] or " +
					"billet_node_unwanted_state_dir | length > 0",
			},
		},
		{
			file:      "upgrade-rollback.yml",
			record:    "Record the billet node's invocation before recovery stops it",
			stop:      "Stop compute before recovering the authoritative ledger",
			check:     "Prove recovery's node stop drained by the node's own ending",
			before:    "billet_recovery_node_before",
			noProcess: "'MainPID=0' in billet_recovery_node_before.stdout_lines",
		},
	} {
		t.Run(site.check, func(t *testing.T) {
			t.Parallel()

			tasks := readRoleTaskMaps(t, site.file)
			record := taskNamed(t, site.file, tasks, site.record)
			stop := taskNamed(t, site.file, tasks, site.stop)
			check := taskNamed(t, site.file, tasks, site.check)

			if record >= stop || stop >= check {
				t.Errorf("%s: record %d, stop %d, check %d; want the invocation read, then the stop, then the "+
					"check", site.file, record, stop, check)
			}

			task := tasks[check]
			if task["ansible.builtin.include_tasks"] != "node-ending.yml" || varText(task, "billet_ending_call") != "check" {
				t.Errorf("%s: %q does not ask node-ending.yml for a check", site.file, site.check)
			}

			switch drain := varValue(task, "billet_ending_drain"); {
			case site.drain == "" && drain != true:
				t.Errorf("%s: %q requires drain %v, want true", site.file, site.check, drain)
			case site.drain != "" && normalized(varText(task, "billet_ending_drain")) != site.drain:
				t.Errorf("%s: %q requires drain %q, want %q", site.file, site.check,
					normalized(varText(task, "billet_ending_drain")), site.drain)
			}

			want := normalized(whenText(tasks[stop]))
			if site.when != nil {
				want = normalized(strings.Join(site.when, "\n"))
			}

			if got := normalized(whenText(task)); got != want {
				t.Errorf("%s: %q is guarded %q, want %q", site.file, site.check, got, want)
			}

			if got, want := normalized(varText(task, "billet_ending_invocation")),
				standingInvocation(site.before, site.noProcess); got != want {
				t.Errorf("%s: %q asks about\n%s\nwant\n%s", site.file, site.check, got, want)
			}

			// A RUNNING PROCESS WITH NO INVOCATION is refused at the read, as
			// one more way the read fails.
			guard, ok := tasks[record]["failed_when"].(string)
			if suffix := "!= 1 or ('MainPID=0' not in " + site.before + ".stdout_lines and 'InvocationID=' in " +
				site.before + ".stdout_lines)"; !ok || !strings.HasSuffix(normalized(guard), suffix) {
				t.Errorf("%s: %q does not refuse a running process systemd names no invocation for:\n%s",
					site.file, site.record, guard)
			}
		})
	}
}

// THE TRANSACTION'S DRAIN IS THE ONE ITS REQUEST ASKED FOR, OR ONE OWED: the
// check requires a drain exactly where the request was written or a drain is
// owed, and judges the owed drain's record.
func TestTheTransactionsCheckRequiresTheDrainItsRequestAskedFor(t *testing.T) {
	t.Parallel()

	tasks := readRoleTaskMaps(t, "services.yml")
	request := tasks[taskNamed(t, "services.yml", tasks,
		"Make the transaction's node stop a drain when the node will not run again or its config changes")]
	check := tasks[taskNamed(t, "services.yml", tasks, "Prove the transaction's node stop by the node's own ending")]

	if got, want := normalized(varText(check, "billet_ending_drain")), "{{ "+normalized(whenText(request))+" }}"; got != want {
		t.Errorf("the transaction's check requires drain %q, want %q", got, want)
	}

	if got := varText(check, "billet_ending_state_dir"); got != "{{ billet_node_drain_owed_dir | default('') }}" {
		t.Errorf("the transaction's check reads the record %q, not the owed drain's", got)
	}
}

// ONE DRAIN REQUEST AND ONE OWED DRAIN, each named the same way by every party
// that reads or writes it.
func TestTheDrainRequestAndTheOwedDrainAreOnePathEach(t *testing.T) {
	t.Parallel()

	roles := "../../../ansible_collections/junioryono/billet/roles/host/"

	read := func(file string) string {
		t.Helper()

		body, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}

		return string(body)
	}

	if body := read(roles + "tasks/node-restart.yml"); !strings.Contains(body,
		"default('"+nodeDrainRequestPath+"')") {
		t.Errorf("node-restart.yml does not look for the drain request at %s", nodeDrainRequestPath)
	}

	for _, site := range []struct{ file, task string }{
		{"services.yml", "Inspect whether a drain is still asked of the billet node"},
		{"upgrade-finalize.yml", "Inspect whether a drain is still asked of the committed billet node"},
	} {
		tasks := readRoleTaskMaps(t, site.file)
		stat, ok := tasks[taskNamed(t, site.file, tasks, site.task)]["ansible.builtin.stat"].(map[string]any)

		if !ok || stat["path"] != nodeDrainRequestPath {
			t.Errorf("%s looks for the drain request at %v, want %s", site.file, stat["path"], nodeDrainRequestPath)
		}
	}

	const owed = "/etc/billet/node-drain-owed"
	if !strings.Contains(read(roles+"vars/main.yml"), "billet_node_drain_owed_path: "+owed+"\n") {
		t.Errorf("the role does not keep its owed drain at %s", owed)
	}

	preremove := read("../../../deploy/scripts/preremove.sh")
	if !strings.Contains(preremove, "if [ -e "+owed+" ]; then") ||
		!strings.Contains(preremove, "owed=$(cat "+owed+")") ||
		!strings.Contains(preremove, `check_node_ending --state-dir "${owed}"`) {
		t.Errorf("preremove.sh does not judge the record the role's owed drain at %s names", owed)
	}
}

// A STOPPED NODE IS PROVED DRAINED BEFORE THE ORDINARY START WHILE A DRAIN IS
// ASKED FOR, and the request a changed configuration needs is written before
// that configuration is installed.
func TestAStartWhileADrainIsAskedForIsOverADrain(t *testing.T) {
	t.Parallel()

	tasks := readRoleTaskMaps(t, "services.yml")
	stat := taskNamed(t, "services.yml", tasks, "Inspect whether a drain is still asked of the billet node")
	check := taskNamed(t, "services.yml", tasks,
		"Prove a stopped node drained before its start where a drain is asked for")
	admit := taskNamed(t, "services.yml", tasks, "Admit the billet node's start over its last ending")
	start := taskNamed(t, "services.yml", tasks, "Enable and start the billet node")

	if stat >= check || check >= admit || admit >= start {
		t.Errorf("stat %d, check %d, admit %d, start %d; want them in that order", stat, check, admit, start)
	}

	task := tasks[check]
	if task["ansible.builtin.include_tasks"] != "node-ending.yml" || varValue(task, "billet_ending_drain") != true ||
		varText(task, "billet_ending_invocation") != "" || varText(task, "billet_ending_call") != "check" {
		t.Errorf("the start's drain check is not a standing check with a drain: %v", task["vars"])
	}

	if got, want := normalized(whenText(task)), normalized(strings.Join([]string{
		"billet_node_should_run",
		"not ansible_check_mode",
		"billet_node_before_start.stdout | default('') not in ['active', 'activating', 'reloading']",
		"billet_node_start_drain_request.stat.exists | default(true) or billet_node_drain_owed_dir | default('') | length > 0",
	}, "\n")); got != want {
		t.Errorf("the start's drain check is guarded %q, want %q", got, want)
	}

	proved := taskNamed(t, "services.yml", tasks, "Note that the stopped node's last stop proved it drained")
	if proved <= check || proved >= admit || whenText(tasks[proved]) != whenText(task) {
		t.Errorf("the start notes its drain proved at %d under %q; want it after the check at %d, under its guard",
			proved, whenText(tasks[proved]), check)
	}

	account := readRoleTaskMaps(t, "account.yml")
	preview := taskNamed(t, "account.yml", account, "Preview whether this converge changes the billet configuration")
	request := taskNamed(t, "account.yml", account, "Make the node's next stop a drain before its configuration changes")
	render := taskNamed(t, "account.yml", account, "Render billet configuration")

	if preview >= request || request >= render {
		t.Errorf("account.yml previews %d, requests %d, renders %d; want them in that order", preview, request, render)
	}

	if account[preview]["check_mode"] != true {
		t.Error("account.yml's preview of the configuration is not a check-mode rendering")
	}

	previewed, okPreview := account[preview]["ansible.builtin.template"].(map[string]any)
	rendered, okRender := account[render]["ansible.builtin.template"].(map[string]any)

	if !okPreview || !okRender {
		t.Fatalf("account.yml's preview or rendering is not a template: %v, %v", previewed, rendered)
	}

	// THE PREVIEW IS THE RENDERING, run in check mode: the same template, the
	// same document, under the same conditions, read through its register.
	if !reflect.DeepEqual(previewed, rendered) || !reflect.DeepEqual(account[preview]["vars"], account[render]["vars"]) ||
		whenText(account[preview]) != whenText(account[render]) || account[preview]["register"] != "billet_config_preview" {
		t.Errorf("account.yml previews %v (vars %v, when %q, register %v), renders %v (vars %v, when %q)", previewed,
			account[preview]["vars"], whenText(account[preview]), account[preview]["register"], rendered,
			account[render]["vars"], whenText(account[render]))
	}

	if got, want := normalized(whenText(account[request])), normalized(strings.Join([]string{
		"billet_node_should_run",
		"not ansible_check_mode",
		"not billet_binary_upgrade | default(false)",
		"billet_config_preview.changed | default(false)",
	}, "\n")); got != want || account[request]["ansible.builtin.include_tasks"] != "request-node-drain.yml" {
		t.Errorf("account.yml's drain request for a changed configuration is %v under %q, want request-node-drain.yml "+
			"under %q", account[request]["ansible.builtin.include_tasks"], got, want)
	}
}

// THE STOPPED NODE'S OWN RELEASE IS ASKED FIRST: the transaction's installed
// binary before its candidate, and recovery's previous release before the
// candidate, each with the configuration installed now before its own.
func TestTheTransactionAsksTheStoppedNodesReleaseFirst(t *testing.T) {
	t.Parallel()

	tasks := readRoleTaskMaps(t, "services.yml")
	check := tasks[taskNamed(t, "services.yml", tasks, "Prove the transaction's node stop by the node's own ending")]

	with, ok := varValue(check, "billet_ending_with").([]any)
	if !ok {
		t.Fatalf("the transaction names no list of executables: %v", varValue(check, "billet_ending_with"))
	}

	var got []string

	for _, pair := range with {
		m, ok := pair.(map[string]any)
		if !ok {
			t.Fatalf("the transaction names %v, not an executable and its configuration", pair)
		}

		got = append(got, fmt.Sprint(m["exe"], " ", m["config"]))
	}

	want := []string{
		"/usr/bin/billet /etc/billet/billet.yaml",
		"{{ billet_upgrade_recovery_dir }}/billet.candidate /etc/billet/billet.yaml",
		"{{ billet_upgrade_recovery_dir }}/billet.candidate {{ billet_upgrade_recovery_dir }}/billet.yaml.candidate",
	}
	if !slices.Equal(got, want) {
		t.Errorf("the transaction asks %q, want %q", got, want)
	}

	rollback := readRoleTaskMaps(t, "upgrade-rollback.yml")
	recovery := strings.Join(strings.Fields(varText(rollback[taskNamed(t, "upgrade-rollback.yml", rollback,
		"Prove recovery's node stop drained by the node's own ending")], "billet_ending_with")), " ")

	order := []string{
		"'/billet.previous', 'config': '/etc/billet/billet.yaml'",
		"'/billet.previous', 'config': billet_upgrade_recovery_dir ~ '/billet.yaml.previous'",
		"'/billet.candidate', 'config': '/etc/billet/billet.yaml'",
		"'/billet.candidate', 'config': billet_upgrade_recovery_dir ~ '/billet.yaml.candidate'",
	}

	at := -1

	for _, pair := range order {
		i := strings.Index(recovery, pair)
		if i <= at {
			t.Errorf("recovery does not ask %q after the pairs before it:\n%s", pair, recovery)
		}

		at = i
	}
}

// RECOVERY RECORDS THE INVOCATION BEFORE IT ASKS FOR THE DRAIN, under the same
// guard as its stop, so the request is never written for a stop that cannot be
// bound to its ending.
func TestRecoveryRecordsTheNodeBeforeItAsksForTheDrain(t *testing.T) {
	t.Parallel()

	const file = "upgrade-rollback.yml"

	tasks := readRoleTaskMaps(t, file)
	record := taskNamed(t, file, tasks, "Record the billet node's invocation before recovery stops it")
	request := taskNamed(t, file, tasks, "Make the stop before recovering the ledger a drain")
	stop := taskNamed(t, file, tasks, "Stop compute before recovering the authoritative ledger")

	if record >= request || request >= stop {
		t.Errorf("record %d, request %d, stop %d; want the invocation read, then the request, then the stop",
			record, request, stop)
	}

	if whenText(tasks[request]) != whenText(tasks[stop]) {
		t.Errorf("the request is guarded %q, the stop %q", whenText(tasks[request]), whenText(tasks[stop]))
	}
}

// FINALIZE STOPS A READINESS PROBE BEFORE IT JUDGES THE START, AND STARTS
// RATHER THAN RESTARTS: a start judged over a running probe, or a restart whose
// stop nothing judges, would start the node over an ending nobody proved.
func TestFinalizeStopsTheProbeBeforeItAdmitsTheNodesStart(t *testing.T) {
	t.Parallel()

	const file = "upgrade-finalize.yml"

	tasks := readRoleTaskMaps(t, file)
	probe := taskNamed(t, file, tasks, "Stop the committed billet node's readiness probe before its start")
	admit := taskNamed(t, file, tasks, "Admit the committed billet node's start over its last ending")
	start := taskNamed(t, file, tasks, "Start the committed billet node after the control plane")

	if probe >= admit || admit >= start {
		t.Errorf("probe stop %d, admit %d, start %d; want them in that order", probe, admit, start)
	}

	module, ok := tasks[start]["ansible.builtin.systemd_service"].(map[string]any)
	if !ok || module["state"] != "started" {
		t.Errorf("finalize's node start is state %v, want started", module["state"])
	}

	if varText(tasks[admit], "billet_ending_call") != "admit-start" {
		t.Errorf("finalize's admission does not ask admit-start")
	}
}

// A DRAIN THE NODE OWES IS RECORDED BEFORE ANYTHING THAT NEEDS IT CHANGES THE
// HOST, AND FORGOTTEN ONLY BY A CONVERGE THAT PROVED IT: read before the
// configuration changes, a node elsewhere than its record refused, recorded
// before the render (or the transaction's stop), the restart run while it is
// owed, and removed after every check that can prove it, only once one did.
func TestAnOwedDrainIsRecordedFirstAndForgottenOnlyOnceProved(t *testing.T) {
	t.Parallel()

	account := readRoleTaskMaps(t, "account.yml")
	read := taskNamed(t, "account.yml", account, "Read the drain the billet node owes before its configuration changes")
	refuse := taskNamed(t, "account.yml", account, "Refuse a node elsewhere than the record whose drain it owes")
	preview := taskNamed(t, "account.yml", account, "Preview whether this converge changes the billet configuration")
	record := taskNamed(t, "account.yml", account, "Record the drain the billet node owes before its configuration changes")
	note := taskNamed(t, "account.yml", account, "Note the record whose drain this converge owes")
	render := taskNamed(t, "account.yml", account, "Render billet configuration")

	if read >= refuse || refuse >= preview || preview >= record || record >= note || note >= render {
		t.Errorf("account.yml reads %d, refuses %d, previews %d, records %d, notes %d, renders %d; want them in "+
			"that order", read, refuse, preview, record, note, render)
	}

	if account[read]["ansible.builtin.include_tasks"] != "node-drain-owed.yml" || whenText(account[read]) != "" {
		t.Errorf("account.yml reads the owed drain as %v when %q, want node-drain-owed.yml always",
			account[read]["ansible.builtin.include_tasks"], whenText(account[read]))
	}

	asserted, ok := account[refuse]["ansible.builtin.assert"].(map[string]any)
	if !ok || fmt.Sprint(asserted["that"]) != "[billet_node_drain_owed_dir == billet_candidate_node_state_dir]" ||
		normalized(whenText(account[refuse])) != "billet_effective_config.get('node') is mapping "+
			"billet_node_drain_owed_dir | length > 0" {
		t.Errorf("a node configured while a drain is owed is refused under %q when %q, want it refused unless it "+
			"is the owed drain's record", asserted["that"], whenText(account[refuse]))
	}

	copied, ok := account[record]["ansible.builtin.copy"].(map[string]any)
	if !ok || normalized(fmt.Sprint(copied["content"])) != "{{ billet_previous_node_state_dir }}" ||
		copied["dest"] != "{{ billet_node_drain_owed_path }}" || account[record]["register"] != "billet_node_drain_owed_write" ||
		normalized(whenText(account[record])) != normalized(strings.Join([]string{
			"billet_previous_config.get('node') is mapping",
			"billet_effective_config.get('node') is not mapping or billet_config_preview.changed | default(false)",
			"not ansible_check_mode",
		}, "\n")) {
		t.Errorf("account.yml records the owed drain as %v (register %v) when %q, want the installed node state "+
			"directory whenever the node is changed or rendered away", copied, account[record]["register"],
			whenText(account[record]))
	}

	if got := fmt.Sprint(account[note]["ansible.builtin.set_fact"]); got != "map[billet_node_drain_owed_dir:{{ "+
		"billet_previous_node_state_dir }} billet_node_drain_proved:false]" ||
		whenText(account[note]) != "billet_node_drain_owed_write is not skipped" {
		t.Errorf("account.yml notes the owed drain as %s when %q, want the recorded directory, unproved",
			got, whenText(account[note]))
	}

	flush := taskNamed(t, "account.yml", account, "Flush the drain the billet node owes before its configuration changes")
	if flush <= record || flush >= render || fmt.Sprint(account[flush]["ansible.builtin.command"]) !=
		"map[argv:[sync -f {{ billet_node_drain_owed_path }}]]" ||
		whenText(account[flush]) != "billet_node_drain_owed_write is not skipped" {
		t.Errorf("account.yml flushes the owed drain at %d (record %d, render %d) as %v when %q", flush, record,
			render, account[flush]["ansible.builtin.command"], whenText(account[flush]))
	}

	const file = "services.yml"

	tasks := readRoleTaskMaps(t, file)
	txRecord := taskNamed(t, file, tasks, "Record the drain the billet node owes before the transaction's stop")
	txStop := taskNamed(t, file, tasks, "Gracefully stop the billet node before changing its guest contract")
	request := tasks[taskNamed(t, file, tasks,
		"Make the transaction's node stop a drain when the node will not run again or its config changes")]

	if txRecord >= txStop || normalized(whenText(tasks[txRecord])) != normalized(strings.Join([]string{
		"billet_service_state_before_upgrade.results[1].rc != 4",
		"billet_previous_config.get('node') is mapping",
		"not billet_node_should_run or (billet_candidate_config_compare.rc | default(2)) != 0",
	}, "\n")) || !strings.HasSuffix(normalized(whenText(request)), " or billet_node_drain_owed_dir | default('') | length > 0") {
		t.Errorf("the transaction records its owed drain at %d (stop %d) when %q", txRecord, txStop,
			whenText(tasks[txRecord]))
	}

	restart := tasks[taskNamed(t, file, tasks, "Restart the billet node after non-binary inputs change")]
	if !strings.HasSuffix(normalized(whenText(restart)), " or billet_node_drain_owed_dir | default('') | length > 0") {
		t.Errorf("the ordinary restart does not run while a drain is owed: %q", whenText(restart))
	}

	resolve := tasks[taskNamed(t, file, tasks, "Resolve the ending record of a node that may not run here")]
	if got := normalized(fmt.Sprint(resolve["ansible.builtin.set_fact"])); got != "map[billet_node_unwanted_state_dir:{{ "+
		"billet_previous_node_state_dir | default('', true) or billet_node_drain_owed_dir | default('') }}]" {
		t.Errorf("a node that may not run here finds its record as %s", got)
	}

	forget := taskNamed(t, file, tasks, "Forget the drain the billet node owed once this converge proved it")
	withdraw := taskNamed(t, file, tasks, "Withdraw the drain request now that the node is where this converge leaves it")

	// EACH PROOF FOLLOWS ITS CHECK, UNDER ITS CHECK'S GUARD, and the owed drain
	// is forgotten only after them.
	for _, pair := range []struct{ check, proof, guard string }{
		{
			check: "Prove the transaction's node stop by the node's own ending",
			proof: "Note that the transaction's stop proved the node drained",
			guard: "billet_service_state_before_upgrade.results[1].rc != 4\n{{DRAIN}}",
		},
		{
			check: "Prove a stopped node drained before its start where a drain is asked for",
			proof: "Note that the stopped node's last stop proved it drained",
		},
		{
			check: "Prove the stop of a node that may not run here drained",
			proof: "Note that the stop of a node that may not run here proved it drained",
		},
	} {
		check, proof := taskNamed(t, file, tasks, pair.check), taskNamed(t, file, tasks, pair.proof)

		// THE PROOF RUNS EXACTLY WHEN ITS CHECK DOES, block conditions and all.
		guard := effectiveWhen(tasks[check])
		if pair.guard != "" {
			drain := strings.TrimSuffix(strings.TrimPrefix(normalized(varText(tasks[check], "billet_ending_drain")),
				"{{ "), " }}")
			guard = strings.TrimSpace(fmt.Sprint(tasks[check][inheritedWhen]) + "\n" +
				strings.ReplaceAll(pair.guard, "{{DRAIN}}", drain))
		}

		if proof != check+1 || proof >= forget || normalized(effectiveWhen(tasks[proof])) != normalized(guard) ||
			fmt.Sprint(tasks[proof]["ansible.builtin.set_fact"]) != "map[billet_node_drain_proved:true]" {
			t.Errorf("%q at %d (check %d, forget %d) sets %v when %q, want proved right after its check, under %q",
				pair.proof, proof, check, forget, tasks[proof]["ansible.builtin.set_fact"], whenText(tasks[proof]),
				guard)
		}
	}

	removed, ok := tasks[forget]["ansible.builtin.file"].(map[string]any)
	if !ok || removed["path"] != "{{ billet_node_drain_owed_path }}" || removed["state"] != "absent" || forget >= withdraw ||
		normalized(whenText(tasks[forget])) != normalized(strings.Join([]string{
			"not ansible_check_mode",
			"billet_node_drain_owed_dir | default('') | length > 0",
			"billet_node_drain_proved | default(false) | bool",
		}, "\n")) {
		t.Errorf("services.yml forgets the owed drain as %v at %d (withdraw %d) when %q, want it removed only once "+
			"proved", removed, forget, withdraw, whenText(tasks[forget]))
	}

	if varText(tasks[taskNamed(t, file, tasks, "Prove the stop of a node that may not run here drained")],
		"billet_ending_state_dir") != "{{ billet_node_unwanted_state_dir }}" {
		t.Error("the leaving node's check does not read the resolved record")
	}

	finalize := readRoleTaskMaps(t, "upgrade-finalize.yml")
	check := taskNamed(t, "upgrade-finalize.yml", finalize,
		"Prove the committed billet node drained before its start where a drain is asked for")
	admit := taskNamed(t, "upgrade-finalize.yml", finalize, "Admit the committed billet node's start over its last ending")

	if check >= admit || varText(finalize[check], "billet_ending_state_dir") != "{{ billet_node_drain_owed_dir | default('') }}" ||
		varValue(finalize[check], "billet_ending_drain") != true || !strings.HasSuffix(normalized(whenText(finalize[check])),
		"billet_finalization_drain_request.stat.exists | default(true) or billet_node_drain_owed_dir | default('') | length > 0") {
		t.Errorf("finalize's drain check at %d (admit %d) is %v when %q", check, admit, finalize[check]["vars"],
			whenText(finalize[check]))
	}

	restartFile := readRoleTaskMaps(t, "node-restart.yml")
	ask := restartFile[taskNamed(t, "node-restart.yml", restartFile, "Ask the node's stop to drain where it must have")]
	asked, ok := ask["ansible.builtin.copy"].(map[string]any)
	if !ok || asked["dest"] != "{{ billet_node_drain_request | default('"+nodeDrainRequestPath+"') }}" ||
		normalized(whenText(ask)) != "billet_node_restart_drain | bool not billet_node_restart_drain_request.stat.exists" {
		t.Errorf("the restart asks for its drain as %v when %q", asked, whenText(ask))
	}

	owedFile := readRoleTaskMaps(t, "node-drain-owed.yml")
	if got := normalized(fmt.Sprint(owedFile[taskNamed(t, "node-drain-owed.yml", owedFile,
		"Note the record whose drain the billet node owes")]["ansible.builtin.set_fact"])); !strings.HasSuffix(got,
		" billet_node_drain_proved:false]") {
		t.Errorf("reading the owed drain keeps a proof an earlier inclusion set: %s", got)
	}

	rollback := readRoleTaskMaps(t, "upgrade-rollback.yml")
	if got := varText(rollback[taskNamed(t, "upgrade-rollback.yml", rollback,
		"Prove recovery's node stop drained by the node's own ending")], "billet_ending_state_dir"); got !=
		"{{ billet_node_drain_owed_dir | default('') }}" {
		t.Errorf("recovery's check reads the record %q, not the owed drain's", got)
	}

	txTasks := readRoleTaskMaps(t, file)
	txFlush := taskNamed(t, file, txTasks, "Flush the drain the billet node owes before the transaction's stop")
	if txFlush <= txRecord || txFlush >= txStop || fmt.Sprint(txTasks[txFlush]["ansible.builtin.command"]) !=
		"map[argv:[sync -f {{ billet_node_drain_owed_path }}]]" ||
		whenText(txTasks[txFlush]) != "billet_transaction_drain_owed is not skipped" {
		t.Errorf("the transaction flushes its owed drain at %d (record %d, stop %d) as %v when %q", txFlush,
			txRecord, txStop, txTasks[txFlush]["ansible.builtin.command"], whenText(txTasks[txFlush]))
	}

	recovery := readRoleTaskMaps(t, "upgrade-recover.yml")
	readOwed := taskNamed(t, "upgrade-recover.yml", recovery, "Read the drain the billet node owes before recovery")
	finish := taskNamed(t, "upgrade-recover.yml", recovery, "Finish a committed host transaction without rolling it back")

	if readOwed >= finish || recovery[readOwed]["ansible.builtin.include_tasks"] != "node-drain-owed.yml" ||
		whenText(recovery[readOwed]) != "" {
		t.Errorf("recovery reads the owed drain at %d (finish %d) as %v when %q", readOwed, finish,
			recovery[readOwed]["ansible.builtin.include_tasks"], whenText(recovery[readOwed]))
	}

	note2 := restartFile[taskNamed(t, "node-restart.yml", restartFile, "Note whether the node's stop must have drained")]
	if !strings.HasSuffix(normalized(fmt.Sprint(note2["ansible.builtin.set_fact"])), "or billet_node_drain_owed_dir | default('') | length > 0 }}]") {
		t.Errorf("the restart's drain does not follow an owed drain: %v", note2["ansible.builtin.set_fact"])
	}
}
