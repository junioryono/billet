package config

import (
	"slices"
	"strings"
	"testing"
)

// THE SIMULATED BACKEND CANNOT BE NAMED IN A FILE, ANYWHERE A FILE COULD NAME ONE.
//
// It fabricates completions, so a deployment that reached it through a config
// would report every job finished and run none. Each site is refused on its own
// so the operator is sent to the line that names it.
func TestTheSimulatedBackendIsRefusedEverywhereAFileCouldNameIt(t *testing.T) {
	const clause = "cannot be named in a configuration"

	for name, body := range map[string]string{
		"node.provider": strings.Replace(validConfig,
			"  provider: firecracker\n  state_dir", "  provider: simulated\n  state_dir", 1),
		"tiers[].provider": strings.Replace(validConfig,
			"    provider: firecracker\n    vcpu: 8", "    provider: simulated\n    vcpu: 8", 1),
		"tiers[].providers": strings.Replace(validConfig,
			"    provider: firecracker\n    vcpu: 8", "    providers: [firecracker, simulated]\n    vcpu: 8", 1),
		"tiers[].launch": strings.Replace(validConfig,
			"    provider: firecracker\n    vcpu: 8\n    memory: 32GiB\n    shm: 1GiB\n    image: ubuntu-2404-x64\n",
			"    providers: [firecracker, docker]\n    vcpu: 8\n    memory: 32GiB\n    shm: 1GiB\n"+
				"    launch:\n      firecracker: {image: ubuntu-2404-x64}\n"+
				"      docker: {image: ghcr.io/actions/actions-runner:latest}\n"+
				"      simulated: {image: simulated}\n", 1),
		"nodes[].provider": validConfig + nodesSection("    provider: simulated\n"),
	} {
		t.Run(name, func(t *testing.T) {
			if !strings.Contains(body, "simulated") {
				t.Fatal("the substitution matched nothing, so this case would test the valid config")
			}

			_, err := Load(writeConfig(t, body))
			if err == nil {
				t.Fatal("Load accepted a configuration naming the simulated backend")
			}

			if !strings.Contains(err.Error(), clause) {
				t.Errorf("the refusal does not say why the backend cannot be named: %v", err)
			}

			// And it is refused as WHAT IT IS, not as a typo: the kind is in the closed
			// set so a test can register a simulated host, and an "is not one of"
			// diagnostic would send the operator to check their spelling.
			if strings.Contains(err.Error(), "is not one of") {
				t.Errorf("the simulated backend was reported as an unknown provider: %v", err)
			}
		})
	}
}

// The kind is IMPLEMENTED, and the ledger classifies it as host-backed.
//
// Valid is what alloc.New and RegisterNode consult, so a simulated host can join a
// test fleet; RunsOnHost decides that it is charged the tier request rather than
// a purchased shape and that custody reads its inventory as causal; and
// RemoteProviders, derived from RunsOnHost, must not list it, because that list
// becomes a cost query.
func TestTheSimulatedBackendIsAValidHostBackedKind(t *testing.T) {
	t.Parallel()

	if !ProviderSimulated.Valid() {
		t.Error("the simulated kind is not valid, so no test could register a simulated host")
	}

	if !ProviderSimulated.RunsOnHost() {
		t.Error("the simulated kind is classified as remote, so it would be expected to declare " +
			"a shape catalogue it does not have")
	}

	if !ProviderSimulated.TestOnly() {
		t.Error("the simulated kind is not test-only, so nothing would refuse it in a file")
	}

	if slices.Contains(RemoteProviders(), ProviderSimulated) {
		t.Errorf("RemoteProviders lists the simulated kind: %v", RemoteProviders())
	}

	for _, p := range []ProviderKind{
		ProviderDocker, ProviderFirecracker, ProviderTart, ProviderEC2, ProviderCodeBuild,
	} {
		if p.TestOnly() {
			t.Errorf("%s is reported as test-only, so a real deployment naming it would be refused", p)
		}
	}
}
