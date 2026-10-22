package firecracker

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// compatibilityFormat is the version of the compatibility record. A record of any
// other version is refused rather than read, because a field this binary does not
// know about is a condition it cannot check. Format 2 added the golden's system
// disk and the generation it was cloned from; a format 1 record names no disk, so
// nothing a restore could clone is known to be the one its memory was taken with.
// Format 3 added the evidence a restore is checked against (Evidence): the
// deployment, the builder's lease and the digests of the state and memory.
const compatibilityFormat = 3

// compatibilityFile is the record beside a published snapshot's state and memory.
const compatibilityFile = "compatibility.json"

// maxCompatibilityRecord bounds what is read of one, which is a few hundred bytes.
const maxCompatibilityRecord = 64 << 10

// errSnapshotIncompatible is a published snapshot taken on a host, a VMM or a guest
// kernel other than the ones this restore would use.
//
// FIRECRACKER'S OWN RULE, NOT A CAUTION OF BILLET'S: "Snapshots must be resumed on
// an software and hardware configuration which is identical to what they were
// generated on" (docs/snapshotting/snapshot-support.md). Firecracker checks only a
// CRC of the state file before loading, so a mismatch is not refused by the VMM: it
// is a guest that misbehaves after it resumes. So billet refuses it before the load.
var errSnapshotIncompatible = errors.New("the snapshot was taken where it cannot be restored")

// compatibility is what a snapshot was taken on, and what a restore must match
// exactly. Every field is a string compared whole: there is no notion here of a
// newer value being good enough, because Firecracker gives none.
type compatibility struct {
	Format int `json:"format"`
	// CPU is the processor's identity, every distinct one the host reports:
	// vendor, family, model and stepping, and the microcode revision, because a
	// microcode update can change what the guest was told the CPU supports.
	CPU string `json:"cpu"`
	// CPUFeatures is a digest of the feature flags the host reports.
	CPUFeatures string `json:"cpu_features"`
	// HostKernel is the running kernel's release and build.
	HostKernel string `json:"host_kernel"`
	// VMM is a digest of the Firecracker binary.
	VMM string `json:"vmm"`
	// GuestKernel is a digest of the kernel the guest booted.
	GuestKernel string `json:"guest_kernel"`
	// VCPUs and Memory are the guest's shape, which the snapshot fixes: a restore
	// must ask for exactly this, because admission charges a microVM by its spec.
	// Not part of the host's description, so mismatch leaves them to the restore.
	VCPUs  int   `json:"vcpus"`
	Memory int64 `json:"memory_bytes"`
	// Image is the generation the captured guest's root disk was cloned from, which
	// pairs a restore with the kernel that guest was verified against.
	Image string `json:"image"`
	// Disk is the golden's system disk, written golden@generation: the disk this
	// memory was taken with, and the only one a restore may put under it.
	Disk string `json:"disk"`
	// Deployment is the deployment whose node captured the golden, and Builder the
	// lease whose microVM it was.
	Deployment string `json:"deployment"`
	Builder    string `json:"builder"`
	// StateDigest and MemoryDigest are the SHA-256 of the published state and
	// memory, computed as they were copied out of the jail.
	StateDigest  string `json:"state_digest"`
	MemoryDigest string `json:"memory_digest"`
}

// errSnapshotNotExpected is a published snapshot that is not the golden the restore
// was told to expect, or a restore told nothing to expect.
var errSnapshotNotExpected = errors.New("the snapshot is not the golden the restore expects")

// Evidence is what a capture published, which the caller keeps and hands back to
// every Restore of that golden.
//
// THE CALLER'S, NOT THE RECORD'S. A restore is checked against what the control
// plane recorded when it admitted the golden's build, carried to the node by the
// command that asks for the restore, so a snapshot of another deployment, another
// build or other bytes under the same name is refused before it is loaded. Reading
// the expectation from the record would check the record against itself.
//
// THE DIGESTS ARE TAKEN AT PUBLICATION, NOT AT EVERY RESTORE. The store is billet's,
// writable by no one else and its files 0444, which is the host Firecracker's threat
// model trusts; what crosses a trust boundary is a golden moved to another node,
// and that is where its files are digested again (#508, item 5). A restore compares
// the published record with the evidence, through the directory it links from,
// because digesting the memory again would cost about 0.7 s per GiB (measured on
// the reference node, 2026-10-22) of a restore that takes under half a second.
type Evidence struct {
	Deployment string
	Builder    string
	Disk       string
	State      string
	Memory     string
}

