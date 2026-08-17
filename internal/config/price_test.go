package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestUSDPerHourParsesExactly(t *testing.T) {
	for input, want := range map[string]USDPerHour{
		`"0.340000"`: 340_000,
		`0.0042`:     4_200,
		`12`:         12_000_000,
	} {
		var got USDPerHour
		if err := yaml.Unmarshal([]byte(input), &got); err != nil {
			t.Fatalf("Unmarshal(%s): %v", input, err)
		}
		if got != want {
			t.Errorf("Unmarshal(%s) = %d, want %d", input, got, want)
		}
	}
}

func TestUSDPerHourRefusesValuesItCannotRepresentExactly(t *testing.T) {
	for _, input := range []string{`-1`, `0.1234567`, `.4`, `1e2`, `NaN`} {
		var got USDPerHour
		if err := yaml.Unmarshal([]byte(input), &got); err == nil {
			t.Errorf("Unmarshal(%s) succeeded with %d", input, got)
		}
	}
}

func TestACloudShapeMustDeclareItsPrice(t *testing.T) {
	for name, replacement := range map[string]string{
		"missing": "",
		"zero":    "        price_usd_per_hour: 0\n",
	} {
		t.Run(name, func(t *testing.T) {
			got := loadErr(t, cloudConfig(t, "        price_usd_per_hour: 0.34\n", replacement))
			if !strings.Contains(got, "price_usd_per_hour") {
				t.Errorf("the error does not name the missing price: %s", got)
			}
		})
	}
}

func TestEC2PeakHourlyExposureBoundsAnyShapeMix(t *testing.T) {
	shapes := []EC2InstanceType{
		{Type: "cpu", VCPU: 2, Memory: 8 * GiB, PriceUSDPerHour: 200_000},
		{Type: "memory", VCPU: 4, Memory: 4 * GiB, PriceUSDPerHour: 600_000},
	}

	got, err := EC2PeakHourlyExposure(8, 16*GiB, shapes)
	if err != nil {
		t.Fatalf("EC2PeakHourlyExposure: %v", err)
	}
	if got != 1_200_000 {
		t.Errorf("peak = %s, want $1.20/hour", &got)
	}
	if monthly := got.ForHours(730); monthly != "$876" {
		t.Errorf("730-hour price = %s, want $876", monthly)
	}
}
