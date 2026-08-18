// Package guestassets carries scripts installed into every managed runner image.
package guestassets

import _ "embed"

// DockerCacheScript mounts and settles the transparent Docker image store.
//
//go:embed docker-cache.sh
var DockerCacheScript string

// RunnerServiceScript retains the stock update loop without hiding one-job results.
//
//go:embed runner-service.sh
var RunnerServiceScript string