// evidence is what this record says its golden is.
func (c compatibility) evidence() Evidence {
	return Evidence{
		Deployment: c.Deployment, Builder: c.Builder, Disk: c.Disk,
		State: c.StateDigest, Memory: c.MemoryDigest,
	}
}

// expected refuses a record that is not the golden want names, or that this
// deployment (owner) did not publish.
//
// ZERO VALUES REFUSE: evidence with any field empty is no evidence of that field,
// and a restore told nothing is a restore of whatever happens to hold the name.
func (c compatibility) expected(want Evidence, owner, path string) error {
	for _, f := range []struct{ name, value string }{
		{"deployment", want.Deployment}, {"builder", want.Builder}, {"system disk", want.Disk},
		{"state digest", want.State}, {"memory digest", want.Memory},
	} {
		if f.value == "" {
			return fmt.Errorf("firecracker: %w: the restore of %s was given no %s to check it against",
				errSnapshotNotExpected, path, f.name)
		}
	}

	if c.Deployment != owner {
		return fmt.Errorf("firecracker: %w: %s was published by deployment %s, not this one",
			errSnapshotNotExpected, path, bounded(c.Deployment))
	}

	got := c.evidence()

	for _, f := range []struct{ name, got, want string }{
		{"deployment", got.Deployment, want.Deployment}, {"builder", got.Builder, want.Builder},
		{"system disk", got.Disk, want.Disk}, {"state", got.State, want.State},
		{"memory", got.Memory, want.Memory},
	} {
		if f.got != f.want {
			return fmt.Errorf("firecracker: %w: %s has %s %s, and the restore expects %s",
				errSnapshotNotExpected, path, f.name, bounded(f.got), bounded(f.want))
		}
	}

	return nil
}

// mismatch names the first field two records disagree on, or "" when they agree.
func (c compatibility) mismatch(host compatibility) string {
	for _, f := range []struct{ name, ours, theirs string }{
		{"the CPU", c.CPU, host.CPU},
		{"the CPU features", c.CPUFeatures, host.CPUFeatures},
		{"the host kernel", c.HostKernel, host.HostKernel},
		{"the Firecracker build", c.VMM, host.VMM},
		{"the guest kernel", c.GuestKernel, host.GuestKernel},
	} {
		if f.ours != f.theirs {
			return fmt.Sprintf("%s: the snapshot's is %s, this host's is %s", f.name, bounded(f.ours),
				bounded(f.theirs))
		}
	}

	return ""
}

// hostCompatibility is this host's record for a guest whose kernel has that digest.
func (p *Provider) hostCompatibility(guestKernel string) (compatibility, error) {
	cpu, features, err := cpuIdentity(p.cpuinfoPath)
	if err != nil {
		return compatibility{}, err
	}

	kernel, err := p.hostKernel()
	if err != nil {
		return compatibility{}, fmt.Errorf("firecracker: read the host kernel's release: %w", err)
	}

	vmm, err := p.digests.of(p.execPath)
	if err != nil {
		return compatibility{}, fmt.Errorf("firecracker: digest the Firecracker binary %s: %w", p.execPath, err)
	}

	return compatibility{
		Format: compatibilityFormat, CPU: cpu, CPUFeatures: features, HostKernel: kernel,
		VMM: vmm, GuestKernel: guestKernel,
	}, nil
}

// cpuIdentityKeys are the /proc/cpuinfo fields that name a processor, on x86 and on
// arm64. The feature flags are digested separately; frequencies, cache sizes and
// core numbering are left out because they vary while the CPU does not.
var cpuIdentityKeys = []string{
	"vendor_id", "cpu family", "model", "model name", "stepping", "microcode",
	"CPU implementer", "CPU architecture", "CPU variant", "CPU part", "CPU revision",
}

