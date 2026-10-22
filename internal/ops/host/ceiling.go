package host

import (
	"fmt"

	"github.com/junioryono/billet/internal/config"
)

// CeilingBelow is the sentence saying a deployment ceiling is below what the
// hosts in scope contribute between them, or "" when the ceiling covers them.
//
// THE ONE STATEMENT OF THE COMPARISON, for `billet status` (the live, undrained
// hosts: what runs now) and `billet check` (every registered host: what the
// configuration is sized for). The deployment ceiling caps every node, so a
// ceiling below the hosts' sum is room the hosts have and the allocator never
// hands out, and a node added without raising it registers, advertises and
// quietly gets none of its own capacity. Nothing else ever says so.
func CeilingBelow(scope string, hosts, vcpu int, memory config.ByteSize, server *config.ServerConfig) string {
	if vcpu <= server.MaxVCPU && memory <= server.MaxMemory {
		return ""
	}

	return fmt.Sprintf("BELOW THE HOSTS: the %d %s hosts contribute %d vCPU and %s, and "+
		"server.max_vcpu / max_memory allow %d and %s, so the ceiling, not the hosts, decides "+
		"what runs; raise the ceiling to the hosts' sum (`billet join accept` does it for a "+
		"joining node), or cap a host with node.max_vcpu / max_memory",
		hosts, scope, vcpu, memory, server.MaxVCPU, server.MaxMemory)
}
