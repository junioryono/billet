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

func TestRemotePeakHourlyExposureBoundsAnyShapeMix(t *testing.T) {
	shapes := []EC2InstanceType{
		{Type: "cpu", VCPU: 2, Memory: 8 * GiB, PriceUSDPerHour: 200_000},
		{Type: "memory", VCPU: 4, Memory: 4 * GiB, PriceUSDPerHour: 600_000},
	}

	got, err := RemotePeakHourlyExposure(8, 16*GiB, shapes)
	if err != nil {
		t.Fatalf("RemotePeakHourlyExposure: %v", err)
	}
	if got != 1_200_000 {
		t.Errorf("peak = %s, want $1.20/hour", &got)
	}
	if monthly := got.ForHours(730); monthly != "$876" {
		t.Errorf("730-hour price = %s, want $876", monthly)
	}
}

func TestRemoteFleetPeakHourlyExposureUsesTheSharedDeploymentCeiling(t *testing.T) {
	nodes := []RemoteCostNode{
		{MaxVCPU: 8, MaxMemory: 16 * GiB, Shapes: []EC2InstanceType{
			{Type: "cpu", VCPU: 8, Memory: 16 * GiB, PriceUSDPerHour: 340_000},
		}},
		{MaxVCPU: 8, MaxMemory: 16 * GiB, Shapes: []EC2InstanceType{
			{Type: "memory", VCPU: 8, Memory: 16 * GiB, PriceUSDPerHour: 600_000},
		}},
	}

	got, err := RemoteFleetPeakHourlyExposure(8, 16*GiB, nodes)
	if err != nil {
		t.Fatalf("RemoteFleetPeakHourlyExposure: %v", err)
	}
	if got != 600_000 {
		t.Errorf("peak = %s, want $0.60/hour from the shared deployment ceiling", &got)
	}
}

func TestRemoteFleetPeakHourlyExposureUsesTheSumOfNodeCeilings(t *testing.T) {
	nodes := []RemoteCostNode{
		{MaxVCPU: 4, MaxMemory: 8 * GiB, Shapes: []EC2InstanceType{
			{Type: "small", VCPU: 4, Memory: 8 * GiB, PriceUSDPerHour: 200_000},
		}},
		{MaxVCPU: 8, MaxMemory: 16 * GiB, Shapes: []EC2InstanceType{
			{Type: "large", VCPU: 8, Memory: 16 * GiB, PriceUSDPerHour: 600_000},
		}},
	}

	got, err := RemoteFleetPeakHourlyExposure(64, 256*GiB, nodes)
	if err != nil {
		t.Fatalf("RemoteFleetPeakHourlyExposure: %v", err)
	}
	if got != 800_000 {
		t.Errorf("peak = %s, want $0.80/hour from the two node ceilings", &got)
	}
}

func TestRemoteFleetPeakHourlyExposureDoesNotDropBelowOutstandingWork(t *testing.T) {
	nodes := []RemoteCostNode{
		{MaxVCPU: 8, MaxMemory: 16 * GiB, Outstanding: 1_200_000,
			Shapes: []EC2InstanceType{
				{Type: "expensive", VCPU: 8, Memory: 16 * GiB, PriceUSDPerHour: 600_000},
			}},
		{MaxVCPU: 8, MaxMemory: 16 * GiB, Outstanding: 0,
			Shapes: []EC2InstanceType{
				{Type: "spare", VCPU: 8, Memory: 16 * GiB, PriceUSDPerHour: 200_000},
			}},
	}

	got, err := RemoteFleetPeakHourlyExposure(8, 16*GiB, nodes)
	if err != nil {
		t.Fatalf("RemoteFleetPeakHourlyExposure: %v", err)
	}
	if got != 1_200_000 {
		t.Errorf("peak = %s, want the $1.20/hour already outstanding above tightened ceilings", &got)
	}
}
