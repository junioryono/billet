package config

import (
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

const microsPerUSD = 1_000_000

// USDPerHour is an hourly US-dollar amount stored as millionths of a dollar.
// EC2 publishes rates beyond cents, so cents are not enough; a float would make
// the estimate depend on rounding at every addition.
type USDPerHour int64

// ParseUSDPerHour parses unsigned decimal dollars with at most six fractional
// digits. Exponents and signs are deliberately outside the config grammar.
func ParseUSDPerHour(s string) (USDPerHour, error) {
	trimmed := strings.TrimSpace(s)
	parts := strings.Split(trimmed, ".")
	if len(parts) > 2 || len(parts) == 0 || parts[0] == "" {
		return 0, fmt.Errorf("price %q must be decimal dollars such as 0.34", s)
	}
	for _, part := range parts {
		if part == "" {
			return 0, fmt.Errorf("price %q must be decimal dollars such as 0.34", s)
		}
		for _, r := range part {
			if r < '0' || r > '9' {
				return 0, fmt.Errorf("price %q must be decimal dollars such as 0.34", s)
			}
		}
	}
	if len(parts) == 2 && len(parts[1]) > 6 {
		return 0, fmt.Errorf("price %q has more than six decimal places", s)
	}

	whole, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || whole > math.MaxInt64/microsPerUSD {
		return 0, fmt.Errorf("price %q overflows the supported dollar amount", s)
	}
	value := whole * microsPerUSD
	if len(parts) == 2 {
		fraction := parts[1] + strings.Repeat("0", 6-len(parts[1]))
		micros, parseErr := strconv.ParseInt(fraction, 10, 64)
		if parseErr != nil || value > math.MaxInt64-micros {
			return 0, fmt.Errorf("price %q overflows the supported dollar amount", s)
		}
		value += micros
	}

	return USDPerHour(value), nil
}

// UnmarshalYAML accepts a quoted or bare decimal.
func (p *USDPerHour) UnmarshalYAML(node *yaml.Node) error {
	var s string
	if err := node.Decode(&s); err != nil {
		return fmt.Errorf("price must be decimal dollars such as 0.34")
	}
	value, err := ParseUSDPerHour(s)
	if err != nil {
		return err
	}
	*p = value

	return nil
}

// MarshalYAML writes decimal dollars without losing precision.
func (p *USDPerHour) MarshalYAML() (any, error) { return p.Decimal(), nil }

// Decimal formats dollars without a currency marker or unit.
func (p *USDPerHour) Decimal() string {
	whole := int64(*p) / microsPerUSD
	fraction := int64(*p) % microsPerUSD
	if fraction == 0 {
		return strconv.FormatInt(whole, 10)
	}

	return fmt.Sprintf("%d.%s", whole,
		strings.TrimRight(fmt.Sprintf("%06d", fraction), "0"))
}

func (p *USDPerHour) String() string { return "$" + p.Decimal() + "/hour" }

// ForHours formats what this rate costs across a fixed number of hours.
func (p *USDPerHour) ForHours(hours int64) string {
	micros := new(big.Int).Mul(big.NewInt(int64(*p)), big.NewInt(hours))

	return "$" + decimalMicros(micros)
}

// RemoteCostNode holds the resource and shape declarations needed to bound one
// registered REMOTE node's compute cost.
//
// NOT EC2-SPECIFIC, and the name said it was. Every remote backend declares ordered
// shapes with a price per hour — that is how placement charges the first that fits —
// so the arithmetic below is about shapes and ceilings rather than about which API
// buys them. What was ec2-specific was the QUERY that fed it, which is why a
// codebuild fleet's exposure was invisible in `billet status`.
type RemoteCostNode struct {
	MaxVCPU     int
	MaxMemory   ByteSize
	Shapes      []RemoteShape
	Outstanding USDPerHour
}

func decimalMicros(micros *big.Int) string {
	whole, fraction := new(big.Int), new(big.Int)
	whole.QuoRem(micros, big.NewInt(microsPerUSD), fraction)
	if fraction.Sign() == 0 {
		return whole.String()
	}

	return whole.String() + "." + strings.TrimRight(fmt.Sprintf("%06d", fraction.Int64()), "0")
}

// RemotePeakHourlyExposure returns a conservative compute-only upper bound for a
// node. It takes the tighter of the highest price-per-vCPU and
// price-per-byte bounds. Either one bounds every possible mix of shapes.
func RemotePeakHourlyExposure(maxVCPU int, maxMemory ByteSize, shapes []RemoteShape) (USDPerHour, error) {
	micros := remotePeakHourlyExposure(maxVCPU, maxMemory, shapes)
	if !micros.IsInt64() {
		return 0, errorsPriceOverflow
	}

	return USDPerHour(micros.Int64()), nil
}

// RemoteFleetPeakHourlyExposure returns the tighter of the shared deployment
// ceiling and the sum of every registered remote node's own ceiling.
func RemoteFleetPeakHourlyExposure(maxVCPU int, maxMemory ByteSize, nodes []RemoteCostNode) (USDPerHour, error) {
	var shapes []RemoteShape
	nodeBound := new(big.Int)
	outstanding := new(big.Int)
	for i := range nodes {
		shapes = append(shapes, nodes[i].Shapes...)
		configured := remotePeakHourlyExposure(
			nodes[i].MaxVCPU, nodes[i].MaxMemory, nodes[i].Shapes)
		open := big.NewInt(int64(nodes[i].Outstanding))
		if open.Cmp(configured) > 0 {
			configured = open
		}
		nodeBound.Add(nodeBound, configured)
		outstanding.Add(outstanding, open)
	}

	deploymentBound := remotePeakHourlyExposure(maxVCPU, maxMemory, shapes)
	if outstanding.Cmp(deploymentBound) > 0 {
		deploymentBound = outstanding
	}
	bound := nodeBound
	if deploymentBound.Cmp(bound) < 0 {
		bound = deploymentBound
	}
	if !bound.IsInt64() {
		return 0, errorsPriceOverflow
	}

	return USDPerHour(bound.Int64()), nil
}

func remotePeakHourlyExposure(maxVCPU int, maxMemory ByteSize, shapes []RemoteShape) *big.Int {
	if maxVCPU <= 0 || maxMemory <= 0 {
		return new(big.Int)
	}

	var perVCPU, perByte *big.Rat
	for i := range shapes {
		shape := &shapes[i]
		if shape.PriceUSDPerHour <= 0 || shape.VCPU <= 0 || shape.Memory <= 0 {
			continue
		}
		vcpuRate := new(big.Rat).SetFrac64(int64(shape.PriceUSDPerHour), int64(shape.VCPU))
		memoryRate := new(big.Rat).SetFrac64(int64(shape.PriceUSDPerHour), int64(shape.Memory))
		if perVCPU == nil || vcpuRate.Cmp(perVCPU) > 0 {
			perVCPU = vcpuRate
		}
		if perByte == nil || memoryRate.Cmp(perByte) > 0 {
			perByte = memoryRate
		}
	}
	if perVCPU == nil {
		return new(big.Int)
	}

	vcpuBound := new(big.Rat).Mul(perVCPU, new(big.Rat).SetInt64(int64(maxVCPU)))
	memoryBound := new(big.Rat).Mul(perByte, new(big.Rat).SetInt64(int64(maxMemory)))
	bound := vcpuBound
	if memoryBound.Cmp(bound) < 0 {
		bound = memoryBound
	}

	return ceilRat(bound)
}

var errorsPriceOverflow = fmt.Errorf("peak EC2 price overflows the supported dollar amount")

func ceilRat(value *big.Rat) *big.Int {
	quotient, remainder := new(big.Int), new(big.Int)
	quotient.QuoRem(value.Num(), value.Denom(), remainder)
	if remainder.Sign() != 0 {
		quotient.Add(quotient, big.NewInt(1))
	}

	return quotient
}
