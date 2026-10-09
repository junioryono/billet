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
	if got := tier.RunnerCommandFor(ProviderFirecracker); len(got) != 1 || got[0] != "./billet-runner-service" {
		t.Errorf("firecracker command = %q, want the packaged runner wrapper", got)
	}
	if got := tier.RunnerCommandFor(ProviderEC2); len(got) != 1 ||
		got[0] != "/usr/local/bin/billet-runner" {
		t.Errorf("ec2 command = %q, want the AMI's runner entrypoint", got)
	}
}

func TestMultiProviderTierRefusesOneAmbiguousImage(t *testing.T) {
	t.Parallel()

	body := strings.Replace(validConfig, "    provider: firecracker\n",
		"    providers: [firecracker, ec2]\n", 1)

	_, err := Load(writeConfig(t, body))
	if err == nil || !strings.Contains(err.Error(),
		"a tier with multiple providers must set launch for each provider") {
		t.Fatalf("Load with one ambiguous image = %v, want a useful refusal", err)
	}
}

func TestLaunchMapMustMatchTheAcceptedProviders(t *testing.T) {
	t.Parallel()

	body := strings.Replace(validConfig, "    provider: firecracker\n",
		"    providers: [firecracker, ec2]\n", 1)
	body = strings.Replace(body, "    image: ubuntu-2404-x64\n", `    launch:
      firecracker:
        image: ubuntu-2404-x64@verified
      docker:
        image: runner:latest
`, 1)

	_, err := Load(writeConfig(t, body))
	if err == nil {
		t.Fatal("Load accepted a launch map that does not match the tier's providers")
	}

	for _, want := range []string{
		"launch.ec2 is required because the tier accepts ec2",
		"launch.docker is set, but the tier does not accept docker",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Load error %v does not contain %q", err, want)
		}
	}
}

func TestLaunchMapRefusesAmbiguousTopLevelBootFields(t *testing.T) {
	t.Parallel()

	body := strings.Replace(validConfig, "    image: ubuntu-2404-x64\n", `    image: ubuntu-2404-x64
    command: [./custom-runner]
    launch:
      firecracker:
        image: ubuntu-2404-x64@verified
`, 1)

	_, err := Load(writeConfig(t, body))
	if err == nil {
		t.Fatal("Load accepted both top-level and mapped launch fields")
	}

	for _, want := range []string{
		"set either image or launch, not both",
		"set commands inside launch when launch is used",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Load error %v does not contain %q", err, want)
		}
	}
}

// AN IMAGE THAT BEGINS WITH A DASH IS REFUSED, top-level or in launch, because
// a backend's command line would read it as an option (docker run takes
// "--network=host" as a flag and the next argument as the image); a dash
// anywhere else is an ordinary name.
func TestAnImageThatReadsAsAnOptionIsRefused(t *testing.T) {
	t.Parallel()

	top := strings.Replace(validConfig, "    image: ubuntu-2404-x64\n", "    image: --network=host\n", 1)
	if _, err := Load(writeConfig(t, top)); err == nil || !strings.Contains(err.Error(), `image "--network=host" begins with "-"`) {
		t.Fatalf("Load with a top-level image beginning with a dash = %v, want a refusal naming it", err)
	}

	launch := strings.Replace(validConfig, "    provider: firecracker\n", "    providers: [firecracker, ec2]\n", 1)
	launch = strings.Replace(launch, "    image: ubuntu-2404-x64\n", `    launch:
      firecracker:
        image: ubuntu-2404-x64@verified
      ec2:
        image: -ami-0123456789abcdef0
`, 1)
	if _, err := Load(writeConfig(t, launch)); err == nil ||
		!strings.Contains(err.Error(), `launch.ec2.image "-ami-0123456789abcdef0" begins with "-"`) {
		t.Fatalf("Load with a launch image beginning with a dash = %v, want a refusal naming it", err)
	}

	spaced := strings.Replace(validConfig, "    image: ubuntu-2404-x64\n", "    image: \" --help@verified\"\n", 1)
	if _, err := Load(writeConfig(t, spaced)); err == nil || !strings.Contains(err.Error(), `begins with "-"`) {
		t.Fatalf("Load with a dash behind a space = %v, want a refusal naming it", err)
	}

	inner := strings.Replace(validConfig, "    image: ubuntu-2404-x64\n", "    image: ubuntu-2404-x64-minimal\n", 1)
	if _, err := Load(writeConfig(t, inner)); err != nil {
		t.Fatalf("Load with a dash inside the image name = %v, want accepted", err)
	}
}
