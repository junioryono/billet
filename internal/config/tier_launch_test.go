package config

import (
	"strings"
	"testing"
)

func TestMultiProviderTierSelectsEachBackendsLaunch(t *testing.T) {
	t.Parallel()

	body := strings.Replace(validConfig, "    provider: firecracker\n",
		"    providers: [firecracker, ec2]\n", 1)
	body = strings.Replace(body, "    image: ubuntu-2404-x64\n", `    launch:
      firecracker:
        image: ubuntu-2404-x64@verified
      ec2:
        image: ami-0123456789abcdef0
        command: [/usr/local/bin/billet-runner]
`, 1)

	cfg, err := Load(writeConfig(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	tier, ok := cfg.TierByLabel("billet-4vcpu-ubuntu-2404")
	if !ok {
		t.Fatal("the multi-provider tier was not loaded")
	}

	if got := tier.ImageFor(ProviderFirecracker); got != "ubuntu-2404-x64@verified" {
		t.Errorf("firecracker image = %q", got)
	}
	if got := tier.ImageFor(ProviderEC2); got != "ami-0123456789abcdef0" {
		t.Errorf("ec2 image = %q", got)
	}
	if got := tier.RunnerCommandFor(ProviderFirecracker); len(got) != 1 || got[0] != "./run.sh" {
		t.Errorf("firecracker command = %q, want the stock runner command", got)
	}
	if got := tier.RunnerCommandFor(ProviderEC2); len(got) != 1 ||
		got[0] != "/usr/local/bin/billet-runner" {
		t.Errorf("ec2 command = %q, want the AMI's runner entrypoint", got)
	}
}

func TestMultiProviderTierRefusesOneAmbiguousImage(t *testing.T) {
	t.Parallel()

	tier := Tier{
		Providers: []ProviderKind{ProviderFirecracker, ProviderEC2},
		Image:     "ubuntu-2404-x64@verified",
	}

	errs := tier.LaunchErrors("tier failover")
	if !errorsContain(errs, "a tier with multiple providers must set launch for each provider") {
		t.Fatalf("ambiguous image was not refused with a useful diagnostic: %v", errs)
	}
}

func TestLaunchMapMustMatchTheAcceptedProviders(t *testing.T) {
	t.Parallel()

	tier := Tier{
		Providers: []ProviderKind{ProviderFirecracker, ProviderEC2},
		Launch: map[ProviderKind]TierLaunch{
			ProviderFirecracker: {Image: "ubuntu-2404-x64@verified"},
			ProviderDocker:      {Image: "runner:latest"},
		},
	}

	errs := tier.LaunchErrors("tier failover")
	for _, want := range []string{
		"launch.ec2 is required because the tier accepts ec2",
		"launch.docker is set, but the tier does not accept docker",
	} {
		if !errorsContain(errs, want) {
			t.Errorf("errors %v do not contain %q", errs, want)
		}
	}
}

func TestLaunchMapRefusesAmbiguousTopLevelBootFields(t *testing.T) {
	t.Parallel()

	tier := Tier{
		Provider: ProviderEC2,
		Image:    "ami-top-level",
		Command:  []string{"./run.sh"},
		Launch: map[ProviderKind]TierLaunch{
			ProviderEC2: {Image: "ami-in-launch"},
		},
	}

	errs := tier.LaunchErrors("tier cloud")
	for _, want := range []string{
		"set either image or launch, not both",
		"set commands inside launch when launch is used",
	} {
		if !errorsContain(errs, want) {
			t.Errorf("errors %v do not contain %q", errs, want)
		}
	}
}

func errorsContain(errs []error, want string) bool {
	for _, err := range errs {
		if strings.Contains(err.Error(), want) {
			return true
		}
	}

	return false
}
