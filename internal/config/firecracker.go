package config

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

// FirecrackerConfig configures the bare-metal microVM backend.
type FirecrackerConfig struct {
	// BinaryPath and JailerPath locate the firecracker and jailer binaries.
	//
	// BinaryPath IS NOT ONLY AN EXECUTABLE. The jailer names its chroot after this
	// file AFTER RESOLVING SYMLINKS, so it also decides the directory billet
	// enumerates to find out what is running here — see the firecracker provider's
	// jail type, where getting it wrong reads as an empty inventory.
	BinaryPath string `yaml:"binary_path,omitempty"`
	JailerPath string `yaml:"jailer_path,omitempty"`
	// KernelImage is the uncompressed guest kernel. It must be built with
	// everything Docker needs; validate with moby's contrib/check-config.sh
	// rather than a hand-maintained list.
	KernelImage string `yaml:"kernel_image"`

	// KernelDir is where `billet images pull` keeps the kernels it fetches.
	//
	// SEPARATE FROM KernelImage BECAUSE THEY ANSWER DIFFERENT QUESTIONS. KernelImage
	// is the fallback for a generation that records no kernel of its own — a
	// hand-built image, whose builder installs none. This is where a PAIRED kernel is
	// looked up, and a generation that names one boots that one instead: the two are
	// published together and a mismatch fails inside somebody's job rather than at
	// launch.
	//
	// Empty means the default, which is where a pull puts them.
	KernelDir string `yaml:"kernel_dir,omitempty"`
	// ChrootBase is where the jailer builds each microVM's chroot. It defaults to
	// the jailer's own default, and it must be on local storage with room for a
	// hard link to the guest kernel per running job.
	ChrootBase string `yaml:"chroot_base,omitempty"`
	// JailUIDMin and JailUIDCount are the range of uids microVMs run as, ONE PER
	// GUEST.
	//
	// NOT ONE ACCOUNT FOR ALL OF THEM. The jailer drops each VMM to a uid, and a
	// shared one means every VMM on the host is the same user to the kernel — so a
	// VMM that escapes its chroot can reach every other jail's files, signal every
	// other VMM, and open every other guest's root disk. The chroot is what
	// separates them while it holds; a uid of its own is what separates them when
	// it does not.
	//
	// THEY ARE NUMBERS, NOT ACCOUNTS, and deliberately: an account a person could
	// log in as is a bigger thing than an integer the kernel uses to keep two
	// processes apart, and creating one per job is a deployment step per job. The
	// default range is far above anything a distribution allocates.
	JailUIDMin   int `yaml:"jail_uid_min,omitempty"`
	JailUIDCount int `yaml:"jail_uid_count,omitempty"`
	// Bridge is the host bridge trusted guests attach to.
	Bridge string `yaml:"bridge"`
	// UntrustedBridge is the bridge fork pull-request guests attach to, and its
	// ABSENCE is what refuses them.
	//
	// A microVM is a real isolation boundary, which is why this backend can run
	// code billet cannot vouch for at all — but that boundary is the KERNEL, not
	// the network. A guest on the ordinary bridge reaches whatever that bridge
	// reaches, which on a machine that also holds the Ceph cluster and the
	// control-plane database is everything that matters. So untrusted work runs
	// only once its network has been described separately, rather than defaulting
	// onto the trusted bridge because nobody said otherwise. This is the same rule,
	// and the same reasoning, as node.ec2.untrusted_security_group_ids.
	UntrustedBridge string `yaml:"untrusted_bridge,omitempty"`
	// ImageVerifyPort is the host port a verification guest reports to. It is
	// fixed so host policy can admit exactly this service instead of opening an
	// arbitrary high port to every guest. Only one verification runs per host,
	// enforced by the verification lock.
	ImageVerifyPort int `yaml:"image_verify_port,omitempty"`
}

// DefaultFirecrackerBinary and friends are where the reference host installs
// these, which is also where each project's own instructions put them.
const (
	DefaultFirecrackerBinary = "/usr/local/bin/firecracker"
	DefaultJailerBinary      = "/usr/local/bin/jailer"
	// DefaultKernelDir is where managed kernels are installed and where a
	// generation's recorded kernel name is resolved at launch.
	DefaultKernelDir = "/var/lib/billet/kernels"
	// DefaultChrootBase is the jailer's own default, so billet and a hand-run
	// jailer agree about where a microVM lives.
	DefaultChrootBase = "/srv/jailer"
	// DefaultJailUIDMin is where the per-microVM uid range starts.
	//
	// ABOVE EVERYTHING A DISTRIBUTION USES. Regular accounts stop at 60000 on
	// Debian and Ubuntu, systemd's DynamicUser range is 61184-65519, and the
	// subordinate-uid ranges `useradd` hands out for user namespaces start at
	// 100000 and are allocated 65536 at a time. Starting at 900000 sits clear of
	// all of it, and clear of the 65534 `nobody` that a truncated or overflowed
	// value would otherwise land on.
	DefaultJailUIDMin = 900000
	// DefaultJailUIDCount is how many microVMs one host may run at once, as far as
	// uids go. Far above what any machine can hold, so it is a guard rather than a
	// capacity limit — the allocator's budget is the real one.
	DefaultJailUIDCount = 1024
	// DefaultImageVerifyPort is adjacent to the control plane and guest cache
	// ports, and is reserved for the short-lived `billet images verify` listener.
	DefaultImageVerifyPort = 7719
	// MinJailUID is the lowest start billet will accept. Below this a range starts
	// overlapping accounts that belong to somebody, and a microVM would run as a
	// user with a home directory and a login shell.
	MinJailUID = 100000
)

