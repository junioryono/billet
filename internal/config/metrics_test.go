package config

import (
	"strings"
	"testing"
)

// withMetrics adds a metrics block to validConfig's server and node sections;
// an empty block string leaves that role without one.
func withMetrics(t *testing.T, server, node string) string {
	t.Helper()

	body := validConfig

	if server != "" {
		if !strings.Contains(body, "  listen: 127.0.0.1:7717\n") {
			t.Fatal("validConfig no longer has the server listen this test anchors to")
		}

		body = strings.Replace(body, "  listen: 127.0.0.1:7717\n", "  listen: 127.0.0.1:7717\n  metrics:\n"+server, 1)
	}

	if node != "" {
		if !strings.Contains(body, "  server_addr: 127.0.0.1:7717\n") {
			t.Fatal("validConfig no longer has the node server_addr this test anchors to")
		}

		body = strings.Replace(body, "  server_addr: 127.0.0.1:7717\n", "  server_addr: 127.0.0.1:7717\n  metrics:\n"+node, 1)
	}

	return body
}

// NO BLOCK, NO ENDPOINT: the zero configuration serves nothing.
func TestMetricsAreOffWithoutABlock(t *testing.T) {
	t.Parallel()

	cfg, err := Load(writeConfig(t, validConfig))
	if err != nil {
		t.Fatal(err)
	}

	if cfg.Server.Metrics != nil || cfg.Node.Metrics != nil {
		t.Errorf("a config without a metrics block has one: server %+v, node %+v",
			cfg.Server.Metrics, cfg.Node.Metrics)
	}
}

// What each role's block may say, and what it may not.
func TestTheMetricsEndpointIsChecked(t *testing.T) {
	t.Parallel()

	for _, c := range []struct {
		name, server, node string
		refusal            string // "" means it loads
	}{
		{name: "server on loopback", server: "    listen: 127.0.0.1:9180\n"},
		{name: "node on loopback", node: "    listen: 127.0.0.1:9181\n"},
		{name: "both, on their own ports", server: "    listen: 127.0.0.1:9180\n", node: "    listen: \"[::1]:9181\"\n"},
		{name: "an IPv4-mapped loopback", server: "    listen: \"[::ffff:127.0.0.1]:9180\"\n    pprof: true\n"},
		{name: "remote, asked for", server: "    listen: 10.0.0.4:9180\n    allow_remote: true\n"},
		{name: "localhost, asked for", server: "    listen: localhost:9180\n    allow_remote: true\n"},
		{name: "pprof on loopback", node: "    listen: 127.0.0.1:9181\n    pprof: true\n"},
		{name: "pprof on loopback beside allow_remote", node: "    listen: 127.0.0.1:9181\n    allow_remote: true\n    pprof: true\n"},
		{name: "no listen", server: "    pprof: true\n", refusal: "server.metrics.listen is required"},
		{name: "not host:port", node: "    listen: http://127.0.0.1:9181\n", refusal: "node.metrics.listen"},
		{name: "padded", server: "    listen: \" 127.0.0.1:9180\"\n", refusal: "has whitespace around it"},
		{name: "padded, remote asked for", server: "    listen: \"10.0.0.4:9180 \"\n    allow_remote: true\n", refusal: "has whitespace around it"},
		{name: "remote, not asked for", server: "    listen: 10.0.0.4:9180\n", refusal: "which is not a loopback address"},
		{name: "localhost, not asked for", server: "    listen: localhost:9180\n", refusal: "a name such as localhost is not accepted"},
		{name: "wildcard, not asked for", node: "    listen: \":9181\"\n", refusal: "which is not a loopback address"},
		{name: "pprof beside remote", server: "    listen: 0.0.0.0:9180\n    allow_remote: true\n    pprof: true\n", refusal: "the profiler is served on 127.0.0.1 or [::1] only"},
		{name: "pprof on localhost", node: "    listen: localhost:9181\n    allow_remote: true\n    pprof: true\n", refusal: "the profiler is served on 127.0.0.1 or [::1] only"},
		{name: "the node wire's socket", server: "    listen: 127.0.0.1:7717\n", refusal: "server.metrics.listen is \"127.0.0.1:7717\" and server.listen"},
		{name: "the node wire's socket, mapped", server: "    listen: \"[::ffff:127.0.0.1]:7717\"\n", refusal: "and server.listen"},
		{name: "the node wire's socket, zero-padded port", server: "    listen: 127.0.0.1:07717\n", refusal: "and server.listen"},
		{name: "the other role's endpoint", server: "    listen: 127.0.0.1:9180\n", node: "    listen: 127.0.0.1:9180\n", refusal: "node.metrics.listen is \"127.0.0.1:9180\" and server.metrics"},
		{name: "the other role's wildcard", server: "    listen: 0.0.0.0:9180\n    allow_remote: true\n", node: "    listen: 127.0.0.1:9180\n", refusal: "the same socket"},
		{name: "the other role's expanded wildcard", server: "    listen: \"[0:0:0:0:0:0:0:0]:9180\"\n    allow_remote: true\n", node: "    listen: 127.0.0.1:9180\n", refusal: "the same socket"},
		{name: "an unknown key", server: "    listen: 127.0.0.1:9180\n    path: /metrics\n", refusal: "path"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			_, err := Load(writeConfig(t, withMetrics(t, c.server, c.node)))

			switch {
			case c.refusal == "" && err != nil:
				t.Errorf("refused: %v", err)
			case c.refusal != "" && err == nil:
				t.Errorf("accepted; want a refusal saying %q", c.refusal)
			case c.refusal != "" && !strings.Contains(err.Error(), c.refusal):
				t.Errorf("the refusal does not say %q: %v", c.refusal, err)
			}
		})
	}
}

