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
		{name: "localhost", server: "    listen: localhost:9180\n"},
		{name: "remote, asked for", server: "    listen: 10.0.0.4:9180\n    allow_remote: true\n"},
		{name: "pprof on loopback", node: "    listen: 127.0.0.1:9181\n    pprof: true\n"},
		{name: "no listen", server: "    pprof: true\n", refusal: "server.metrics.listen is required"},
		{name: "not host:port", node: "    listen: http://127.0.0.1:9181\n", refusal: "node.metrics.listen"},
		{name: "remote, not asked for", server: "    listen: 10.0.0.4:9180\n", refusal: "which is not loopback. The endpoint has no authentication"},
		{name: "wildcard, not asked for", node: "    listen: \":9181\"\n", refusal: "which is not loopback. The endpoint has no authentication"},
		{name: "pprof beside remote", server: "    listen: 0.0.0.0:9180\n    allow_remote: true\n    pprof: true\n", refusal: "the profiler is served on loopback only"},
		{name: "the node wire's socket", server: "    listen: 127.0.0.1:7717\n", refusal: "server.metrics.listen is \"127.0.0.1:7717\" and server.listen"},
		{name: "the other role's endpoint", server: "    listen: 127.0.0.1:9180\n", node: "    listen: 127.0.0.1:9180\n", refusal: "node.metrics.listen is \"127.0.0.1:9180\" and server.metrics"},
		{name: "the other role's wildcard", server: "    listen: 0.0.0.0:9180\n    allow_remote: true\n", node: "    listen: 127.0.0.1:9180\n", refusal: "the same socket"},
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
