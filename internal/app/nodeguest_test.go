package app

import (
	"testing"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/provider"
	storecontract "github.com/junioryono/billet/internal/store"
)

// THE NODE'S GUEST CACHE SERVICE TAKES ITS GUESTS' REPORTS EXACTLY WHEN
// node.monitoring.guest SAYS SO: not for a node with no monitoring block, and not
// for one whose block measures the host alone.
func TestGuestMonitoringIsTheConfigsGuestSwitch(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		monitoring *config.NodeMonitoringConfig
		want       bool
	}{
		"no monitoring block":  {nil, false},
		"the host alone":       {&config.NodeMonitoringConfig{RAPL: true}, false},
		"the guest's own view": {&config.NodeMonitoringConfig{Guest: true}, true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			cfg := &config.Config{Node: &config.NodeConfig{
				StateDir:   t.TempDir(),
				Cache:      &config.NodeCacheConfig{Listen: "172.20.0.1:7718", GuestEndpoint: "http://172.20.0.1:7718"},
				Monitoring: tc.monitoring,
			}}

			// NOTHING HERE IS CALLED: building the service asks none of them.
			service, err := NewNodeCacheService(cfg, "deployment-1", struct{ storecontract.Store }{},
				struct{ provider.VolumeAttacher }{}, struct{ NodeCacheControl }{})
			if err != nil {
				t.Fatalf("NewNodeCacheService: %v", err)
			}

			if got := service.GuestMonitoring(); got != tc.want {
				t.Errorf("the service takes guest reports %t, want %t", got, tc.want)
			}
		})
	}
}
