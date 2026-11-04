package config

import (
	"fmt"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// MinuteWeight is what one minute of a tier counts as, in minutes of a 2 vCPU
// Linux runner, stored as thousandths so that a sum of minutes never depends on
// a float's rounding. Zero means the tier names none.
//
//nolint:recvcheck // UnmarshalYAML must take a pointer to assign; the rest read a value, so a field marshals.
type MinuteWeight int64

const (
	minuteWeightUnit = 1000
	// maxMinuteWeight is a thousand minutes a minute, far past any runner billet
	// can buy, so a typo reads as a refusal and not as a bill.
	maxMinuteWeight = 1000 * minuteWeightUnit
)

// ParseMinuteWeight parses an unsigned decimal with at most three places, more
// than zero and at most 1000. Exponents and signs are outside the grammar, as
// they are for a price.
func ParseMinuteWeight(s string) (MinuteWeight, error) {
	refuse := func() (MinuteWeight, error) {
		return 0, fmt.Errorf("minute_weight %q must be a decimal such as 1.5, more than 0, at most "+
			"1000 and with at most three places", s)
	}

	whole, fraction, dotted := strings.Cut(strings.TrimSpace(s), ".")
	if whole == "" || (dotted && (fraction == "" || len(fraction) > 3)) {
		return refuse()
	}
	for _, r := range whole + fraction {
		if r < '0' || r > '9' {
			return refuse()
		}
	}
	if len(whole) > 4 {
		return refuse()
	}

	units, err := strconv.ParseInt(whole, 10, 64)
	if err != nil {
		return refuse()
	}

	value := units * minuteWeightUnit
	if fraction != "" {
		thousandths, err := strconv.ParseInt(fraction+strings.Repeat("0", 3-len(fraction)), 10, 64)
		if err != nil {
			return refuse()
		}
		value += thousandths
	}
	if value <= 0 || value > maxMinuteWeight {
		return refuse()
	}

	return MinuteWeight(value), nil
}

// UnmarshalYAML accepts a quoted or bare decimal.
func (w *MinuteWeight) UnmarshalYAML(node *yaml.Node) error {
	var s string
	if err := node.Decode(&s); err != nil {
		return fmt.Errorf("minute_weight must be a decimal such as 1.5")
	}

	value, err := ParseMinuteWeight(s)
	if err != nil {
		return err
	}
	*w = value

	return nil
}

// MarshalYAML writes the decimal without losing precision.
func (w MinuteWeight) MarshalYAML() (any, error) { return w.Decimal(), nil }

// Decimal formats the weight with no trailing zeros.
func (w MinuteWeight) Decimal() string { return FormatThousandths(int64(w)) }

// Thousandths is the weight as an integer count of thousandths.
func (w MinuteWeight) Thousandths() int64 { return int64(w) }

// FormatThousandths writes an integer count of thousandths as an exact decimal.
func FormatThousandths(n int64) string {
	whole, fraction := n/minuteWeightUnit, n%minuteWeightUnit
	if fraction == 0 {
		return strconv.FormatInt(whole, 10)
	}

	return fmt.Sprintf("%d.%s", whole, strings.TrimRight(fmt.Sprintf("%03d", fraction), "0"))
}
