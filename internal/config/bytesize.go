package config

import (
	"fmt"
	"math"
	"math/big"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// ByteSize is a byte count that reads and writes human units in YAML, so tiers
// can say `memory: 32GiB` instead of a nine-digit number.
//
// Both IEC (KiB/MiB/GiB/TiB, powers of 1024) and SI (KB/MB/GB/TB, powers of
// 1000) suffixes are accepted and are NOT treated as equivalent — 1GB is
// 1_000_000_000 and 1GiB is 1_073_741_824. Memory sizing runs close enough to a
// machine's real limits that silently conflating them would matter.
//
// Parsing is exact integer arithmetic on a deliberately restricted grammar. An
// earlier version used strconv.ParseFloat, which accepts "NaN", "Inf",
// hexadecimal floats, and exponent notation, and which loses precision above
// 2^53 — converting any of those to int64 is implementation-defined and can
// produce a negative size. A negative or wrapped ceiling silently disables the
// capacity check that stops billet overcommitting the machine, so this parser
// rejects anything it cannot represent exactly.
// MarshalYAML must work on values so a ByteSize field formats and marshals
// without the caller taking its address. Mixed receivers are correct here.
//
//nolint:recvcheck // UnmarshalYAML must take a pointer to assign; String and
type ByteSize int64

const (
	KiB ByteSize = 1 << (10 * (iota + 1))
	MiB
	GiB
	TiB
)

var byteUnits = []struct {
	suffix string
	mult   int64
}{
	// Longest suffixes first: "GiB" must win over "B".
	{"KIB", int64(KiB)}, {"MIB", int64(MiB)}, {"GIB", int64(GiB)}, {"TIB", int64(TiB)},
	{"KB", 1_000}, {"MB", 1_000_000}, {"GB", 1_000_000_000}, {"TB", 1_000_000_000_000},
	{"K", int64(KiB)}, {"M", int64(MiB)}, {"G", int64(GiB)}, {"T", int64(TiB)},
	{"B", 1},
}

// mantissaRe is the whole grammar: digits, with an optional fractional part.
// No sign, no exponent, no hex, no "inf"/"nan".
var mantissaRe = regexp.MustCompile(`^(\d+)(?:\.(\d+))?$`)

// ParseByteSize parses a size like "32GiB", "512 MB", "1024", or "1.5GiB".
// A bare number is bytes.
//
// Fractional values are allowed only when they land on a whole number of bytes:
// "1.5GiB" is exact, "0.1KiB" is not and is rejected rather than silently
// truncated to 102.
func ParseByteSize(s string) (ByteSize, error) {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return 0, fmt.Errorf("empty byte size")
	}
	upper := strings.ToUpper(trimmed)

	mantissa, mult := upper, int64(1)
	for _, u := range byteUnits {
		if strings.HasSuffix(upper, u.suffix) {
			mantissa = strings.TrimSpace(upper[:len(upper)-len(u.suffix)])
			mult = u.mult
			break
		}
	}
	if mantissa == "" {
		return 0, fmt.Errorf("byte size %q has a unit but no number", s)
	}

	m := mantissaRe.FindStringSubmatch(mantissa)
	if m == nil {
		return 0, fmt.Errorf(
			"byte size %q: expected digits optionally followed by a unit such as KiB, MiB, GiB, TiB, KB, MB, GB or TB", s)
	}

	// value = (intPart + fracPart/10^len(fracPart)) * mult, computed exactly.
	value := new(big.Rat)
	if _, ok := value.SetString(m[1]); !ok {
		return 0, fmt.Errorf("byte size %q: cannot parse %q", s, m[1])
	}
	if frac := m[2]; frac != "" {
		num, ok := new(big.Int).SetString(frac, 10)
		if !ok {
			return 0, fmt.Errorf("byte size %q: cannot parse fraction %q", s, frac)
		}
		den := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(len(frac))), nil)
		value.Add(value, new(big.Rat).SetFrac(num, den))
	}
	value.Mul(value, new(big.Rat).SetInt64(mult))

	if !value.IsInt() {
		return 0, fmt.Errorf("byte size %q does not resolve to a whole number of bytes", s)
	}
	// IsInt64 is the whole bound: a big.Int that fits in an int64 is by definition
	// <= MaxInt64, so comparing against MaxInt64 afterwards is dead code.
	n := value.Num()
	if !n.IsInt64() {
		return 0, fmt.Errorf("byte size %q overflows int64 (max %s)", s, ByteSize(math.MaxInt64))
	}
	return ByteSize(n.Int64()), nil
}

// String renders the size using the largest IEC unit that divides it exactly,
// so a value parsed from "32GiB" round-trips as "32GiB" rather than as bytes.
func (b ByteSize) String() string {
	if b == 0 {
		return "0B"
	}
	if b < 0 {
		// Should be unreachable via ParseByteSize, but a struct literal can still
		// produce one and a silently wrong rendering would be worse.
		return strconv.FormatInt(int64(b), 10) + "B"
	}
	for _, u := range []struct {
		suffix string
		mult   ByteSize
	}{{"TiB", TiB}, {"GiB", GiB}, {"MiB", MiB}, {"KiB", KiB}} {
		if b%u.mult == 0 {
			return strconv.FormatInt(int64(b/u.mult), 10) + u.suffix
		}
	}
	return strconv.FormatInt(int64(b), 10) + "B"
}

// UnmarshalYAML accepts either a quoted string ("32GiB") or a bare integer.
func (b *ByteSize) UnmarshalYAML(node *yaml.Node) error {
	var s string
	if err := node.Decode(&s); err == nil {
		v, err := ParseByteSize(s)
		if err != nil {
			return err
		}
		*b = v
		return nil
	}
	var n int64
	if err := node.Decode(&n); err != nil {
		return fmt.Errorf("byte size must be a string such as \"32GiB\" or a plain byte count")
	}
	if n < 0 {
		return fmt.Errorf("byte size must not be negative")
	}
	*b = ByteSize(n)
	return nil
}

// MarshalYAML writes the human form so a rewritten config stays readable.
func (b ByteSize) MarshalYAML() (any, error) { return b.String(), nil }