// cpuIdentity reads every distinct processor identity and feature set a host
// reports, so a host whose cores differ (a hybrid part, or a microcode update that
// has reached some cores) is described as it is, deterministically.
func cpuIdentity(path string) (string, string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", "", fmt.Errorf("firecracker: read the CPU's identity from %s: %w", path, err)
	}

	var (
		identities, features []string
		identity             []string
		flags                string
	)

	flush := func() {
		if len(identity) > 0 && !slices.Contains(identities, strings.Join(identity, "; ")) {
			identities = append(identities, strings.Join(identity, "; "))
		}

		if flags != "" && !slices.Contains(features, flags) {
			features = append(features, flags)
		}

		identity, flags = nil, ""
	}

	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)

	for scanner.Scan() {
		key, value, found := strings.Cut(scanner.Text(), ":")
		if !found {
			flush()

			continue
		}

		key, value = strings.TrimSpace(key), strings.TrimSpace(value)

		switch {
		case key == "flags" || key == "Features":
			fields := strings.Fields(value)
			slices.Sort(fields)
			flags = strings.Join(fields, " ")
		case slices.Contains(cpuIdentityKeys, key):
			identity = append(identity, key+"="+value)
		}
	}

	if err := scanner.Err(); err != nil {
		return "", "", fmt.Errorf("firecracker: read the CPU's identity from %s: %w", path, err)
	}

	flush()

	if len(identities) == 0 || len(features) == 0 {
		return "", "", fmt.Errorf("firecracker: %s names no processor and its features, so this host's "+
			"CPU cannot be told apart from another's", path)
	}

	slices.Sort(identities)
	slices.Sort(features)

	digest := sha256.Sum256([]byte(strings.Join(features, "\n")))

	return strings.Join(identities, " | "), "sha256:" + hex.EncodeToString(digest[:]), nil
}

// unameKernel is the running kernel's release and build, as uname reports them.
func unameKernel() (string, error) {
	var u unix.Utsname
	if err := unix.Uname(&u); err != nil {
		return "", err
	}

	return unix.ByteSliceToString(u.Release[:]) + " " + unix.ByteSliceToString(u.Version[:]), nil
}

// fileDigests remembers the digest of a file for as long as it is the same file.
//
// A GUEST KERNEL IS TENS OF MEBIBYTES, and every restore needs its digest. Keyed by
// the inode and everything a rewrite changes (size, modification and change time),
// so a file replaced or edited in place is digested again, and a file merely read
// again is not.
//
// AND ONLY ONCE THE FILE HAS SETTLED. A filesystem stamps times at its clock's tick,
// not to the nanosecond (measured on Linux CI: a same-size rewrite within one tick
// kept the inode, the size and both times), so a key read moments after a write can
// equal the key after the next one. A digest is remembered only when the file's last
// change is older than settledAfter, the way git treats a "racy" index entry; a file
// still being written is digested every time.
//
// A LINK IS NOT A CHANGE. Every launch hard-links the guest kernel into its jail,
// and a link moves the inode's change time, so a digest read while another launch
// linked the file was refused as "changed while it was being read" (measured on the
// reference node, 2026-10-22: 8 of 10 concurrent restores). The read is judged by
// what a write moves, the size and the modification time; the change time stays in
// the key, so a file linked since is digested again rather than trusted.
type fileDigests struct {
	mu   sync.Mutex
	seen map[fileIdentity]string

	// now is the clock a file's settling is judged by; nil is time.Now.
	now func() time.Time

	// afterRead runs between the read and the second look at the file, so a test
	// can change the file at the one moment that matters; nil in production.
	afterRead func()
}

type fileIdentity struct {
	path                 string
	dev, ino             uint64
	size                 int64
	mtimeNsec, ctimeNsec int64
}

// sameBytes reports whether two identities are of one file whose contents no write
// has changed between them: everything but the change time, which a link moves too.
//
// ONLY FOR FILES BILLET'S OWN ACCOUNT WRITES, the kernel and the Firecracker binary,
// whose installer writes a new file and renames it: a writer that reset the
// modification time after writing would need that account. A file the jailed
// account wrote is judged by its whole identity (copyOut).
func (a fileIdentity) sameBytes(b fileIdentity) bool {
	a.ctimeNsec, b.ctimeNsec = 0, 0

	return a == b
}

