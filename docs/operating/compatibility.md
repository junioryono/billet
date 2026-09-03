# Moving a workflow to billet

The contract is that a workflow moves to billet by changing `runs-on` and nothing else. This page says, feature by feature, whether that is measured, believed, or a known limit, and names the conformance job or test that measures it. A row is added only with a measurement to point at; a row without one says so. The conformance workflow is the one `billet cache conformance install` renders into a consumer repository, so every job named here runs on the consumer's own tier.

| Feature | Status | Measured by |
|---|---|---|
| Hosted-runner conventions third-party actions branch on (`ImageOS`, `ImageVersion`, `RUNNER_TOOL_CACHE`, `AGENT_TOOLSDIRECTORY`, passwordless `sudo`, the `docker` group, `HOME` for the runner account) | Measured | the image gate `scripts/check-guest-image.sh`, run before every guest image is published |
| The software GitHub's declaration names, at the pinned versions | Measured | the image gate, and `docs/reference/decisions/adr-005-runner-image-parity.md` for what is still missing |
| `actions/cache` from host steps | Measured | `save-host`, `restore-host` |
| `actions/cache` inside a `container:` job | Measured | `save-container`, `restore-container` |
| `actions/upload-artifact` and `download-artifact` | Measured | `save-host`, `restore-container` |
| `services:` reached from host steps through the mapped port, with a health check | Measured | `services-host` |
| `services:` reached by hostname from inside a `container:` job | Measured | `services-container` |
| `docker compose up` with a database from a host step | Measured | `compose-database` |
| Service images served from the site's Docker image store on the next job | Reported, not asserted | `container-init-timings` (both `Initialize containers` durations in the run summary) |
| `docker/build-push-action` with `cache-to: type=gha` on a container-driver builder, nothing billet-specific named | Measured | `buildkit-gha` |
| The same, naming the adapter with `url_v2` | Measured | `loopback-v2-save`, `loopback-v2-restore`, `buildkit-loopback-export`, `buildkit-loopback-import` |
| A `docker` client inside a `container:` job exporting `type=gha`, nothing billet-specific named | Measured | `container-docker-buildkit` |
| `curl`, Python's standard library, Go's `net/http` reaching the results origin | Measured | `client-trust` |
| Python `requests` (certifi) reaching the results origin | Measured | `client-trust`, through `REQUESTS_CA_BUNDLE` published by the job hook |
| `sccache` with the Actions cache backend | Measured, not accelerated | `client-trust`: it reaches the origin and caches; a client billet does not admit by user agent is spliced to GitHub by design |
| Java clients (a custom truststore) reaching the results origin | Not measured | no conformance job; Gradle's build cache does not use the Actions cache |
| The kill switch putting a repository back on GitHub's cache | Measured | `loopback-passthrough` and the `mode: passthrough` run |
| A poisoned or faulting cache never failing a job | Measured | `poisoned-clients`, `restore-poison-artifact` |
| Untrusted (fork) work isolated from the trusted cache | Measured on Firecracker | `internal/e2e/lifecycle_test.go`; EC2 and Tart are tracked in the repository's issues |
| macOS and Linux arm64 jobs on an owned Mac | Measured | `internal/provider/tart/realguest_test.go` and the acceptance records under `docs/reference/records/` |
| Anything on a tier that is not Linux Firecracker getting an accelerated `actions/cache` | Known limit | interception is refused there; tracked in the repository's issues |

Two things this page does not promise. Speed is measured and reported by the jobs above, never asserted, because a cold store and a warm one are both correct. And every "Measured" row is measured on the reference hardware named in `docs/reference/reference-hardware.md`; a different backend is a different measurement.
