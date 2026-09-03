package initconfig

import (
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/config"
)

// THE PLANNER'S WHOLE JOB IS SAYING NO. Each case here is one operator edit a
// converge would destroy; every one must land beside. The single yes — the
// canonical-equality case, including the App identity `github-app create`
// fills in — is what makes re-running init safe at all.
func TestPlanReRunTable(t *testing.T) {
	fresh, _, err := Generate(dockerParams())
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	mutate := func(from, to string) string {
		if !strings.Contains(fresh, from) {
			t.Fatalf("the fixture does not contain %q; the mutation would be vacuous", from)
		}

		return strings.Replace(fresh, from, to, 1)
	}

	cases := map[string]struct {
		existing string
		want     ReRun
	}{
		"pristine": {fresh, Regenerate},
		"ceiling lowered": {
			mutate("max_vcpu: ", "max_vcpu: 1 # capped\n  ignored_was_vcpu: "), WriteBeside},
		"image pinned": {
			mutate("ghcr.io/actions/actions-runner:latest",
				"ghcr.io/actions/actions-runner@sha256:0000000000000000000000000000000000000000000000000000000000000000"),
			WriteBeside},
		"comment added": {
			mutate("server:", "# capped deliberately, see ops ticket 42\nserver:"), WriteBeside},
		"workflow added": {
			mutate("workflows:", "workflows:\n      - acme/other/.github/workflows/x.yml@refs/heads/main"),
			WriteBeside},
		"site appended": {fresh + "\nsites:\n  - name: garage\n    store: ceph\n", WriteBeside},
		"not yaml":      {"just a string", WriteBeside},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := PlanReRun([]byte(tc.existing), fresh); got != tc.want {
				t.Errorf("PlanReRun = %v, want %v", got, tc.want)
			}
		})
	}
}

// THE ONE TOLERATED DIFFERENCE: the App identity, exactly as `github-app
// create`'s own rewriter produces it — the flow every onboarded deployment
// went through.
func TestPlanReRunToleratesOnlyTheAppIdentity(t *testing.T) {
	fresh, _, err := Generate(dockerParams())
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	// The same edits github-app create makes: real ids, a client_id line.
	onboarded := strings.Replace(fresh, "app_id: 0", "app_id: 7", 1)
	onboarded = strings.Replace(onboarded, "installation_id: 0", "installation_id: 42", 1)
	onboarded = strings.Replace(onboarded, "installation_id: 42",
		"installation_id: 42\n  client_id: Iv1.abc", 1)

	if got := PlanReRun([]byte(onboarded), fresh); got != Regenerate {
		t.Errorf("an onboarded but otherwise pristine config did not converge: %v", got)
	}

	// But identity plus ANY other edit still goes beside.
	edited := strings.Replace(onboarded, "max_vcpu: ", "max_vcpu: 1 # capped\n  was: ", 1)
	if got := PlanReRun([]byte(edited), fresh); got != WriteBeside {
		t.Errorf("an identity-plus-edit config converged: %v", got)
	}
}

// A MACHINE THAT CHANGED SIZE IS INDISTINGUISHABLE FROM AN EDIT, so it goes
// beside — the conservative side, by design.
func TestPlanReRunSendsAChangedMachineBeside(t *testing.T) {
	p := dockerParams()
	fresh, _, err := Generate(p)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	p.VCPU *= 2
	p.Memory *= 2
	bigger, _, err := Generate(p)
	if err != nil {
		t.Fatalf("generate bigger: %v", err)
	}

	if got := PlanReRun([]byte(fresh), bigger); got != WriteBeside {
		t.Errorf("a capacity change converged: %v", got)
	}
}

// The lenient state-dir read: absent KEY reports the config layer's default
// (a live deployment that omitted the key keeps its state exactly there);
// absent SECTION reports nothing to protect; non-YAML reports NOT-ok so the
// caller fails closed.
func TestExistingServerStateDir(t *testing.T) {
	if dir, ok := ExistingServerStateDir([]byte("server:\n  state_dir: /var/lib/billet/server\n")); !ok ||
		dir != "/var/lib/billet/server" {
		t.Errorf("explicit dir: %q ok=%v", dir, ok)
	}

	if dir, ok := ExistingServerStateDir([]byte("server:\n  listen: 127.0.0.1:7717\n")); !ok ||
		dir != config.DefaultServerStateDir() {
		t.Errorf("absent key must report the config default, got %q ok=%v", dir, ok)
	}

	if dir, ok := ExistingServerStateDir([]byte("node:\n  provider: docker\n")); !ok || dir != "" {
		t.Errorf("absent section: %q ok=%v", dir, ok)
	}

	if _, ok := ExistingServerStateDir([]byte("just a string")); ok {
		t.Error("non-YAML reported ok; the caller would fail open")
	}
}

// A COMMENT ON client_id IS OPERATOR CONTENT: canonicalization must not erase
// it into equality, or converge re-adds the line without the note.
func TestPlanReRunKeepsACommentedClientID(t *testing.T) {
	fresh, _, err := Generate(dockerParams())
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	onboarded := strings.Replace(fresh, "installation_id: 0",
		"installation_id: 0\n  client_id: Iv1.abc # rotated 2026-08, see ops note", 1)

	if got := PlanReRun([]byte(onboarded), fresh); got != WriteBeside {
		t.Errorf("a commented client_id was canonicalized away: %v", got)
	}
}