// Normalize fills in the defaults and trims what billet later passes verbatim.
//
// EXPORTED FOR THE SAME REASON CheckFirecracker IS. The provider's constructor is
// exported and cannot assume its configuration came through Load, and a value that
// was trimmed for a CHECK while the caller used the raw one is the exact defect the
// ec2 and ceph blocks each shipped with once.
func (f *FirecrackerConfig) Normalize() {
	if f == nil {
		return
	}

	f.BinaryPath = strings.TrimSpace(f.BinaryPath)
	f.JailerPath = strings.TrimSpace(f.JailerPath)
	f.KernelImage = strings.TrimSpace(f.KernelImage)
	f.KernelDir = strings.TrimSpace(f.KernelDir)
	f.ChrootBase = strings.TrimSpace(f.ChrootBase)
	f.Bridge = strings.TrimSpace(f.Bridge)
	f.UntrustedBridge = strings.TrimSpace(f.UntrustedBridge)

	for _, d := range []struct {
		into  *string
		value string
	}{
		{&f.BinaryPath, DefaultFirecrackerBinary},
		{&f.JailerPath, DefaultJailerBinary},
		{&f.KernelDir, DefaultKernelDir},
		{&f.ChrootBase, DefaultChrootBase},
	} {
		if *d.into == "" {
			*d.into = d.value
		}
	}

	if f.JailUIDMin == 0 {
		f.JailUIDMin = DefaultJailUIDMin
	}

	if f.JailUIDCount == 0 {
		f.JailUIDCount = DefaultJailUIDCount
	}

	if f.ImageVerifyPort == 0 {
		f.ImageVerifyPort = DefaultImageVerifyPort
	}
}

// CheckFirecracker refuses a microVM block billet cannot safely act on.
//
// EXPORTED AND CALLED FROM BOTH SIDES, like CheckCeph and CheckEC2Endpoint: the
// provider's constructor is exported, so a rule enforced only in Load has a second
// entry point that does not enforce it.
//
// It takes a VALUE, so a caller cannot hand it a nil pointer and read the empty
// result as approval.
func CheckFirecracker(f FirecrackerConfig) []error {
	var errs []error

	// EVERY PATH IS ABSOLUTE, because a node is a service whose working directory
	// is whatever started it — the same rule node.ceph.conf_path follows. A relative
	// chroot base resolves against `/` under systemd, so a jail an operator built by
	// hand while testing is not the one billet enumerates in production.
	for _, f := range []struct{ field, value string }{
		{"binary_path", f.BinaryPath},
		{"jailer_path", f.JailerPath},
		{"kernel_image", f.KernelImage},
		{"kernel_dir", f.KernelDir},
		{"chroot_base", f.ChrootBase},
	} {
		switch {
		case f.value == "":
			errs = append(errs, fmt.Errorf("node.firecracker.%s is required", f.field))
		case f.value != strings.TrimSpace(f.value):
			errs = append(errs, fmt.Errorf("node.firecracker.%s %q has leading or trailing "+
				"whitespace; billet passes it to the jailer exactly as written", f.field, f.value))
		case !filepath.IsAbs(f.value):
			errs = append(errs, fmt.Errorf("node.firecracker.%s %q is relative; a node runs as a "+
				"service, whose working directory is not where you wrote this file",
				f.field, f.value))
		}
	}

	errs = append(errs, checkBridge("bridge", f.Bridge, true)...)
	errs = append(errs, checkBridge("untrusted_bridge", f.UntrustedBridge, false)...)

	// THE TWO BRIDGES MUST DIFFER, or the setting that admits untrusted work admits
	// it onto the trusted network — which is the one outcome
	// node.firecracker.untrusted_bridge exists to prevent, reached by a
	// configuration that looks like it took the precaution.
	if f.Bridge != "" && f.Bridge == f.UntrustedBridge {
		errs = append(errs, fmt.Errorf("node.firecracker.bridge and "+
			"node.firecracker.untrusted_bridge are both %q: a fork's pull request would run on "+
			"the same network as everything else, which is what naming a separate bridge is for",
			f.Bridge))
	}

	errs = append(errs, checkJailUIDs(f.JailUIDMin, f.JailUIDCount)...)

	if f.ImageVerifyPort < 1 || f.ImageVerifyPort > 65535 {
		errs = append(errs, fmt.Errorf("node.firecracker.image_verify_port is %d; expected 1-65535",
			f.ImageVerifyPort))
	}

	return errs
}

