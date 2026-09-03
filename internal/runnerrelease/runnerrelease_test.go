package runnerrelease

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// THE PIN IS ONE VALUE, and the guest image build script reads the same file.
//
// It was a Go constant in `billet ami` and a shell default in the image script,
// which made bumping the runner two edits in two languages. Doing one of them
// produces a fleet where the ec2 backend is current and the microVM backend is not,
// or the reverse — discovered on the day GitHub stops queueing to the stale half.
func TestThePinnedRunnerVersionLooksLikeAVersion(t *testing.T) {
	t.Parallel()

	got := Pinned()

	if got == "" {
		t.Fatal("nothing is pinned, so a build has no runner to install")
	}

	if strings.HasPrefix(got, "v") {
		t.Errorf("the pin is %q; it is used to build a download URL that already has its own "+
			"`v`, so this would ask for v v2.x", got)
	}

	if strings.ContainsAny(got, " \t\r\n") {
		t.Errorf("the pin %q carries whitespace, and it is interpolated into a URL and a "+
			"filename", got)
	}

	if strings.Count(got, ".") != 2 {
		t.Errorf("the pin %q is not a three-part release like 2.328.0", got)
	}
}

// THE WARNING COMES BEFORE THE DEADLINE, WITH ROOM TO ACT.
//
// What this warns about is not a click: it is building an image, proving it boots
// and registers, and rolling a fleet onto it. A warning that arrived the day before
// would be an alarm about something already too late to do calmly.
func TestTheWarningLeavesTimeToActOnIt(t *testing.T) {
	t.Parallel()

	if Warn >= Grace {
		t.Fatalf("the warning at %v is not before the deadline at %v", Warn, Grace)
	}

	if left := Grace - Warn; left < 7*24*time.Hour {
		t.Errorf("the warning leaves %v to rebuild, verify and roll a fleet", left)
	}
}

// writeOrFail writes a fixture response and fails the test if it cannot, so a
// broken fixture is reported as a broken fixture rather than as a broken subject.
func writeOrFail(t *testing.T, w http.ResponseWriter, body []byte) {
	t.Helper()

	if _, err := w.Write(body); err != nil {
		t.Errorf("write the fixture response: %v", err)
	}
}
