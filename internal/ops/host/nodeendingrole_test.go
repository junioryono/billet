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
			drain:     "{{ not billet_node_should_run or (billet_candidate_config_compare.rc | default(2)) != 0 }}",
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

// THE TRANSACTION'S DRAIN IS THE ONE ITS REQUEST ASKED FOR: the check requires
// a drain exactly where the request was written.
func TestTheTransactionsCheckRequiresTheDrainItsRequestAskedFor(t *testing.T) {
	t.Parallel()

	tasks := readRoleTaskMaps(t, "services.yml")
	request := tasks[taskNamed(t, "services.yml", tasks,
		"Make the transaction's node stop a drain when the node will not run again or its config changes")]
	check := tasks[taskNamed(t, "services.yml", tasks, "Prove the transaction's node stop by the node's own ending")]

	if got, want := normalized(varText(check, "billet_ending_drain")), "{{ "+normalized(whenText(request))+" }}"; got != want {
		t.Errorf("the transaction's check requires drain %q, its request was written under %q", got, want)
	}
}

// ONE DRAIN REQUEST AND ONE MARKER, each named the same way by every party
// that reads or writes it.
func TestTheDrainRequestAndTheLeavingMarkerAreOnePathEach(t *testing.T) {
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

	tasks := readRoleTaskMaps(t, "services.yml")
	stat, ok := tasks[taskNamed(t, "services.yml", tasks,
		"Inspect whether a drain is still asked of the billet node")]["ansible.builtin.stat"].(map[string]any)
	if !ok || stat["path"] != nodeDrainRequestPath {
		t.Errorf("services.yml's start looks for the drain request at %v, want %s", stat["path"], nodeDrainRequestPath)
	}

	const marker = "/etc/billet/node-leaving"
	if !strings.Contains(read(roles+"vars/main.yml"), "billet_node_leaving_path: "+marker+"\n") {
		t.Errorf("the role does not keep its marker at %s", marker)
	}

	preremove := read("../../../deploy/scripts/preremove.sh")
	if !strings.Contains(preremove, "if [ -e "+marker+" ]; then") ||
		!strings.Contains(preremove, "leaving=$(cat "+marker+")") {
		t.Errorf("preremove.sh does not judge the record the role's marker at %s names", marker)
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
	if varValue(task, "billet_ending_drain") != true || varText(task, "billet_ending_invocation") != "" ||
		varText(task, "billet_ending_call") != "check" {
		t.Errorf("the start's drain check is not a standing check with a drain: %v", task["vars"])
	}

	if got, want := normalized(whenText(task)), normalized(strings.Join([]string{
		"billet_node_should_run",
		"not ansible_check_mode",
		"billet_node_before_start.stdout | default('') not in ['active', 'activating', 'reloading']",
		"billet_node_start_drain_request.stat.exists | default(true)",
	}, "\n")); got != want {
		t.Errorf("the start's drain check is guarded %q, want %q", got, want)
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

	for _, key := range []string{"src", "dest"} {
		if previewed[key] != rendered[key] {
			t.Errorf("account.yml previews %s %v, renders %v", key, previewed[key], rendered[key])
		}
	}

	if !strings.Contains(whenText(account[request]), "billet_config_preview.changed | default(false)") ||
		account[request]["ansible.builtin.include_tasks"] != "request-node-drain.yml" {
		t.Errorf("account.yml's request is not the drain request for a changed configuration: %v", account[request])
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
// PROVED: written before the configuration loses the node, found before the
// check, which names it, and forgotten only after; a node that returns
// elsewhere meanwhile is refused.
func TestALeavingNodesRecordIsRememberedUntilItsDrainIsProved(t *testing.T) {
	t.Parallel()

	account := readRoleTaskMaps(t, "account.yml")
	pending := taskNamed(t, "account.yml", account, "Read where a node an earlier converge removed kept its ending record")
	refuse := taskNamed(t, "account.yml", account, "Refuse a node that returns elsewhere while the one removed is unproved")
	remember := taskNamed(t, "account.yml", account, "Remember where a node this converge removes keeps its ending record")
	render := taskNamed(t, "account.yml", account, "Render billet configuration")

	if pending >= refuse || refuse >= remember || remember >= render {
		t.Errorf("account.yml reads %d, refuses %d, remembers %d, renders %d; want them in that order", pending,
			refuse, remember, render)
	}

	asserted, ok := account[refuse]["ansible.builtin.assert"].(map[string]any)
	if !ok || fmt.Sprint(asserted["that"]) !=
		"[(billet_node_pending_leaving.content | b64decode | trim) == billet_candidate_node_state_dir]" ||
		normalized(whenText(account[refuse])) != "billet_effective_config.get('node') is mapping "+
			"billet_node_pending_leaving.content is defined" {
		t.Errorf("a node returning while a removed one is unproved is refused under %q when %q, want it refused "+
			"unless it returns to the removed node's record", asserted["that"], whenText(account[refuse]))
	}

	if got, want := normalized(whenText(account[remember])), normalized(strings.Join([]string{
		"billet_previous_config.get('node') is mapping",
		"billet_effective_config.get('node') is not mapping",
		"not ansible_check_mode",
	}, "\n")); got != want {
		t.Errorf("the marker is written under %q, want %q", got, want)
	}

	copied, ok := account[remember]["ansible.builtin.copy"].(map[string]any)
	if !ok || normalized(fmt.Sprint(copied["content"])) != "{{ billet_previous_node_state_dir }}" ||
		copied["dest"] != "{{ billet_node_leaving_path }}" {
		t.Errorf("the marker is %v, want the installed node state directory at billet_node_leaving_path", copied)
	}

	const file = "services.yml"

	tasks := readRoleTaskMaps(t, file)
	find := taskNamed(t, file, tasks, "Find where a node that may not run here kept its ending record")
	check := taskNamed(t, file, tasks, "Prove the stop of a node that may not run here drained")
	forget := taskNamed(t, file, tasks, "Forget where a node whose drain is proved kept its ending record")

	if find >= check || check >= forget {
		t.Errorf("find %d, check %d, forget %d; want them in that order", find, check, forget)
	}

	if tasks[find]["ansible.builtin.include_tasks"] != "node-leaving.yml" {
		t.Errorf("services.yml does not find the leaving node's record through node-leaving.yml")
	}

	if varText(tasks[check], "billet_ending_state_dir") != "{{ billet_node_unwanted_state_dir }}" {
		t.Errorf("the leaving node's check reads %q, not the resolved record", varText(tasks[check],
			"billet_ending_state_dir"))
	}
}
