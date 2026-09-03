package config

import (
	"math"
	"testing"
)

func TestParseByteSize(t *testing.T) {
	tests := []struct {
		in   string
		want ByteSize
	}{
		{"0", 0},
		{"1024", 1024},
		{"1B", 1},
		{"8K", 8 * KiB},
		{"32GiB", 32 * GiB},
		{"512 MB", 512_000_000},
		{"1TiB", TiB},
		{"  4 gib  ", 4 * GiB},
		{"1MIB", MiB},
		// Fractional values are fine when they land on a whole byte count.
		{"1.5GiB", 1_610_612_736},
		{"0.5KiB", 512},
	}
	for _, tt := range tests {
		got, err := ParseByteSize(tt.in)
		if err != nil {
			t.Errorf("ParseByteSize(%q) returned error: %v", tt.in, err)
			continue
		}
		if got != tt.want {
			t.Errorf("ParseByteSize(%q) = %d, want %d", tt.in, got, tt.want)
		}
	}
}

// IEC and SI suffixes must not be conflated. Memory sizing runs close enough to
// the machine's real limits that a 7% error compounds into overcommit.
func TestParseByteSizeDistinguishesIECFromSI(t *testing.T) {
	gib, err := ParseByteSize("1GiB")
	if err != nil {
		t.Fatalf("1GiB: %v", err)
	}
	gb, err := ParseByteSize("1GB")
	if err != nil {
		t.Fatalf("1GB: %v", err)
	}
	if gib == gb {
		t.Fatalf("1GiB and 1GB parsed identically (%d); they must differ", gib)
	}
	if gib != 1_073_741_824 {
		t.Errorf("1GiB = %d, want 1073741824", gib)
	}
	if gb != 1_000_000_000 {
		t.Errorf("1GB = %d, want 1000000000", gb)
	}
}

// The parser must not accept anything it cannot represent exactly. A NaN or
// wrapped value converted to int64 is implementation-defined and can come out
// negative, which would silently disable the capacity ceiling that stops billet
// overcommitting the machine.
func TestParseByteSizeRejectsNonFiniteAndExotic(t *testing.T) {
	for _, in := range []string{
		"NaN", "NaNB", "nanB", "Inf", "InfB", "+Inf", "-Inf", "infinity",
		"1e3B", "1E3GiB", "0x10B", "0x1p4B", // exponent and hex forms
		"+5GiB", "-5GiB", "-1", // signs
		"1_000B", "1,000B", // separators
		"", "   ", "GiB", "abc", "12XB", "..1B", "1.B", ".5B",
	} {
		if got, err := ParseByteSize(in); err == nil {
			t.Errorf("ParseByteSize(%q) = %d, want error", in, got)
		}
	}
}

// Overflow must be an error, not a wrap into a negative size.
func TestParseByteSizeRejectsOverflow(t *testing.T) {
	for _, in := range []string{
		"9999999999TiB",
		"99999999999999999999",
		"8388609TiB", // just past MaxInt64
	} {
		got, err := ParseByteSize(in)
		if err == nil {
			t.Errorf("ParseByteSize(%q) = %d, want overflow error", in, got)
		}
	}
	// The largest exactly-representable TiB value must still parse.
	if _, err := ParseByteSize("8388607TiB"); err != nil {
		t.Errorf("ParseByteSize(8388607TiB) should be within int64: %v", err)
	}
}

// Sub-byte precision is rejected rather than silently truncated: 0.1KiB is
// 102.4 bytes, and quietly returning 102 hides an operator mistake.
func TestParseByteSizeRejectsFractionalBytes(t *testing.T) {
	for _, in := range []string{"0.1KiB", "1.1B", "0.5B", "1.3333GiB"} {
		if got, err := ParseByteSize(in); err == nil {
			t.Errorf("ParseByteSize(%q) = %d, want a whole-bytes error", in, got)
		}
	}
}

// Values above 2^53 must stay exact; a float-based parser silently rounds them.
func TestParseByteSizeIsExactAboveFloat64Precision(t *testing.T) {
	const in = "9007199254740993" // 2^53 + 1
	got, err := ParseByteSize(in)
	if err != nil {
		t.Fatalf("ParseByteSize(%q): %v", in, err)
	}
	if got != 9_007_199_254_740_993 {
		t.Errorf("ParseByteSize(%q) = %d, want 9007199254740993 exactly", in, got)
	}
	if int64(got) == int64(float64(9_007_199_254_740_993)) && got != 9_007_199_254_740_993 {
		t.Error("value was rounded through float64")
	}
}

func TestByteSizeStringRoundTrips(t *testing.T) {
	for _, in := range []string{"32GiB", "8KiB", "1TiB", "512MiB"} {
		v, err := ParseByteSize(in)
		if err != nil {
			t.Fatalf("ParseByteSize(%q): %v", in, err)
		}
		if got := v.String(); got != in {
			t.Errorf("ByteSize(%q).String() = %q, want %q", in, got, in)
		}
	}
	if got := ByteSize(0).String(); got != "0B" {
		t.Errorf("ByteSize(0).String() = %q, want \"0B\"", got)
	}
	// A value with no exact IEC divisor falls back to plain bytes.
	if got := ByteSize(1_000_000_000).String(); got != "1000000000B" {
		t.Errorf("ByteSize(1e9).String() = %q, want plain bytes", got)
	}
	// A negative size cannot come from the parser, but a struct literal can
	// produce one and it must not render as a bogus unit.
	if got := ByteSize(-1).String(); got != "-1B" {
		t.Errorf("ByteSize(-1).String() = %q, want \"-1B\"", got)
	}
}

func TestParseByteSizeMaxInt64(t *testing.T) {
	got, err := ParseByteSize("9223372036854775807")
	if err != nil {
		t.Fatalf("MaxInt64: %v", err)
	}
	if got != ByteSize(math.MaxInt64) {
		t.Errorf("got %d, want MaxInt64", got)
	}
	if _, err := ParseByteSize("9223372036854775808"); err == nil {
		t.Error("MaxInt64+1 should overflow")
	}
}
