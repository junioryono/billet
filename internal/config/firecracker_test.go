package config

import (
	"strings"
	"testing"
)

// validFirecracker is a microVM block that passes CheckFirecracker, so a case can
// change exactly the field it is about.
func validFirecracker() FirecrackerConfig {
	f := FirecrackerConfig{
		KernelImage: "/var/lib/billet/images/vmlinux",
		Bridge:      "br0",
	}
	f.Normalize()

	return f
}

// firecrackerErrors renders what CheckFirecracker said, for an assertion on the
// diagnostic rather than on the count.
func firecrackerErrors(f FirecrackerConfig) string {
	var b strings.Builder

	for _, err := range CheckFirecracker(f) {
		b.WriteString(err.Error())
		b.WriteByte('\n')
	}

	return b.String()
}

// THE DEFAULTS ARE WHERE THE REFERENCE HOST PUTS THEM, and a config that says
// nothing gets a working one rather than an empty string that fails later.
func TestTheFirecrackerDefaultsPointAtARealInstallation(t *testing.T) {
	t.Parallel()

	f := FirecrackerConfig{KernelImage: "/k", Bridge: "br0"}
	f.Normalize()

	for _, tc := range []struct{ field, got, want string }{
		{"binary_path", f.BinaryPath, DefaultFirecrackerBinary},
		{"jailer_path", f.JailerPath, DefaultJailerBinary},
		{"chroot_base", f.ChrootBase, DefaultChrootBase},
	} {
		if tc.got != tc.want {
			t.Errorf("%s defaulted to %q, want %q", tc.field, tc.got, tc.want)
		}
	}

	if f.JailUIDMin != DefaultJailUIDMin || f.JailUIDCount != DefaultJailUIDCount {
		t.Errorf("the uid range defaulted to %d+%d, want %d+%d",
			f.JailUIDMin, f.JailUIDCount, DefaultJailUIDMin, DefaultJailUIDCount)
	}

	if errs := CheckFirecracker(f); len(errs) != 0 {
		t.Errorf("a normalized block with only a kernel and a bridge was refused: %v", errs)
	}
}

// A UID RANGE THAT REACHES A REAL ACCOUNT IS REFUSED.
//
// The jailer drops each VMM to whichever number it is given, without asking whether
// a person owns it. Below the floor these belong to root, to the distribution's own
// system accounts, or to somebody's login — and running a guest's VMM as an existing
// user is worse than running it as root would at least be honest about.
func TestAJailUIDRangeThatReachesARealAccountIsRefused(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		minUID int
		count  int
		says   string
	}{
		{"root", 0, 16, "below"},
		{"a system account", 100, 16, "below"},
		{"an ordinary login", 1000, 16, "below"},
		{"just under the floor", MinJailUID - 1, 16, "below"},
		{"no uids at all", DefaultJailUIDMin, 0, "no microVM"},
		{"a negative count", DefaultJailUIDMin, -5, "no microVM"},
		{"past the top", maxJailUID - 4, 1000, "past the highest"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := validFirecracker()
			f.JailUIDMin, f.JailUIDCount = tc.minUID, tc.count

			if got := firecrackerErrors(f); !strings.Contains(got, tc.says) {
				t.Errorf("expected an error saying %q, got: %s", tc.says, got)
			}
		})
	}
}

// AND AN ORDINARY RANGE IS ACCEPTED, without which the rule above is
// indistinguishable from one that refuses everything.
func TestAnOrdinaryJailUIDRangeIsAccepted(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ minUID, count int }{
		{MinJailUID, 1},
		{DefaultJailUIDMin, DefaultJailUIDCount},
		{500000, 4096},
	} {
		f := validFirecracker()
		f.JailUIDMin, f.JailUIDCount = tc.minUID, tc.count

		if errs := CheckFirecracker(f); len(errs) != 0 {
			t.Errorf("the range %d+%d was refused: %v", tc.minUID, tc.count, errs)
		}
	}
}

// EVERY GUEST NEEDS A NETWORK, so the trusted bridge is required rather than
// defaulted: a runner that cannot reach GitHub registers and then does nothing,
// which looks like a healthy tier running no jobs.
func TestATrustedBridgeIsRequired(t *testing.T) {
	t.Parallel()

	f := validFirecracker()
	f.Bridge = ""

	if got := firecrackerErrors(f); !strings.Contains(got, "bridge is required") {
		t.Errorf("a firecracker node with no bridge was accepted: %s", got)
	}
}

// AND THE UNTRUSTED ONE IS NOT, because its ABSENCE is what refuses fork
// pull-request work. Requiring it would force every deployment to describe a
// network it does not want to offer.
func TestTheUntrustedBridgeIsOptional(t *testing.T) {
	t.Parallel()

	f := validFirecracker()

	if errs := CheckFirecracker(f); len(errs) != 0 {
		t.Errorf("a block with no untrusted bridge was refused: %v", errs)
	}
}

