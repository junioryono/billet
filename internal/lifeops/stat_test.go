package lifeops

import "syscall"

// statFor builds this platform's own stat structure for a fixture.
//
// THE FIELD WIDTHS DIFFER BY PLATFORM AND BY ARCHITECTURE: Nlink is uint64 on
// linux/amd64, uint32 on linux/arm64 and uint16 on darwin, and Dev is signed on
// darwin. billet cross-builds all three, so a fixture that wrote a literal of
// any one width would compile on the machine it was written on and break the
// build somewhere else. Assigning through a converting helper needs no build
// tags and cannot drift.
func statFor(uid uint32, links, dev uint64) *syscall.Stat_t {
	st := &syscall.Stat_t{Uid: uid}
	assign(&st.Nlink, links)
	assign(&st.Dev, dev)

	return st
}

func assign[T ~uint16 | ~uint32 | ~uint64 | ~int32 | ~int64](dst *T, v uint64) {
	*dst = T(v)
}
