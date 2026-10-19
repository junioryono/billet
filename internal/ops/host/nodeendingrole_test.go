package host

import (
	"fmt"
	"os"
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

	var walk func([]map[string]any)

	walk = func(list []map[string]any) {
		for _, task := range list {
			flat = append(flat, task)

			if block, ok := task["block"].([]any); ok {
				var inner []map[string]any

				for _, b := range block {
					if m, ok := b.(map[string]any); ok {
						inner = append(inner, m)
					}
				}

				walk(inner)
			}
		}
	}
	walk(tasks)

	return flat
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
			if s, ok := c.(string); ok {
				parts = append(parts, s)
			}
		}

		return strings.Join(parts, "\n")
	}

	return ""
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

// EVERY NODE STOP THE ROLE MUST PROVE DRAINED IS JUDGED AFTER IT, BY THE
// INVOCATION READ BEFORE IT, AND A NODE WITH NO PROCESS TO STOP IS JUDGED TOO
// (#127). The scenario tests drive the restart; these callers run only inside
// a whole converge, so the order and the standing branch are held here: a check
// moved above its stop, guarded off for a node with no process, or asked about
// an invocation that was never read would let guests be orphaned.
func TestEveryDrainingStopTheRoleProvesIsJudgedByItsEnding(t *testing.T) {
	t.Parallel()

	for _, site := range []struct {
		file, record, stop, check, before string
	}{
		{
			file:   "network.yml",
			record: "Record the billet node's invocation before the network drain",
			stop:   "Drain billet compute before changing guest networking",
			check:  "Prove the network drain by the node's own ending",
			before: "billet_network_node_before",
		},
		{
			file:   "services.yml",
			record: "Record the billet node's invocation before the transaction's stop",
			stop:   "Gracefully stop the billet node before changing its guest contract",
			check:  "Prove the transaction's node stop by the node's own ending",
			before: "billet_transaction_node_before",
		},
		{
			file:   "services.yml",
			record: "Record the invocation of a node that may not run here before its stop",
			stop:   "Stop and disable the billet node unless it may run here",
			check:  "Prove the stop of a node that may not run here drained",
			before: "billet_node_unwanted_before",
		},
		{
			file:   "upgrade-rollback.yml",
			record: "Record the billet node's invocation before recovery stops it",
			stop:   "Stop compute before recovering the authoritative ledger",
			check:  "Prove recovery's node stop drained by the node's own ending",
			before: "billet_recovery_node_before",
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

			if drain := varValue(task, "billet_ending_drain"); drain != true &&
				!strings.Contains(varText(task, "billet_ending_drain"), "billet_candidate_config_compare") {
				t.Errorf("%s: %q requires no drain (%v)", site.file, site.check, drain)
			}

			if strings.Contains(whenText(task), "MainPID=0") {
				t.Errorf("%s: %q is guarded off for a node with no process to stop:\n%s", site.file, site.check,
					whenText(task))
			}

			invocation := varText(task, "billet_ending_invocation")
			if !strings.Contains(invocation, "'' if") || !strings.Contains(invocation, site.before+".stdout_lines") {
				t.Errorf("%s: %q does not ask a standing question for a node with no process and the recorded "+
					"invocation otherwise:\n%s", site.file, site.check, invocation)
			}

			// A RUNNING PROCESS WITH NO INVOCATION is refused at the read.
			guard, ok := tasks[record]["failed_when"].(string)
			if !ok || !strings.Contains(guard, "'MainPID=0' not in "+site.before+".stdout_lines and 'InvocationID=' in "+
				site.before+".stdout_lines") {
				t.Errorf("%s: %q does not refuse a running process systemd names no invocation for:\n%s",
					site.file, site.record, guard)
			}
		})
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

// A NODE THE CONVERGE RENDERS AWAY KEEPS ITS RECORD'S PATH UNTIL ITS DRAIN IS
// PROVED: written before the configuration loses the node, read before the
// check, which names it, and forgotten only after.
func TestALeavingNodesRecordIsRememberedUntilItsDrainIsProved(t *testing.T) {
	t.Parallel()

	account := readRoleTaskMaps(t, "account.yml")
	remember := taskNamed(t, "account.yml", account, "Remember where a node this converge removes keeps its ending record")
	render := taskNamed(t, "account.yml", account, "Render billet configuration")

	if remember >= render {
		t.Errorf("account.yml remembers the leaving node's record at %d, after rendering at %d", remember, render)
	}

	const file = "services.yml"

	tasks := readRoleTaskMaps(t, file)
	read := taskNamed(t, file, tasks, "Read where a node that may not run here kept its ending record")
	resolve := taskNamed(t, file, tasks, "Resolve the ending record of a node that may not run here")
	check := taskNamed(t, file, tasks, "Prove the stop of a node that may not run here drained")
	forget := taskNamed(t, file, tasks, "Forget where a node whose drain is proved kept its ending record")

	if read >= resolve || resolve >= check || check >= forget {
		t.Errorf("read %d, resolve %d, check %d, forget %d; want them in that order", read, resolve, check, forget)
	}

	if varText(tasks[check], "billet_ending_state_dir") != "{{ billet_node_unwanted_state_dir }}" {
		t.Errorf("the leaving node's check reads %q, not the resolved record", varText(tasks[check],
			"billet_ending_state_dir"))
	}

	if !strings.Contains(whenText(tasks[check]), "billet_node_unwanted_state_dir | length > 0") {
		t.Errorf("the leaving node's check does not run for a stopped node with a record:\n%s",
			whenText(tasks[check]))
	}
}
