package host

import (
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/config"
)

// A WINDOWS AMI ANSWERS TO ITS OWN CONTRACT: a current one is reported as that, and none
// of Linux's gaps, about Docker's store or the toolcache, is said of it. A Windows tier on
// another image, and any other tier on a Windows one, is a launch that is always refused,
// so `billet check` fails on it in both directions.
func TestTheCloudPreflightJudgesAWindowsAMIByItsOwnContract(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIDEXAMPLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")

	cfgFor := func(endpoint string, guest config.GuestOS) *config.Config {
		return wrapEC2(&config.EC2Config{
			Region:           "us-west-2",
			Endpoint:         endpoint,
			SubnetID:         "subnet-0abc",
			SecurityGroupIDs: []string{"sg-0abc"},
			InstanceTypes:    []config.EC2InstanceType{{Type: "c7i.2xlarge", VCPU: 8, Memory: 16 * config.GiB}},
		}, config.Tier{
			Label: "cloud", Provider: config.ProviderEC2, Trust: config.WorkloadTrusted, VCPU: 8,
			Memory: 16 * config.GiB, Image: "ami-good", GuestOS: guest,
		})
	}

	for _, tc := range []struct {
		name     string
		topology fakeEC2Topology
		guest    config.GuestOS
		wantOut  string
		wantErr  string
	}{
		{
			name:     "a stamped Windows AMI",
			topology: fakeEC2Topology{windowsImage: true, imageContract: "1"},
			guest:    config.GuestWindows,
			wantOut:  "ami-good available (Windows AMI contract 1)",
		},
		{
			name:     "an unstamped Windows AMI",
			topology: fakeEC2Topology{windowsImage: true, untaggedImage: true},
			guest:    config.GuestWindows,
			wantOut:  "`billet ami build --os windows`",
		},
		{
			name:     "a Windows tier on a Linux AMI",
			topology: fakeEC2Topology{},
			guest:    config.GuestWindows,
			wantErr:  "is guest_os windows and its AMI ami-good is not a Windows AMI",
		},
		{
			name:     "a Linux tier on a Windows AMI",
			topology: fakeEC2Topology{windowsImage: true, imageContract: "1"},
			guest:    config.GuestLinux,
			wantErr:  "is guest_os linux and its AMI ami-good is a Windows AMI",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			topology := tc.topology
			topology.accept = "AKIDEXAMPLE"
			topology.imageState = "available"

			var err error

			out := capture(t, func() {
				err = checkEC2Credentials(t.Context(), processEnv(), cfgFor(fakeEC2With(t, topology), tc.guest), nil, false, false)
			})

			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("the preflight answered %v, want %q", err, tc.wantErr)
				}

				return
			}

			if err != nil {
				t.Fatalf("the preflight failed: %v", err)
			}
			if !strings.Contains(out, tc.wantOut) {
				t.Errorf("the report does not say %q:\n%s", tc.wantOut, out)
			}
			if strings.Contains(out, "Docker image store") || strings.Contains(out, "toolcache") ||
				strings.Contains(out, "components") {
				t.Errorf("a Windows image is told Linux's gaps:\n%s", out)
			}
		})
	}
}
