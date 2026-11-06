package macoslimit

import (
	"os"
	"strings"
	"testing"
	"time"
)

const (
	deploymentA = "0123456789abcdef0123456789abcdef"
	deploymentB = "fedcba9876543210fedcba9876543210"
)

func TestARecordAnswersOnlyForItsOwnDeployment(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	if got, ok, err := Read(dir, deploymentA); err != nil || ok || got != 0 {
		t.Fatalf("no record: %d, %v, %v", got, ok, err)
	}

	if err := Write(dir, deploymentA, true, 3, time.Now()); err != nil {
		t.Fatal(err)
	}

	if got, ok, err := Read(dir, deploymentA); err != nil || !ok || got != 3 {
		t.Fatalf("own deployment: %d, %v, %v", got, ok, err)
	}

	if _, ok, err := Read(dir, deploymentB); err != nil || ok {
		t.Fatalf("another deployment: %v, %v", ok, err)
	}
}

// A CONTROL PLANE THAT DOES NOT SAY IS NOT A LIMIT OF ZERO, which would refuse every macOS
// guest on a node whose control plane is merely older.
func TestAControlPlaneThatDoesNotSayIsNotZero(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	if err := Write(dir, deploymentA, false, 5, time.Now()); err != nil {
		t.Fatal(err)
	}

	if _, ok, err := Read(dir, deploymentA); err != nil || ok {
		t.Fatalf("Read = %v, %v, want no answer", ok, err)
	}

	raw, err := os.ReadFile(Path(dir))
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(string(raw), `"carried":false`) || !strings.Contains(string(raw), `"limit":0`) {
		t.Fatalf("recorded %s", raw)
	}
}

// A ZERO THE CONTROL PLANE SAID IS AN ANSWER: the node runs no macOS guest.
func TestAZeroItSaidIsAnAnswer(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	if err := Write(dir, deploymentA, true, 0, time.Now()); err != nil {
		t.Fatal(err)
	}

	if got, ok, err := Read(dir, deploymentA); err != nil || !ok || got != 0 {
		t.Fatalf("Read = %d, %v, %v", got, ok, err)
	}
}

// COULD NOT TELL IS NOT ABSENCE: a reader that fell back on Apple's default would count
// guests against a limit the control plane did not name.
func TestARecordThatCannotBeReadIsAnError(t *testing.T) {
	t.Parallel()

	for name, body := range map[string]string{
		"not JSON":     "{",
		"a new schema": `{"schema":2,"deployment":"` + deploymentA + `","carried":true,"limit":2}`,
	} {
		dir := t.TempDir()

		if err := os.WriteFile(Path(dir), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}

		if _, _, err := Read(dir, deploymentA); err == nil {
			t.Errorf("%s: read", name)
		}
	}
}

func TestWriteNeedsADeploymentAndRemoveToleratesAbsence(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	if err := Write(dir, "", true, 2, time.Now()); err == nil {
		t.Fatal("wrote a limit no deployment said")
	}

	if err := Remove(dir); err != nil {
		t.Fatal(err)
	}

	if err := Write(dir, deploymentA, true, 2, time.Now()); err != nil {
		t.Fatal(err)
	}

	if err := Remove(dir); err != nil {
		t.Fatal(err)
	}

	if _, ok, err := Read(dir, deploymentA); err != nil || ok {
		t.Fatalf("after Remove: %v, %v", ok, err)
	}
}