// The values reach the struct a role reads.
func TestTheMetricsBlockIsWhatTheRoleReads(t *testing.T) {
	t.Parallel()

	cfg, err := Load(writeConfig(t, withMetrics(t, "    listen: 127.0.0.1:9180\n    pprof: true\n",
		"    listen: 10.0.0.5:9181\n    allow_remote: true\n")))
	if err != nil {
		t.Fatal(err)
	}

	if got := *cfg.Server.Metrics; got != (MetricsConfig{Listen: "127.0.0.1:9180", Pprof: true}) {
		t.Errorf("server.metrics = %+v", got)
	}

	if got := *cfg.Node.Metrics; got != (MetricsConfig{Listen: "10.0.0.5:9181", AllowRemote: true}) {
		t.Errorf("node.metrics = %+v", got)
	}
}

// The listeners the file-level table cannot stand up cheaply (enrollment needs
// an mTLS wire, a cache a provider that serves one), asked of the validator
// directly, in both directions.
func TestTheMetricsEndpointKeepsOffEveryOtherListener(t *testing.T) {
	t.Parallel()

	for _, c := range []struct {
		name    string
		cfg     Config
		overlap string // the key named beside the metrics one; "" means accepted
	}{
		{
			name: "the enrollment listener",
			cfg: Config{Server: &ServerConfig{Listen: "10.0.0.4:7717", BootstrapListen: "10.0.0.4:7718",
				Metrics: &MetricsConfig{Listen: "0.0.0.0:7718", AllowRemote: true}}},
			overlap: "server.bootstrap_listen",
		},
		{
			name: "beside the enrollment listener",
			cfg: Config{Server: &ServerConfig{Listen: "10.0.0.4:7717", BootstrapListen: "10.0.0.4:7718",
				Metrics: &MetricsConfig{Listen: "127.0.0.1:9180"}}},
		},
		{
			name: "the node's cache listener",
			cfg: Config{Node: &NodeConfig{Cache: &NodeCacheConfig{Listen: "172.16.0.1:9200"},
				Metrics: &MetricsConfig{Listen: "[::ffff:172.16.0.1]:9200", AllowRemote: true}}},
			overlap: "node.cache.listen",
		},
		{
			name: "beside the node's cache listener",
			cfg: Config{Node: &NodeConfig{Cache: &NodeCacheConfig{Listen: "172.16.0.1:9200"},
				Metrics: &MetricsConfig{Listen: "127.0.0.1:9200"}}},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			errs := c.cfg.validateMetrics()

			switch {
			case c.overlap == "" && len(errs) != 0:
				t.Errorf("refused: %v", errs)
			case c.overlap != "" && len(errs) != 1:
				t.Errorf("want one refusal naming %s, got %v", c.overlap, errs)
			case c.overlap != "" && !strings.Contains(errs[0].Error(), "and "+c.overlap+" is"):
				t.Errorf("the refusal does not name %s: %v", c.overlap, errs[0])
			}
		})
	}
}

// One socket, however its address is spelled; and two sockets are two.
func TestAddressesOverlapOnTheSocketNotTheSpelling(t *testing.T) {
	t.Parallel()

	for _, c := range []struct {
		a, b string
		same bool
	}{
		{"127.0.0.1:9180", "127.0.0.1:9180", true},
		{"127.0.0.1:9180", "[::ffff:127.0.0.1]:9180", true},
		{"127.0.0.1:9180", "127.0.0.1:09180", true},
		{"[::1]:9180", "[0:0:0:0:0:0:0:1]:9180", true},
		{"[0:0:0:0:0:0:0:0]:9180", "10.0.0.4:9180", true},
		{"[::1%0]:9180", "[::1]:9180", true},
		{"[::1]:9180", "[::1%0]:9180", true},
		{"[::%0]:9180", "127.0.0.1:9180", true},
		{"127.0.0.1:9180", "[::%0]:9180", true},
		{"[fe80::1%eth0]:9180", "[fe80::1%eth1]:9180", false},
		// A zone that names a link is compared as written, never resolved: two
		// spellings that might be one interface are left to the bind.
		{"[fe80::1%2]:9180", "[fe80::1%02]:9180", false},
		{"[fe80::1%2]:9180", "[fe80::1%3]:9180", false},
		{":9180", "[::1]:9180", true},
		{"Billet.Example:9180", "billet.example:9180", true},
		{"127.0.0.1:9180", "127.0.0.1:9181", false},
		{"127.0.0.1:9180", "127.0.0.2:9180", false},
		{"[::1]:9180", "127.0.0.1:9180", false},
		{"127.0.0.1:x", "127.0.0.1:x", false},
	} {
		if got := addressesOverlap(c.a, c.b); got != c.same {
			t.Errorf("addressesOverlap(%q, %q) = %v, want %v", c.a, c.b, got, c.same)
		}
	}
}
