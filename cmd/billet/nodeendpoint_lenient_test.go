package main

import (
	"strings"
	"testing"
)

// A DRY RUN DESCRIBES A CONFIGURATION THE LOADER REFUSES; THE ACTION DOES NOT.
// An operator's first emission carries a `backup:` block (or GitHub App ids of
// zero) the loader refuses at load by design, and the role's own `billet
// check` is what judges that at converge, not the endpoint decision's dry run
// in a check-mode converge over a fresh host. So the dry run parses the node
// section alone and reports; the action, which stops and starts on the
// configuration, keeps the loader's whole judgement; and a node section that
// is itself invalid refuses in both.
func TestADryRunParsesTheNodeSectionAloneAndTheActionTheWhole(t *testing.T) {
	// A backup block on a node-only configuration is the loader's refusal
	// ("backup: is set but this config declares no server") with a valid
	// node section.
	invalid := func(f *endpointFixture, addr string) string {
		return f.rendering(addr) + "backup:\n  s3:\n    bucket: b\n    region: us-west-2\n"
	}

	t.Run("the dry run reports over an installed configuration the loader refuses", func(t *testing.T) {
		f := newEndpointFixture(t)
		f.writeConfig(t, invalid(f, endpointA))

		o := f.migrate(t, f.rendering(endpointB), "--dry-run")
		mustEndpointOutcome(t, o, outcomeReported)

		if !o.boolean("planned") || o.str("from") != canonicalA || o.str("to") != canonicalB {
			t.Errorf("reported %v", o.doc)
		}
	})

	t.Run("the dry run reports over a rendering the loader refuses", func(t *testing.T) {
		f := newEndpointFixture(t)

		o := f.migrate(t, invalid(f, endpointB), "--dry-run")
		mustEndpointOutcome(t, o, outcomeReported)

		if !o.boolean("planned") || o.str("to") != canonicalB {
			t.Errorf("reported %v", o.doc)
		}
	})

	t.Run("the action refuses a rendering the loader refuses", func(t *testing.T) {
		f := newEndpointFixture(t)
		f.writeConfig(t, invalid(f, endpointB))

		o := f.migrate(t, invalid(f, endpointB))
		mustEndpointRefusal(t, o, outcomeRefused, endpointReasonConfig)

		if !strings.Contains(o.str("why"), "backup:") {
			t.Errorf("why %q", o.str("why"))
		}

		if f.calls(t, "stop") != 0 {
			t.Errorf("the action stopped the node over a configuration the loader refuses: %v", f.systemctlCalls(t))
		}
	})

	t.Run("an invalid node section refuses the dry run too", func(t *testing.T) {
		f := newEndpointFixture(t)
		bad := strings.Replace(f.rendering(endpointB), "provider: docker\n", "provider: teleporter\n", 1)

		o := f.migrate(t, bad, "--dry-run")
		mustEndpointRefusal(t, o, outcomeRefused, endpointReasonDesired)

		if !strings.Contains(o.str("why"), "node.provider") {
			t.Errorf("why %q", o.str("why"))
		}
	})

	t.Run("the refresh's dry run reports over a rendering the loader refuses", func(t *testing.T) {
		f := newReceiptCmdFixture(t)
		f.migrated(t)

		o := f.refresh(t, invalid(f.endpointFixture, endpointB), "--dry-run")
		mustEndpointOutcome(t, o, outcomeReported)
		f.noReceipt(t)
	})

	t.Run("the refresh itself refuses a rendering the loader refuses", func(t *testing.T) {
		f := newReceiptCmdFixture(t)
		f.migrated(t)

		o := f.refresh(t, invalid(f.endpointFixture, endpointB))
		mustEndpointRefusal(t, o, outcomeRefused, endpointReasonDesired)
		f.noReceipt(t)
	})
}
