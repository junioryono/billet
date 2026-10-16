package firecracker

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// snapshotFiles names a snapshot's two files as the VMM sees them, inside its chroot.
type snapshotFiles struct {
	// State is the microVM's device and vCPU state, a few tens of kilobytes.
	State string
	// Memory is the guest's memory, as large as the machine it was taken from.
	Memory string
}

// errNotRestorable marks a VMM whose state after a load is not the paused microVM
// the load was asked for, so nothing more may be done to it but destroy it.
var errNotRestorable = errors.New("the vmm is not the paused microVM it was asked to load")

// capture pauses a running microVM and writes a Full snapshot of it.
//
// THE VM IS LEFT PAUSED, whatever happens after the pause. Whether it ever runs
// again is the caller's decision (ADR-017): a golden's source is destroyed without
// resuming, because a state executed twice duplicates everything in it, and a
// suspension resumes its original only after it has durably invalidated the
// generation this wrote.
func capture(ctx context.Context, api *vmmAPI, files snapshotFiles) error {
	if err := api.patch(ctx, "/vm", map[string]string{"state": statePaused}); err != nil {
		return fmt.Errorf("firecracker: pause before a snapshot: %w", err)
	}

	if err := api.withTimeout(snapshotTimeout).put(ctx, "/snapshot/create", map[string]string{
		"snapshot_type": "Full",
		"snapshot_path": files.State,
		"mem_file_path": files.Memory,
	}); err != nil {
		return fmt.Errorf("firecracker: write a snapshot to %s: %w", files.State, err)
	}

	return nil
}

// snapshotTimeout bounds writing one snapshot.
//
// Measured on the reference node: 4.0 s for a 2 GiB guest. The time grows with the
// guest's memory and the disk under the files, so this is a bound on a wedged VMM
// rather than an estimate of a slow one.
const snapshotTimeout = 10 * time.Minute

// restoreSpec is what one restore of a snapshot needs that the snapshot itself
// does not carry.
type restoreSpec struct {
	// ID is the jail id the VMM was started under, which it reports as its own.
	ID string
	// Files are the snapshot to load, as paths inside the chroot.
	Files snapshotFiles
	// Tap is the host tap the guest's interface is attached to on this restore.
	Tap string
	// Vsock, when set, is the host socket path for the guest's vsock device.
	Vsock string
	// Metadata is the MMDS document this restore's guest reads.
	Metadata map[string]any
	// ClockRealtime advances the guest's clock by the time since capture, which
	// corrects a guest that keeps time on kvm-clock and changes nothing on one that
	// keeps it on tsc (measured, docs/reference/records/workspace-snapshots.md).
	ClockRealtime bool
}

// restore loads a snapshot into a fresh VMM, gives it this restore's metadata, and
// only then lets the guest run.
//
// THE ORDER IS THE SAFETY CONTENT. The load is always paused: MMDS's data store
// does not survive a snapshot (measured: `{}` after a paused load), so a guest
// resumed by the load itself would run before its credentials and identity were
// there to read. Logging and metrics are not configured here at all, because
// Firecracker refuses both once a snapshot is loaded (measured: 400, "not supported
// after starting the microVM"); the jailer's --log-path sets them when the VMM
// starts, which is before any load.
//
// AND THE VMM IS CHECKED BETWEEN THE LOAD AND THE METADATA. A VMM that reports any
// state but Paused, or any id but the one it was started under, is not the microVM
// this restore loaded, and putting a credential into it would hand the credential to
// whatever it is. It is refused with errNotRestorable and never resumed.
func restore(ctx context.Context, api *vmmAPI, spec restoreSpec) error {
	load := map[string]any{
		"snapshot_path": spec.Files.State,
		"mem_backend": map[string]string{
			"backend_path": spec.Files.Memory,
			"backend_type": "File",
		},
		"resume_vm": false,
		"network_overrides": []map[string]string{
			{"iface_id": guestInterface, "host_dev_name": spec.Tap},
		},
		"clock_realtime": spec.ClockRealtime,
	}

	if spec.Vsock != "" {
		load["vsock_override"] = map[string]string{"uds_path": spec.Vsock}
	}

	if err := api.put(ctx, "/snapshot/load", load); err != nil {
		return fmt.Errorf("firecracker: load the snapshot %s into %s: %w", spec.Files.State, spec.ID, err)
	}

	info, err := api.info(ctx)
	if err != nil {
		return fmt.Errorf("firecracker: %s did not report its state after a load: %w", spec.ID, err)
	}

	if info.State != statePaused || info.ID != spec.ID {
		return fmt.Errorf("firecracker: %w: asked to load %s paused, and it reports %s in state %s",
			errNotRestorable, spec.ID, bounded(info.ID), bounded(info.State))
	}

	if err := api.put(ctx, "/mmds", spec.Metadata); err != nil {
		return fmt.Errorf("firecracker: place the metadata of %s before it runs: %w", spec.ID, err)
	}

	if err := api.patch(ctx, "/vm", map[string]string{"state": stateResumed}); err != nil {
		return fmt.Errorf("firecracker: resume %s after placing its metadata: %w", spec.ID, err)
	}

	// THE SAME CONFIRMATION A LAUNCH ENDS ON: a VMM that accepted the resume can
	// still not be running, and nothing before this would have noticed.
	info, err = api.info(ctx)
	if err != nil {
		return fmt.Errorf("firecracker: %s did not report its state after being resumed: %w", spec.ID, err)
	}

	if info.State != stateRunning {
		return fmt.Errorf("firecracker: %s was resumed and reports state %s rather than %s",
			spec.ID, bounded(info.State), stateRunning)
	}

	return nil
}

// The states a VMM accepts through PATCH /vm, and the one it then reports.
const (
	statePaused  = "Paused"
	stateResumed = "Resumed"
)
