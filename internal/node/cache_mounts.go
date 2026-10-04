package node

import (
	"context"
)

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
// mounted from its recorded device the way it was first mounted. One that cannot
// be asked about or mounted is marked lost for this process and refused like a
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
		if !hv.Mounted {
			continue
		}
		if err := s.ensureMounted(ctx, s.actionIO.MountWritable, hv.Volume.Device,
			s.casMountPath(session, kind)); err != nil {
			hv.lost = true
			s.log.Warn("a recovered cache volume could not be mounted again; the job's "+
				"cache of this kind is a miss from now on", "instance", session.instance,
				"kind", kind, "error", err)
		}
	}

	for _, archive := range session.actions {
		if archive.Unmounted {
			continue
		}
		mount := s.actionIO.MountWritable
		if archive.Mode == actionsModeDownload {
			mount = s.actionIO.MountReadOnly
		}
		if err := s.ensureMounted(ctx, mount, archive.Volume.Device,
			s.actionsMountPath(session, archive)); err != nil {
			archive.lost = true
			s.log.Warn("a recovered Actions cache archive could not be mounted again; it is "+
				"answered as absent", "instance", session.instance, "mode", archive.Mode,
				"error", err)
		}
	}
}

// ensureMounted mounts device at target unless something already is, asking
// first so a mount this process made is never stacked on.
func (s *CacheService) ensureMounted(
	ctx context.Context,
	mount func(ctx context.Context, device, target string) error,
	device, target string,
) error {
	mounted, err := s.actionIO.Mounted(ctx, target)
	if err != nil {
		return err
	}
	if mounted {
		return nil
	}

	return mount(ctx, device, target)
}
