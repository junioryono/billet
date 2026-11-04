package app

import (
	"slices"
	"testing"

	"github.com/junioryono/billet/internal/config"
)

// A NODE REPORTS WHAT ITS PROVIDER LAUNCHES: the host's own for one that runs work
// on it, an EC2 node's shapes' declared architectures, a CodeBuild node's
// environment's, and none where billet builds no image for the host (ADR-022).
func TestANodeReportsWhatItsProviderLaunches(t *testing.T) {
	t.Parallel()

	ec2 := func(shapes ...config.EC2InstanceType) *config.Config {
		return &config.Config{Node: &config.NodeConfig{Provider: config.ProviderEC2,
			EC2: &config.EC2Config{InstanceTypes: shapes}}}
	}

	for _, tc := range []struct {
		name   string
		cfg    *config.Config
		goarch string
		want   []config.Arch
	}{
		{name: "firecracker on arm64", goarch: "arm64", want: []config.Arch{config.ArchARM64},
			cfg: &config.Config{Node: &config.NodeConfig{Provider: config.ProviderFirecracker}}},
		{name: "docker on amd64", goarch: "amd64", want: []config.Arch{config.ArchX64},
			cfg: &config.Config{Node: &config.NodeConfig{Provider: config.ProviderDocker}}},
		{name: "a host billet builds nothing for", goarch: "riscv64", want: nil,
			cfg: &config.Config{Node: &config.NodeConfig{Provider: config.ProviderFirecracker}}},
		{name: "ec2 declaring both, one twice", goarch: "amd64",
			want: []config.Arch{config.ArchARM64, config.ArchX64},
			cfg: ec2(config.EC2InstanceType{Type: "a", Arch: config.ArchX64},
				config.EC2InstanceType{Type: "b", Arch: config.ArchARM64},
				config.EC2InstanceType{Type: "c", Arch: config.ArchX64},
				config.EC2InstanceType{Type: "d"})},
		// THE ORCHESTRATOR'S OWN ARCHITECTURE IS NOT WHAT IT LAUNCHES.
		{name: "ec2 declaring none", goarch: "arm64", want: nil,
			cfg: ec2(config.EC2InstanceType{Type: "a"})},
		{name: "codebuild on ARM_CONTAINER", goarch: "amd64", want: []config.Arch{config.ArchARM64},
			cfg: &config.Config{Node: &config.NodeConfig{Provider: config.ProviderCodeBuild,
				CodeBuild: &config.CodeBuildConfig{EnvironmentType: config.CodeBuildARMContainer}}}},
	} {
		if got := NodeArchitectures(tc.cfg, tc.goarch); !slices.Equal(got, tc.want) {
			t.Errorf("%s: reported %v, want %v", tc.name, got, tc.want)
		}
	}
}