// checkJailUIDs refuses a uid range billet will not run microVMs as.
//
// THE FLOOR IS THE POINT. A range that starts low overlaps accounts that belong to
// somebody — root at 0, the distribution's system accounts, the operator's own login
// — and the jailer drops a VMM to whatever number it is given without asking whether
// a person owns it. Running a guest's VMM as an existing user is strictly worse than
// running it as root would be honest about.
func checkJailUIDs(minUID, count int) []error {
	var errs []error

	if minUID < MinJailUID {
		errs = append(errs, fmt.Errorf("node.firecracker.jail_uid_min is %d, and billet will not "+
			"run a microVM as a uid below %d: the jailer drops each vmm to one of these numbers, "+
			"and below that they belong to root, to the distribution's own accounts, or to a "+
			"person", minUID, MinJailUID))
	}

	if count <= 0 {
		errs = append(errs, fmt.Errorf("node.firecracker.jail_uid_count is %d, so no microVM "+
			"could ever be given a uid", count))
	}

	// A RANGE THAT WRAPS IS NOT A RANGE. These are added together to find the end,
	// and a large enough pair overflows to a negative one — which every "is this uid
	// in range" comparison then passes, including for uid 0.
	if count > 0 && minUID > 0 && minUID+count < minUID {
		errs = append(errs, fmt.Errorf("node.firecracker.jail_uid_min %d plus jail_uid_count %d "+
			"overflows", minUID, count))
	}

	// LINUX UIDS STOP AT 2^32-2, and the kernel refuses (uid_t)-1 outright. Staying
	// inside a signed 32-bit range keeps every number here representable everywhere
	// billet passes it — an argv value to the jailer, an argument to `ip tuntap`,
	// and a Go int on a 32-bit host.
	if end := minUID + count - 1; count > 0 && minUID >= MinJailUID && end > maxJailUID {
		errs = append(errs, fmt.Errorf("node.firecracker.jail_uid_min %d plus jail_uid_count %d "+
			"ends at %d, past the highest uid billet will use (%d)", minUID, count, end, maxJailUID))
	}

	return errs
}

// maxJailUID is the highest uid billet will hand a microVM. Well inside the
// kernel's own limit, and inside a signed 32-bit integer on every host billet
// builds for.
const maxJailUID = 2_000_000

// maxBridgeName is the kernel's IFNAMSIZ limit, less the terminator.
const maxBridgeName = 15

// bridgeName is what the kernel accepts as a network device name.
//
// A SHAPE RATHER THAN A LIST, and narrow because billet builds an `ip link set dev
// <tap> master <bridge>` argv from it. There is no shell, so this is not about
// quoting; it is that a leading dash is read by `ip` as an option, and a name over
// IFNAMSIZ is refused by the kernel with an error that names neither the field nor
// the limit.
var bridgeName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

// checkBridge refuses a bridge name that would not reach the kernel intact.
func checkBridge(field, name string, required bool) []error {
	if name == "" {
		if required {
			return []error{fmt.Errorf("node.firecracker.%s is required: every guest gets one "+
				"network device, and billet attaches it to this bridge", field)}
		}

		return nil
	}

	if name != strings.TrimSpace(name) {
		return []error{fmt.Errorf("node.firecracker.%s %q has leading or trailing whitespace; "+
			"billet passes it to `ip` exactly as written", field, name)}
	}

	if len(name) > maxBridgeName {
		return []error{fmt.Errorf("node.firecracker.%s %q is %d characters; the kernel's limit "+
			"for a network device name is %d", field, name, len(name), maxBridgeName)}
	}

	if !bridgeName.MatchString(name) {
		return []error{fmt.Errorf("node.firecracker.%s %q is not a network device name (expected "+
			"something like br0); billet builds an `ip link` argument from it, where a leading "+
			"dash is read as an option", field, name)}
	}

	// NOT A NAME BILLET ALLOCATES FOR ITSELF. Guest devices are named bt-0, bt-1 and
	// so on, taken lowest-first, so a bridge called bt-3 is a name a launch will
	// eventually try to create — and `ip tuntap add` refuses it because the kernel
	// already has it. That launch fails, hands the name back, and the next one draws
	// the same name and fails identically: the host stops being able to start a
	// microVM, for a reason that names a device rather than this setting.
	if strings.HasPrefix(name, TapPrefix) {
		return []error{fmt.Errorf("node.firecracker.%s %q starts with %q, which is how billet "+
			"names the device it creates for each guest; a launch would eventually try to create "+
			"this exact name and be refused, and every launch after it the same way. Rename the "+
			"bridge", field, name, TapPrefix)}
	}

	return nil
}

// TapPrefix is how the firecracker backend names the network device it creates for
// each guest.
//
// DECLARED HERE AND USED THERE, rather than the other way round, because config may
// not import a provider — and a second copy in this file would be a constant that can
// drift from the thing it is validating against, which is the whole failure this
// check exists to prevent.
const TapPrefix = "bt-"
