//go:build unix && !linux && !darwin && !freebsd

package state

// searchOnlyFlag reports that this platform has no search-only directory open
// that billet knows about, so the caller falls back to O_RDONLY.
//
// Zero rather than a guess. A wrong flag value would either fail every open or,
// worse, request something unintended — and the fallback merely reinstates the
// requirement that a shared lock directory be readable, which is a documented
// contract rather than a broken one.
func searchOnlyFlag() int { return 0 }