// of is the digest of the regular file at path, read through one descriptor.
func (d *fileDigests) of(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	return d.ofOpen(f, path)
}

// ofOpen is the digest of a regular file already open, read from its start.
func (d *fileDigests) ofOpen(f *os.File, path string) (string, error) {
	var st unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &st); err != nil {
		return "", err
	}

	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		return "", fmt.Errorf("%s is not a regular file", path)
	}

	key := identityOf(path, &st)

	d.mu.Lock()
	known, ok := d.seen[key]
	d.mu.Unlock()

	if ok {
		return known, nil
	}

	hash := sha256.New()
	if _, err := io.Copy(hash, io.NewSectionReader(f, 0, st.Size)); err != nil {
		return "", err
	}

	if d.afterRead != nil {
		d.afterRead()
	}

	// THE SAME BYTES AFTER THE READ AS BEFORE IT, or the digest describes neither.
	var after unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &after); err != nil {
		return "", err
	}

	if !identityOf(path, &after).sameBytes(key) {
		return "", fmt.Errorf("%s changed while it was being read", path)
	}

	digest := "sha256:" + hex.EncodeToString(hash.Sum(nil))

	if !d.settled(&after) {
		return digest, nil
	}

	d.mu.Lock()
	if d.seen == nil {
		d.seen = map[fileIdentity]string{}
	}
	d.seen[key] = digest
	d.mu.Unlock()

	return digest, nil
}

// settledAfter is how long a file must go unchanged before its digest is
// remembered: far more than any filesystem's timestamp tick.
const settledAfter = 2 * time.Second

// settled reports whether a file's last change is old enough that a later change
// is certain to carry a later time.
func (d *fileDigests) settled(st *unix.Stat_t) bool {
	now := time.Now
	if d.now != nil {
		now = d.now
	}

	changed := time.Unix(0, max(unix.TimespecToNsec(st.Mtim), unix.TimespecToNsec(st.Ctim)))

	return now().Sub(changed) > settledAfter
}

// readCompatibility reads the record published beside a snapshot, through the
// snapshot directory already held, judging it as the state and memory are judged.
func readCompatibility(dir *os.File, path string) (compatibility, error) {
	fd, err := unix.Openat(int(dir.Fd()), compatibilityFile, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return compatibility{}, fmt.Errorf("firecracker: %w: %s has no compatibility record, so where "+
				"it can be restored is unknown", errSnapshotIncompatible, path)
		}

		return compatibility{}, fmt.Errorf("firecracker: could not tell where %s can be restored: %w", path, err)
	}

	f := os.NewFile(uintptr(fd), path)
	defer f.Close()

	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return compatibility{}, fmt.Errorf("firecracker: could not tell where %s can be restored: %w", path, err)
	}

	if err := judgePublished(st, path); err != nil {
		return compatibility{}, err
	}

	raw, err := io.ReadAll(io.LimitReader(f, maxCompatibilityRecord+1))
	if err != nil {
		return compatibility{}, fmt.Errorf("firecracker: could not tell where %s can be restored: %w", path, err)
	}

	if len(raw) > maxCompatibilityRecord {
		return compatibility{}, fmt.Errorf("firecracker: %w: %s is larger than any compatibility record",
			errSnapshotIncompatible, path)
	}

	var record compatibility

	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(&record); err != nil {
		return compatibility{}, fmt.Errorf("firecracker: %w: %s is not a compatibility record: %w",
			errSnapshotIncompatible, path, err)
	}

	if record.Format > 0 && record.Format < compatibilityFormat {
		return compatibility{}, fmt.Errorf("firecracker: %w: %s is compatibility record format %d, written "+
			"by an earlier billet, which recorded no evidence of what it published; capture the golden again",
			errSnapshotIncompatible, path, record.Format)
	}

	if record.Format != compatibilityFormat {
		return compatibility{}, fmt.Errorf("firecracker: %w: %s is compatibility record format %d, and "+
			"this billet reads only %d", errSnapshotIncompatible, path, record.Format, compatibilityFormat)
	}

	return record, nil
}
