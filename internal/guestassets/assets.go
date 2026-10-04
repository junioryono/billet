// Package guestassets carries scripts installed into every managed runner image.
package guestassets

import _ "embed"

// DockerCacheScript mounts and settles the transparent Docker image store.
//
//go:embed docker-cache.sh
var DockerCacheScript string

// ActionsProxyScript keeps non-results HTTPS independent from node interception.
//
//go:embed actions-proxy.py
var ActionsProxyScript string

// DNSUpstreamsScript filters resolver upstreams so no value dockerd rejects reaches
// daemon.json, which would stop the daemon and turn a lost cache remap into a dead job.
//
//go:embed dns-upstreams.py
var DNSUpstreamsScript string

// RunnerServiceScript retains the stock update loop without hiding one-job results.
//
//go:embed runner-service.sh
var RunnerServiceScript string

// ExecEnvScript execs the runner with an environment read from descriptor 3, so
// the registration and the cache bearer never appear in an argument list.
//
//go:embed exec-env.sh
var ExecEnvScript string
