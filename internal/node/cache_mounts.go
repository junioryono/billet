package node

import (
	"context"
	"time"

	storecontract "github.com/junioryono/billet/internal/store"
)

// restoreMountLimit bounds asking about and remounting one recovered volume. A
// mount that outlasts it is could-not-tell, and the volume is refused.
const restoreMountLimit = 30 * time.Second

// RestoreMounts mounts again, in this process, every volume a recovered session
// records as mounted, before anything serves from their paths (#374).
//
// A RECORD SAYS WHAT A PREVIOUS PROCESS MOUNTED, NOT WHAT IS MOUNTED HERE. The
// node's unit runs in a mount namespace of its own, so the mounts a process made
// die with it while the guests it handed over keep running and keep asking. Read
// as current, the record served those guests the empty directory beneath each
// mount point and wrote what they sent into the host's state directory.
//
// A volume that is already mounted here is left alone; one that is not is
// mounted from the device it is mapped to now, the way it was first mounted. One
// that cannot be asked about or mounted is marked lost for this process and refused like a
// volume that was never mounted, which every cache treats as a miss; the record
// is not changed, so cleanup and publication, which work from the device, and
// the next process, which tries again, see what was there. A closed session is
// skipped: it serves nothing, and its cleanup unmounts by path.
func (s *CacheService) RestoreMounts(ctx context.Context) {
	s.mu.Lock()
	sessions := make([]*cacheSession, 0, len(s.byToken))
	for _, session := range s.byToken {
		sessions = append(sessions, session)
	}
	s.mu.Unlock()

	for _, session := range sessions {
		s.restoreSessionMounts(ctx, session)
	}
}

func (s *CacheService) restoreSessionMounts(ctx context.Context, session *cacheSession) {
	session.mu.Lock()
	defer session.mu.Unlock()

	if session.closed {
		return
	}

	for kind, hv := range session.hosts {
		// A DIRECTORY ON THE NODE'S STATE DISK IS NOT A MOUNT, and is still there, with
		// what the job has already spent of its allowance in it.
		if hv.Mounted && hv.Objects {
			hv.written.Store(s.writtenBytes(s.casMountPath(session, kind)))

			continue
		}
		if !hv.Mounted {
			continue
		}
		if err := s.ensureMounted(ctx, s.actionIO.MountWritable, hv.Volume,
			s.casMountPath(session, kind)); err != nil {
			hv.lost = true
			s.log.Warn("a recovered cache volume could not be mounted again; the job's "+
				"cache of this kind is a miss from now on", "instance", session.instance,
				"kind", kind, "error", err)
		}
	}

	for _, archive := range session.actions {
		if archive.Unmounted || archive.objectBacked() {
			continue
		}
		mount := s.actionIO.MountWritable
		if archive.Mode == actionsModeDownload {
			mount = s.actionIO.MountReadOnly
		}
		if err := s.ensureMounted(ctx, mount, archive.Volume,
			s.actionsMountPath(session, archive)); err != nil {
			archive.lost = true
			s.log.Warn("a recovered Actions cache archive could not be mounted again; it is "+
				"answered as absent", "instance", session.instance, "mode", archive.Mode,
				"error", err)
		}
	}
}

// ensureMounted mounts volume at target unless something already is, asking
// first so a mount this process made is never stacked on.
func (s *CacheService) ensureMounted(
	ctx context.Context,
	mount func(ctx context.Context, device, target string) error,
	volume storecontract.Volume,
	target string,
) error {
	ctx, cancel := context.WithTimeout(ctx, restoreMountLimit)
	defer cancel()

	mounted, err := s.actionIO.Mounted(ctx, target)
	if err != nil {
		return err
	}
	if mounted {
		return nil
	}
	device, err := s.provenDevice(ctx, volume)
	if err != nil {
		return err
	}

	return mount(ctx, device, target)
}

// provenDevice is the device volume is mapped to now. A recorded number outlives
// its mapping across a reboot, and the kernel may have given it to another job's
// disk, which mounted here would be served or merged from as this job's.
// detachProofLimit bounds proving a volume's device before its detach.
const detachProofLimit = 5 * time.Second

// detachDevice is what a detach is handed for volume: the device as the store proves it
// now, or none. A Firecracker detach writes that device back before it replaces the
// drive, and a recorded name the kernel may since have given another volume must not be
// the one written back; with none it leaves the write-back to the VMM's close. An EC2
// detach is handed the volume's id, which its store returns unproved and unchanged.
//
// ON A FEW SECONDS OF ITS OWN: a store that cannot find the device here asks over the
// network, and a slow answer costs the write-back, never the detach's time.
func (s *CacheService) detachDevice(ctx context.Context, volume storecontract.Volume) string {
	proofCtx, cancel := context.WithTimeout(ctx, detachProofLimit)
	defer cancel()
	device, err := s.provenDevice(proofCtx, volume)
	if err != nil {
		return ""
	}

	return device
}

func (s *CacheService) provenDevice(ctx context.Context, volume storecontract.Volume) (string, error) {
	prover, ok := s.store.(storecontract.DeviceProver)
	if !ok {
		return volume.Device, nil
	}

	return prover.MappedDevice(ctx, volume)
}
