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

// DockerDaemonJSON is the daemon configuration GitHub's template installs in place
// of its own, so the image store is the classic one the Docker cache mounts. The
// guest build reads it from the tree; the EC2 builder is sent it.
//
//go:embed docker-daemon.json
var DockerDaemonJSON string

// DockerShimScript sits ahead of the real docker client and points a build's BuildKit
// cache client at the guest's adapter.
//
//go:embed docker-shim.sh
var DockerShimScript string

// The container hook's wrapper, the pin of GitHub's hook it wraps, and that hook's
// notices, for an image built from a binary rather than a checkout.
var (
	//go:embed container-hook.js
	ContainerHookScript string
	//go:embed container-hook.pin
	ContainerHookPin string
	//go:embed container-hook.LICENSE
	ContainerHookLicense string
	//go:embed container-hook.NOTICE
	ContainerHookNotice string
)
