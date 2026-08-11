package config

import (
	"math"
	"runtime"
	"strings"
	"testing"
)

// A CONTRIBUTION OF ZERO IS "I DID NOT SAY", and a negative one is not a smaller
// contribution — it is a number that makes every capacity comparison pass. The
// memory side is already refused when the size is parsed (bytesize.go); vcpu is
// a plain int and has nothing standing in front of it.
func TestANegativeContributionIsRefused(t *testing.T) {
	body := strings.Replace(validConfig, "  name: epyc-1", "  name: epyc-1\n  max_vcpu: -1", 1)

	_, err := Load(writeConfig(t, body))
	if err == nil {
		t.Fatal("Load accepted a node contributing a negative number of vCPU")
	}

	if !strings.Contains(err.Error(), "max_vcpu") {
		t.Errorf("the error does not name the field: %v", err)
	}
}

// Unset is the ordinary case and means "detect it", so it must keep loading.
func TestAnUnsetContributionIsAccepted(t *testing.T) {
	if _, err := Load(writeConfig(t, validConfig)); err != nil {
		t.Fatalf("a node that does not declare a contribution was refused: %v", err)
	}
}

// WHAT A NODE CONTRIBUTES WHEN NOBODY SAID, and the reason this is tested at all
// is that the failure is silent. A detector that returns zero does not error; it
// registers a host that can never be given work, and the tier it serves
// advertises nothing while the machine sits idle.
func TestDetectedCapacityDescribesThisMachine(t *testing.T) {
	t.Parallel()

	vcpu, memory, err := DetectHostCapacity()
	if err != nil {
		t.Fatalf("DetectHostCapacity: %v", err)
	}

	if vcpu != runtime.NumCPU() {
		t.Errorf("vcpu = %d, want %d — the detected count is not this machine's",
			vcpu, runtime.NumCPU())
	}

	if vcpu < 1 {
		t.Errorf("vcpu = %d, want at least 1 — a host with no cores cannot run a job", vcpu)
	}

	// A FLOOR RATHER THAN A VALUE, because the number is the machine's and the
	// test has to run on any of them. 256MiB is below anything that can run a
	// container and above anything a broken detector is likely to invent.
	if memory < 256*MiB {
		t.Errorf("memory = %s, want at least 256MiB — this is not a real reading", memory)
	}
}

// THE MULTIPLICATION IS WHERE THIS BREAKS. Linux reports memory as a count of
// units plus the unit size, so the value is a product, and ByteSize is a signed
// int64. A product past MaxInt64 converts to a NEGATIVE size, and a negative
// ceiling does not fail loudly — it silently disables the capacity check that
// stops billet overcommitting the machine, which is the same class of bug
// bytesize.go already documents for parsing.
func TestMemoryThatCannotBeRepresentedIsRefused(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		bytes uint64
		want  ByteSize
		fails bool
	}{
		{name: "an ordinary machine", bytes: 64 * uint64(GiB), want: 64 * GiB},
		{name: "exactly the largest representable", bytes: math.MaxInt64, want: math.MaxInt64},
		{name: "one past it", bytes: math.MaxInt64 + 1, fails: true},
		{name: "every bit set", bytes: math.MaxUint64, fails: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := memoryToByteSize(tc.bytes)

			if tc.fails {
				if err == nil {
					t.Fatalf("memoryToByteSize(%d) = %s, want an error rather than a size",
						tc.bytes, got)
				}

				return
			}

			if err != nil {
				t.Fatalf("memoryToByteSize(%d): %v", tc.bytes, err)
			}

			if got != tc.want {
				t.Errorf("memoryToByteSize(%d) = %s, want %s", tc.bytes, got, tc.want)
			}
		})
	}
}
