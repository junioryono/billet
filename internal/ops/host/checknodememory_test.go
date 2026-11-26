package host

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeNodeMemoryConfig is a node-only host, docker so nothing is launched, with
// the given lines added to its node block.
func writeNodeMemoryConfig(t *testing.T, extra string) string {
	t.Helper()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "billet.yaml")
	body := fmt.Sprintf(`node:
  name: probe-node
  server_addr: 127.0.0.1:7717
  provider: docker
  state_dir: %s
%s`, filepath.Join(dir, "node"), extra)
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatalf("write the config: %v", err)
	}

	return cfgPath
}

// THE CHECK SAYS WHAT THE NODE WILL OFFER, AND FROM WHAT (#755): the machine, the
// declared max_memory and the reserve, resolved by the function the node itself
// starts with.
func TestTheCheckSaysWhatANodeOffersAndWhatItReserves(t *testing.T) {
	t.Setenv("BILLET_MAINTENANCE", "")

	cfgPath := writeNodeMemoryConfig(t, "  reserve_memory: 1GiB\n")

	var err error

	out := capture(t, func() {
		_, err = runCheck(t.Context(), processEnv(), checkOptions{configPath: cfgPath})
	})
	if err != nil {
		t.Fatalf("check: %v\n%s", err, out)
	}

	if !strings.Contains(out, "memory   offers ") ||
		!strings.Contains(out, " detected, node.max_memory unset, 1GiB reserved for the host\n") {
		t.Fatalf("the check does not say what the node offers and reserves:\n%s", out)
	}
}

// A RESERVE THAT LEAVES THE NODE NOTHING FAILS THE CHECK, for the reason the node
// would refuse to start: no machine this test runs on has a petabyte.
func TestTheCheckRefusesAReserveThatLeavesNothing(t *testing.T) {
	t.Setenv("BILLET_MAINTENANCE", "")

	cfgPath := writeNodeMemoryConfig(t, "  reserve_memory: 1024TiB\n")

	var err error

	out := capture(t, func() {
		_, err = runCheck(t.Context(), processEnv(), checkOptions{configPath: cfgPath})
	})
	if err == nil || !strings.Contains(err.Error(), "node memory: node.reserve_memory holds back") {
		t.Fatalf("check = %v, want the reserve refused:\n%s", err, out)
	}
}
