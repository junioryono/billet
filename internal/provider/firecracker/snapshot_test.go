package firecracker

import (
	"errors"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// aVMM serves one fake VMM and returns a client for it.
func aVMM(t *testing.T, id, state string) (*fakeVMM, *vmmAPI) {
	t.Helper()

	socket := filepath.Join(shortDir(t), "api.sock")
	vmm := startFakeVMM(t, socket, id)
	vmm.state = state

	return vmm, newVMMAPI(socket)
}

// restoredID is the jail id every restore here is started under.
const restoredID = "billet-ws-1"

func aRestore() restoreSpec {
	return restoreSpec{
		ID:       restoredID,
		Files:    snapshotFiles{State: "snap.state", Memory: "snap.mem"},
		Tap:      "bt-7",
		Vsock:    "v.sock",
		Metadata: map[string]any{"billet": map[string]any{"workspace": "ws-1"}},
	}
}

// THE GUEST NEVER RUNS BEFORE ITS METADATA IS THERE. MMDS's data store is empty
// after any load (measured), so a resume that came before the PUT /mmds would let
// the guest ask for its identity and get nothing, and a load that resumed by itself
// would do the same with no call billet could put in between.
func TestARestoreLoadsPausedAndPlacesMetadataBeforeItResumes(t *testing.T) {
	t.Parallel()

	vmm, api := aVMM(t, restoredID, "Not started")

	if err := restore(t.Context(), api, aRestore()); err != nil {
		t.Fatalf("restore: %v", err)
	}

	want := []string{"PUT /snapshot/load", "PUT /mmds", "PATCH /vm"}
	if got := vmm.calls(); !slices.Equal(got, want) {
		t.Fatalf("calls %v, want %v: the metadata must land while the guest is paused", got, want)
	}

	load, _ := vmm.bodyPut("/snapshot/load")
	if load["resume_vm"] != false {
		t.Errorf("the load asked to resume (%v), so the guest could run before its metadata", load["resume_vm"])
	}

	if load["snapshot_path"] != "snap.state" {
		t.Errorf("snapshot_path = %v, want snap.state: the api requires it", load["snapshot_path"])
	}

	// THE WIRE'S OWN WORD, written out rather than taken from billet's constant.
	if vm, _ := vmm.bodyPut("/vm"); vm["state"] != "Resumed" {
		t.Errorf("the last call set the run state to %v, want Resumed", vm["state"])
	}

	md, _ := vmm.bodyPut("/mmds")
	if want := map[string]any{"billet": map[string]any{"workspace": "ws-1"}}; !reflect.DeepEqual(md, want) {
		t.Errorf("the metadata document was %v, want exactly %v", md, want)
	}
}

// AND THE RESTORE ATTACHES THE TAP AND SOCKET IT IS GIVEN, not the golden's. The
// snapshot carries the tap name and socket path it was taken with, and two forks on
// one host cannot both have them (measured: network_overrides and vsock_override
// each worked on v1.16.1).
func TestARestoreOverridesTheTapAndTheSocketTheSnapshotCarries(t *testing.T) {
	t.Parallel()

	vmm, api := aVMM(t, restoredID, "Not started")

	spec := aRestore()
	spec.ClockRealtime = true

	if err := restore(t.Context(), api, spec); err != nil {
		t.Fatalf("restore: %v", err)
	}

	load, _ := vmm.bodyPut("/snapshot/load")

	overrides := field[[]any](t, load, "network_overrides")
	if len(overrides) != 1 {
		t.Fatalf("network_overrides = %v, want exactly one, for eth0", overrides)
	}

	o, isObject := overrides[0].(map[string]any)
	if !isObject || o["iface_id"] != guestInterface || o["host_dev_name"] != "bt-7" {
		t.Errorf("network override %v, want eth0 on bt-7", overrides[0])
	}

	if v := field[map[string]any](t, load, "vsock_override"); v["uds_path"] != "v.sock" {
		t.Errorf("vsock_override = %v, want v.sock", v)
	}

	if load["clock_realtime"] != true {
		t.Errorf("clock_realtime = %v, want true as asked", load["clock_realtime"])
	}

	if mem := field[map[string]any](t, load, "mem_backend"); mem["backend_path"] != "snap.mem" || mem["backend_type"] != "File" {
		t.Errorf("mem_backend = %v, want the File backend on snap.mem", mem)
	}
}

// field reads one key of a decoded request body as the type the request must
// have sent it as.
func field[T any](t *testing.T, body map[string]any, key string) T {
	t.Helper()

	v, isT := body[key].(T)
	if !isT {
		t.Fatalf("%s = %#v, which is not the %T the request must carry", key, body[key], v)
	}

	return v
}

// A RESTORE THAT CANNOT PLACE THE METADATA LEAVES THE GUEST PAUSED. Resuming it
// anyway would run a guest with no identity, which is the case the order exists
// to rule out.
func TestARestoreThatCannotPlaceMetadataNeverResumes(t *testing.T) {
	t.Parallel()

	vmm, api := aVMM(t, restoredID, "Not started")
	vmm.refuse["/mmds"] = "The MMDS data store is full"

	err := restore(t.Context(), api, aRestore())
	if err == nil {
		t.Fatal("restore reported success although the metadata was refused")
	}

	if !strings.Contains(err.Error(), "The MMDS data store is full") {
		t.Errorf("the vmm's reason was dropped: %v", err)
	}

	if slices.Contains(vmm.calls(), "PATCH /vm") {
		t.Errorf("the guest was resumed after its metadata was refused: calls %v", vmm.calls())
	}

	if got := vmm.currentState(); got != statePaused {
		t.Errorf("the vmm is %s, want it left %s", got, statePaused)
	}
}

// A VMM THAT IS NOT THE ONE THIS RESTORE LOADED IS GIVEN NOTHING. Its id comes from
// the jail it was started in, so a different one means the socket belongs to some
// other microVM, and the metadata carries this workspace's credentials.
func TestARestoreGivesNothingToAVMMWithAnotherID(t *testing.T) {
	t.Parallel()

	vmm, api := aVMM(t, "billet-someone-else", "Not started")

	err := restore(t.Context(), api, aRestore())
	if !errors.Is(err, errNotRestorable) {
		t.Fatalf("restore = %v, want errNotRestorable", err)
	}

	if got := vmm.calls(); !slices.Equal(got, []string{"PUT /snapshot/load"}) {
		t.Errorf("calls %v after the id mismatch, want the load alone: no metadata, no resume", got)
	}
}

// NOR TO ONE THAT IS ALREADY RUNNING after a load billet asked to be paused, which
// would mean the guest is executing with no metadata in it.
func TestARestoreGivesNothingToAVMMThatRunsAfterAPausedLoad(t *testing.T) {
	t.Parallel()

	vmm, api := aVMM(t, restoredID, "Not started")
	vmm.ignoreResumeFlag = true

	err := restore(t.Context(), api, aRestore())
	if !errors.Is(err, errNotRestorable) {
		t.Fatalf("restore = %v, want errNotRestorable", err)
	}

	if slices.Contains(vmm.calls(), "PUT /mmds") {
		t.Errorf("metadata was placed in a guest that was already running: calls %v", vmm.calls())
	}
}

// A CAPTURE PAUSES FIRST AND WRITES A FULL SNAPSHOT, and leaves the guest paused:
// running it on is the caller's decision, because a state that runs twice
// duplicates everything in it.
func TestACapturePausesThenWritesAFullSnapshotAndLeavesItPaused(t *testing.T) {
	t.Parallel()

	vmm, api := aVMM(t, "billet-gold-1", stateRunning)

	if err := capture(t.Context(), api, snapshotFiles{State: "g.state", Memory: "g.mem"}); err != nil {
		t.Fatalf("capture: %v", err)
	}

	if got, want := vmm.calls(), []string{"PATCH /vm", "PUT /snapshot/create"}; !slices.Equal(got, want) {
		t.Fatalf("calls %v, want %v", got, want)
	}

	create, _ := vmm.bodyPut("/snapshot/create")
	if create["snapshot_type"] != "Full" || create["snapshot_path"] != "g.state" || create["mem_file_path"] != "g.mem" {
		t.Errorf("snapshot/create = %v, want a Full snapshot to g.state and g.mem", create)
	}

	if got := vmm.currentState(); got != statePaused {
		t.Errorf("the vmm is %s after a capture, want it left %s", got, statePaused)
	}
}

// A CAPTURE THAT CANNOT PAUSE WRITES NOTHING. A snapshot of a running guest is not
// a state any restore could trust.
func TestACaptureThatCannotPauseWritesNoSnapshot(t *testing.T) {
	t.Parallel()

	vmm, api := aVMM(t, "billet-gold-1", stateRunning)
	vmm.refuse["/vm"] = "The microVM is not running"

	err := capture(t.Context(), api, snapshotFiles{State: "g.state", Memory: "g.mem"})
	if err == nil || !strings.Contains(err.Error(), "The microVM is not running") {
		t.Fatalf("capture = %v, want the vmm's refusal of the pause", err)
	}

	if slices.Contains(vmm.calls(), "PUT /snapshot/create") {
		t.Errorf("a snapshot was written although the pause was refused: calls %v", vmm.calls())
	}
}

// WRITING A SNAPSHOT OUTLASTS THE BOUND ON AN ORDINARY CALL. Firecracker answers
// only after it has written and fsynced the guest's memory, so a healthy capture of
// a large guest takes longer than any configuration call, and an http.Client's
// Timeout is not something a longer caller context can extend.
func TestACaptureIsNotCutShortByTheBoundOnAnOrdinaryCall(t *testing.T) {
	t.Parallel()

	vmm, api := aVMM(t, "billet-gold-1", stateRunning)
	vmm.createTakes = 300 * time.Millisecond
	api.http.Timeout = 100 * time.Millisecond // the ordinary bound, shrunk to below the write

	if err := capture(t.Context(), api, snapshotFiles{State: "g.state", Memory: "g.mem"}); err != nil {
		t.Fatalf("capture of a write that takes longer than an ordinary call: %v", err)
	}
}

// A RESUME THE VMM ACCEPTED IS NOT A GUEST THAT RUNS. The same confirmation a
// launch ends on: the state the VMM reports afterwards, not the answer to the call.
func TestARestoreWhoseGuestDoesNotRunAfterTheResumeFails(t *testing.T) {
	t.Parallel()

	vmm, api := aVMM(t, restoredID, "Not started")
	vmm.stayPausedOnResume = true

	err := restore(t.Context(), api, aRestore())
	if err == nil || !strings.Contains(err.Error(), "reports state \"Paused\" rather than Running") {
		t.Fatalf("restore = %v, want the refusal naming the state it found", err)
	}
}
