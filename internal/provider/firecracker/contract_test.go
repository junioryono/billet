package firecracker

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/junioryono/billet/internal/provider"
)

// THE GUEST CONTRACT IS WRITTEN IN TWO PLACES AND NOTHING ELSE CHECKS THEY
// AGREE.
//
// The host's number is GuestContract, in Go. The guest's is WANT_CONTRACT, a
// literal inside build-guest-image.sh's agent — embedded in a QUOTED heredoc, so
// no build step can interpolate the Go value into it and no compiler will ever
// see both. They are two independent constants that must be equal.
//
// The failure when they drift has no early symptom. The agent refuses a contract
// it does not recognise, so every microVM boots, reads the metadata, declines it,
// and never registers. The host sees VMs that start and never take a job; the
// guest has no console anybody reads. Nothing points at a number changed on one
// side of a heredoc.
//
// So the drift is caught here, at `go test`, by the only mechanism that can see
// both: reading the script.
func TestGuestContractMatchesTheAgentInTheBuildScript(t *testing.T) {
	path := filepath.Join("..", "..", "..", "scripts", "build-guest-image.sh")

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("could not read %s, which is where the guest half of the contract "+
			"is written: %v", path, err)
	}

	// ANCHORED TO A WHOLE LINE. The script mentions WANT_CONTRACT several times —
	// in a comparison and in a log line — and a loose pattern would match the
	// comparison's `$WANT_CONTRACT` and extract nothing, or match a comment.
	re := regexp.MustCompile(`(?m)^WANT_CONTRACT=(\d+)$`)

	matches := re.FindAllStringSubmatch(string(data), -1)
	if len(matches) == 0 {
		t.Fatalf("no `WANT_CONTRACT=<n>` assignment in %s; either the guest agent no "+
			"longer declares which contract it speaks, or it was renamed and this test "+
			"is now checking nothing", path)
	}

	// MORE THAN ONE ASSIGNMENT IS ITSELF THE BUG. Two agents, or a copy left
	// behind, means the image's behaviour depends on which one wins.
	if len(matches) > 1 {
		t.Fatalf("%s assigns WANT_CONTRACT %d times; the image speaks whichever the "+
			"agent evaluates last, which is not a thing anybody can reason about",
			path, len(matches))
	}

	if got := matches[0][1]; got != GuestContract {
		t.Fatalf("the host speaks guest contract %s and the agent baked into the image "+
			"speaks %s.\n\n"+
			"Nothing else catches this. The agent refuses a contract it does not "+
			"recognise, so every microVM would boot, read its metadata, decline it, and "+
			"never register -- with no console anybody reads and no error naming the "+
			"number.\n\n"+
			"Change both, and bump the manifest's guest_contract with them.",
			GuestContract, got)
	}
}

// THE METADATA PATH IS HARDCODED IN TWO PLACES AND NOTHING ELSE CHECKS THEY AGREE.
//
// metadata() builds the data store here, in Go. scripts/boot-guest-image.sh serves
// a stand-in payload in shell, so the boot gate can drive the agent to a refusal
// without a control plane. The guest agent fetches a fixed path, and all three
// have to describe the same tree.
//
// This was found the way these things are found: the boot gate served the payload
// flat, the guest 404'd on it, and the agent reported that billet had not said
// which contract it speaks -- which reads exactly like a broken image and was a
// broken test.
func TestMetadataNestsUnderThePathTheGuestFetches(t *testing.T) {
	m, err := metadata(provider.Spec{Name: "probe", JITConfig: "x", Command: []string{"./run.sh"}})
	if err != nil {
		t.Fatalf("metadata: %v", err)
	}

	// The agent fetches http://<mmds>/latest/meta-data/billet/<key>, so the store
	// must be nested exactly this way.
	latest, ok := m["latest"].(map[string]any)
	if !ok {
		t.Fatalf("no `latest` in the data store; the agent fetches /latest/meta-data/... "+
			"and would 404. Store: %v", m)
	}

	meta, ok := latest["meta-data"].(map[string]any)
	if !ok {
		t.Fatalf("no `meta-data` under `latest`; the agent would 404. Store: %v", m)
	}

	billet, ok := meta["billet"].(map[string]any)
	if !ok {
		t.Fatalf("no `billet` under `meta-data`; the agent would 404. Store: %v", m)
	}

	if got := billet["contract"]; got != GuestContract {
		t.Errorf("contract in the data store = %v, want %s", got, GuestContract)
	}

	// EVERY KEY THE AGENT READS. A renamed key produces a guest that boots, fetches,
	// finds nothing, and runs no job -- with no error naming the key.
	for _, key := range []string{"contract", "runner-name", "jit-config", "command"} {
		if _, present := billet[key]; !present {
			t.Errorf("the data store carries no %q, which the agent fetches by name", key)
		}
	}
}
