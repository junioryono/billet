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

	"golang.org/x/sys/unix"
)

// compatibilityFormat is the version of the compatibility record. A record of any
// other version is refused rather than read, because a field this binary does not
// know about is a condition it cannot check.
const compatibilityFormat = 1

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
type fileDigests struct {
	mu   sync.Mutex
	seen map[fileIdentity]string
}

type fileIdentity struct {
	path                 string
	dev, ino             uint64
	size                 int64
	mtimeNsec, ctimeNsec int64
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

	// THE SAME FILE AFTER THE READ AS BEFORE IT, or the digest describes neither.
	var after unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &after); err != nil {
		return "", err
	}

	if identityOf(path, &after) != key {
		return "", fmt.Errorf("%s changed while it was being read", path)
	}

	digest := "sha256:" + hex.EncodeToString(hash.Sum(nil))

	d.mu.Lock()
	if d.seen == nil {
		d.seen = map[fileIdentity]string{}
	}
	d.seen[key] = digest
	d.mu.Unlock()

	return digest, nil
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

	if record.Format != compatibilityFormat {
		return compatibility{}, fmt.Errorf("firecracker: %w: %s is compatibility record format %d, and "+
			"this billet reads only %d", errSnapshotIncompatible, path, record.Format, compatibilityFormat)
	}

	return record, nil
}
