package host

import (
	"os"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// roleTask is the part of a host-role task this test reads.
type roleTask struct {
	Name    string     `yaml:"name"`
	When    any        `yaml:"when"`
	Include string     `yaml:"ansible.builtin.include_tasks"`
	Block   []roleTask `yaml:"block"`
}

func (t roleTask) conditions() []string {
	switch when := t.When.(type) {
	case string:
		return []string{when}
	case []any:
		out := make([]string, 0, len(when))
		for _, condition := range when {
			if text, ok := condition.(string); ok {
				out = append(out, text)
			}
		}

		return out
	}

	return nil
}

func readRoleTasks(t *testing.T, file string) []roleTask {
	t.Helper()

	body, err := os.ReadFile("../../../ansible_collections/junioryono/billet/roles/host/tasks/" + file)
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}

	var tasks []roleTask
	if err := yaml.Unmarshal(body, &tasks); err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}

	return tasks
}

func taskIndex(tasks []roleTask, name string) int {
	return slices.IndexFunc(tasks, func(task roleTask) bool { return task.Name == name })
}

// EVERY STOP THAT MUST DRAIN IS PRECEDED BY THE REQUEST, UNDER ITS OWN
// CONDITIONS, AND FOLLOWED BY THE RELEASE (#374). The role's other gates check
// imports, conditions and task text, never the order of siblings, so a request
// moved below its stop, or guarded more narrowly than it, would leave that stop
// handing guests over on a host being taken apart.
func TestEveryDrainingStopInTheRoleIsRequestedFirstAndReleasedAfter(t *testing.T) {
	t.Parallel()

	for _, site := range []struct {
		file, request, stop, release string
		// sameConditions says the request is guarded exactly as the stop is;
		// otherwise its conditions must include all of the stop's.
		sameConditions bool
	}{
		{
			file:           "network.yml",
			request:        "Make every node stop while guest networking changes a drain",
			stop:           "Drain billet compute before changing guest networking",
			release:        "Let node stops hand over again now that guest networking is in place",
			sameConditions: true,
		},
		{
			// NARROWED TO A NODE THAT IS UP: an inactive or failed unit has no
			// process for the stop to drain, and asking anyway on a host that runs
			// no node changed it on every converge.
			file:    "services.yml",
			request: "Make the stop of a node that may not run here a drain",
			stop:    "Stop and disable the billet node unless it may run here",
			release: "Withdraw the drain request now that the node is where this converge leaves it",
		},
		{
			file:    "services.yml",
			request: "Make a node restart for a changed config a drain",
			stop:    "Restart the billet node after non-binary inputs change",
			release: "Withdraw the drain request now that the node is where this converge leaves it",
		},
		{
			file:    "upgrade-rollback.yml",
			request: "Make the stop before recovering the ledger a drain",
			stop:    "Stop compute before recovering the authoritative ledger",
			release: "Let node stops hand over again now that the ledger is recovered",
		},
	} {
		tasks := readRoleTasks(t, site.file)
		request, stop, release := taskIndex(tasks, site.request), taskIndex(tasks, site.stop),
			taskIndex(tasks, site.release)
		if request < 0 || stop < 0 || release < 0 || request >= stop || stop >= release {
			t.Errorf("%s: %q, %q and %q are at %d, %d and %d; want request, stop, release in order",
				site.file, site.request, site.stop, site.release, request, stop, release)

			continue
		}
		if tasks[request].Include != "request-node-drain.yml" || tasks[release].Include != "release-node-drain.yml" {
			t.Errorf("%s: the request includes %q and the release %q", site.file,
				tasks[request].Include, tasks[release].Include)
		}

		want, got := tasks[stop].conditions(), tasks[request].conditions()
		if site.sameConditions && !slices.Equal(got, want) {
			t.Errorf("%s: the request is guarded by %q, the stop by %q", site.file, got, want)
		}
		for _, condition := range want {
			// THE RESTART'S "SOMETHING CHANGED" IS NARROWED, on purpose, to the
			// config changing; every other condition the stop has, the request has.
			// And the request covers the endpoint migration's stop as well as the
			// ordinary restart, so it is not limited to an unmigrated node.
			if !site.sameConditions && (strings.Contains(condition, "billet_config_install.changed") ||
				strings.Contains(condition, "billet_endpoint_migrated")) {
				continue
			}
			if !slices.Contains(got, condition) {
				t.Errorf("%s: the stop is guarded by %q and its request is not", site.file, condition)
			}
		}
	}

	services := readRoleTasks(t, "services.yml")

	// A CHANGED CONFIG'S REQUEST COMES BEFORE THE ENDPOINT MIGRATION, which stops
	// and starts the node in place of the ordinary restart.
	if request, migration := taskIndex(services, "Make a node restart for a changed config a drain"),
		taskIndex(services, "Migrate the node's endpoint before the ordinary restart"); request < 0 ||
		migration < 0 || request >= migration {
		t.Errorf("the changed config's request (%d) does not precede the endpoint migration (%d)",
			request, migration)
	}

	// THE TRANSACTION'S STOP, inside its block, for a node that will not run again
	// or whose candidate config differs from the installed one, compared first.
	at := taskIndex(services, "Upgrade billet as one recoverable host transaction")
	if at < 0 {
		t.Fatal("services.yml has no upgrade transaction")
	}
	compare := taskIndex(services, "Compare the transaction's candidate config with the installed one")
	block := services[at].Block
	request := taskIndex(block, "Make the transaction's node stop a drain when the node will not run again or its config changes")
	stop := taskIndex(block, "Gracefully stop the billet node before changing its guest contract")
	release := taskIndex(services, "Withdraw the drain request now that the node is where this converge leaves it")
	if compare < 0 || compare >= at || request < 0 || stop < 0 || request >= stop || release <= at {
		t.Errorf("the transaction's comparison, request, stop and the release after it are at %d, %d, %d and %d "+
			"(block at %d)", compare, request, stop, release, at)
	}
	if request >= 0 {
		guard := strings.Join(block[request].conditions(), " ")
		for _, want := range []string{"not billet_node_should_run", "billet_candidate_config_compare.rc"} {
			if !strings.Contains(guard, want) {
				t.Errorf("the transaction's request is guarded by %q, without %q", guard, want)
			}
		}
	}
}