// THE TWO BRIDGES MUST DIFFER. Naming one bridge for both admits untrusted work
// onto the trusted network — the single outcome the setting exists to prevent,
// reached by a configuration that looks like it took the precaution.
func TestOneBridgeCannotServeBothTrustClasses(t *testing.T) {
	t.Parallel()

	f := validFirecracker()
	f.UntrustedBridge = f.Bridge

	got := firecrackerErrors(f)
	if !strings.Contains(got, "same network") {
		t.Errorf("one bridge was accepted for both trust classes: %s", got)
	}
}

// A PATH THE KERNEL OR THE JAILER WOULD NOT TAKE IS REFUSED WHEN THE FILE IS READ,
// not when a job lands. A node is a service, so a relative path resolves against
// whatever directory started it.
func TestAFirecrackerPathMustBeAbsoluteAndUnpadded(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		apply func(*FirecrackerConfig)
		says  string
	}{
		{"relative binary", func(f *FirecrackerConfig) { f.BinaryPath = "bin/firecracker" }, "relative"},
		{"relative jailer", func(f *FirecrackerConfig) { f.JailerPath = "bin/jailer" }, "relative"},
		{"relative kernel", func(f *FirecrackerConfig) { f.KernelImage = "images/vmlinux" }, "relative"},
		{"relative chroot", func(f *FirecrackerConfig) { f.ChrootBase = "jails" }, "relative"},
		{"padded kernel", func(f *FirecrackerConfig) { f.KernelImage = " /k " }, "whitespace"},
		{"empty kernel", func(f *FirecrackerConfig) { f.KernelImage = "" }, "required"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := validFirecracker()
			tc.apply(&f)

			if got := firecrackerErrors(f); !strings.Contains(got, tc.says) {
				t.Errorf("expected an error saying %q, got: %s", tc.says, got)
			}
		})
	}
}

// A BRIDGE NAME THE KERNEL WOULD NOT ACCEPT IS REFUSED HERE, where the field is,
// rather than by `ip` in a message that names neither.
func TestABridgeNameTheKernelWouldRefuseIsRefused(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		bridge string
		says   string
	}{
		{"too long", "br0123456789abcdef", "limit"},
		{"leading dash", "-br0", "network device name"},
		{"a path", "/dev/br0", "network device name"},
		{"padded", " br0 ", "whitespace"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := validFirecracker()
			f.Bridge = tc.bridge

			if got := firecrackerErrors(f); !strings.Contains(got, tc.says) {
				t.Errorf("expected an error saying %q, got: %s", tc.says, got)
			}
		})
	}
}

// AND ORDINARY NAMES ARE ACCEPTED. A rule that only ever refuses is one nobody can
// distinguish from a broken one — the same reason the runner-group validator is
// tested in both directions.
func TestOrdinaryBridgeNamesAreAccepted(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"br0", "virbr0", "billet-br", "br_untrusted", "br0.100"} {
		f := validFirecracker()
		f.Bridge = name

		if errs := CheckFirecracker(f); len(errs) != 0 {
			t.Errorf("the bridge name %q was refused: %v", name, errs)
		}
	}
}

// EVERY PROBLEM AT ONCE, so a misconfigured host takes one round trip to fix
// instead of five. The house rule for every other block here.
func TestEveryFirecrackerProblemIsReportedTogether(t *testing.T) {
	t.Parallel()

	got := firecrackerErrors(FirecrackerConfig{
		BinaryPath:  "relative/firecracker",
		JailerPath:  "",
		KernelImage: "",
		ChrootBase:  "also/relative",
		JailUIDMin:  0,
		Bridge:      "",
	})

	for _, must := range []string{"binary_path", "jailer_path", "kernel_image", "chroot_base",
		"jail_uid_min", "bridge"} {
		if !strings.Contains(got, must) {
			t.Errorf("the report does not name %s:\n%s", must, got)
		}
	}
}

// NORMALIZE TRIMS WHAT BILLET LATER PASSES VERBATIM. Validating a trimmed copy
// while the caller used the raw string is the exact defect the ec2 and ceph blocks
// each shipped with once.
func TestFirecrackerValuesAreTrimmed(t *testing.T) {
	t.Parallel()

	f := FirecrackerConfig{
		BinaryPath:      "  /usr/local/bin/firecracker  ",
		KernelImage:     "  /k  ",
		Bridge:          "  br0  ",
		UntrustedBridge: "  br1  ",
	}
	f.Normalize()

	for _, tc := range []struct{ field, got string }{
		{"binary_path", f.BinaryPath},
		{"kernel_image", f.KernelImage},
		{"bridge", f.Bridge},
		{"untrusted_bridge", f.UntrustedBridge},
	} {
		if tc.got != strings.TrimSpace(tc.got) {
			t.Errorf("%s is still padded: %q", tc.field, tc.got)
		}
	}

	if errs := CheckFirecracker(f); len(errs) != 0 {
		t.Errorf("a normalized block was refused: %v", errs)
	}
}

// A NIL BLOCK NORMALIZES WITHOUT PANICKING, which is how a docker or ec2 node
// reaches applyDefaults.
func TestNormalizingAnAbsentFirecrackerBlockIsSafe(t *testing.T) {
	t.Parallel()

	var f *FirecrackerConfig

	f.Normalize()
}
