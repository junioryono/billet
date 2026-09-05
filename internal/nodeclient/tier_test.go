package nodeclient

import (
	"testing"

	"github.com/junioryono/billet/internal/nodeapi"
)

// The tier travels only on a wire that knows the field. The plane decodes
// with DisallowUnknownFields, so a node that negotiated an older version (a
// rollback, or a node upgraded ahead of its controller) would otherwise have
// every trusted launch refused as a malformed body.
func TestTheTierIsSentOnlyFromTheVersionThatAddedIt(t *testing.T) {
	t.Parallel()

	below := trustedRunnerGroupRequest(nodeapi.VersionTargetedRunnerGroup-1, "billet-4vcpu",
		"billet-trusted", []string{"acme/repo/.github/workflows/ci.yml@refs/heads/main"})
	if below.Tier != "" {
		t.Errorf("a wire below %d carried tier %q", nodeapi.VersionTargetedRunnerGroup, below.Tier)
	}

	if below.Group != "billet-trusted" || len(below.Workflows) != 1 {
		t.Errorf("the older wire's request lost its group or workflows: %+v", below)
	}

	at := trustedRunnerGroupRequest(nodeapi.VersionTargetedRunnerGroup, "billet-4vcpu",
		"billet-trusted", nil)
	if at.Tier != "billet-4vcpu" {
		t.Errorf("wire %d did not carry the tier: %+v", nodeapi.VersionTargetedRunnerGroup, at)
	}

	if above := trustedRunnerGroupRequest(nodeapi.Version, "billet-4vcpu", "g", nil); above.Tier != "billet-4vcpu" {
		t.Errorf("the current wire did not carry the tier: %+v", above)
	}
}
