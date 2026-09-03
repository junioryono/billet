package config

import (
	"strings"
	"testing"
)

// unpinnedMacOSConfig is one macOS label served by an owned Mac and a managed
// fleet: the topology a pinned tier cannot express, with the two node policies
// the guard counts the label against.
func unpinnedMacOSConfig(nodes, tierExtra string) string {
	return `
server:
  listen: 127.0.0.1:7717
  state_dir: /var/lib/billet/server
  max_vcpu: 32
  max_memory: 128GiB
github:
  org: acme
  app_id: 12345
  installation_id: 67890
  private_key_path: /etc/billet/app.pem
node:
  name: mac-1
  server_addr: 127.0.0.1:7717
  provider: tart
  state_dir: /var/lib/billet/node
  max_vcpu: 8
  max_memory: 16GiB
nodes:
` + nodes + `
tiers:
  - label: billet-macos
    providers: [tart, codebuild]
    guest_os: macos
    vcpu: 4
    memory: 8GiB
` + tierExtra + `
    launch:
      tart:
        image: ghcr.io/cirruslabs/macos-tahoe-xcode:latest
      codebuild:
        image: aws/codebuild/macos-arm-base:14
`
}

const bothMacHosts = `  - name: mac-1
    provider: tart
    guest_os: [macos]
  - name: cb-mac
    provider: codebuild
    guest_os: [macos]
    macos_vm_limit: 1
`

// A macOS TIER MAY LEAVE ITS NODE UNNAMED ONLY TO SPAN SEVERAL BACKENDS, and
// then it is counted against the hosts those backends declare: the owned Mac at
// Apple's two, the fleet's node at the capacity it declares, and max_concurrent
// defaulting to — and bounded by — what they permit between them.
func TestAnUnpinnedMacOSTierIsCountedAgainstItsBackendsDeclaredHosts(t *testing.T) {
	cfg, err := Load(writeConfig(t, unpinnedMacOSConfig(bothMacHosts, "")))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Tiers[0].MaxConcurrent; got != DefaultMacOSVMLimit+1 {
		t.Fatalf("max_concurrent defaulted to %d, want the two hosts' %d between them", got, DefaultMacOSVMLimit+1)
	}

	for _, tc := range []struct {
		name  string
		nodes string
		tier  string
		want  string
	}{
		{"one backend still needs the pin", "", "", "requires an explicit node"},
		{"a reservation has no host to hold it on", bothMacHosts, "    reserved: 1\n", "cannot reserve"},
		{"more concurrency than the hosts permit", bothMacHosts, "    max_concurrent: 4\n", "between 1 and 3"},
		{"the fleet's node must declare its capacity",
			"  - name: mac-1\n    provider: tart\n    guest_os: [macos]\n  - name: cb-mac\n    provider: codebuild\n    guest_os: [macos]\n",
			"", "set macos_vm_limit for it to that fleet's capacity"},
		{"a backend with no declared host",
			"  - name: mac-1\n    provider: tart\n    guest_os: [macos]\n", "",
			"no nodes[] entry declares a codebuild host"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := unpinnedMacOSConfig(tc.nodes, tc.tier)
			if tc.name == "one backend still needs the pin" {
				body = strings.Replace(unpinnedMacOSConfig(bothMacHosts, ""),
					"providers: [tart, codebuild]", "provider: tart", 1)
				body = strings.Replace(body, "    launch:\n      tart:\n        image: ghcr.io/cirruslabs/macos-tahoe-xcode:latest\n      codebuild:\n        image: aws/codebuild/macos-arm-base:14\n",
					"    image: ghcr.io/cirruslabs/macos-tahoe-xcode:latest\n", 1)
			}
			got := loadErr(t, body)
			if !strings.Contains(got, tc.want) {
				t.Errorf("Load = %s\nwant an error containing %q", got, tc.want)
			}
		})
	}
}
