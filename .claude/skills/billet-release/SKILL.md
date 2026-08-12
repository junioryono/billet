---
name: billet-release
description: "Cutting a billet release and updating a running host — coverage expectations, the tag-driven release pipeline, what the packages install, and how an operator upgrades without killing running jobs. Use when tagging a release, changing .goreleaser.yml or the systemd units, or answering how an upgrade behaves."
---

# Releases and upgrades

## Releases

Semantic version tags, because Go gives no choice: a module version must be `vX.Y.Z`, so a date-stamped tag is not a version Go will ever resolve. Staying on `v0` also keeps the module path free of a `/vN` suffix, which is required from major version 2 onwards.

One branch per MINOR version, carrying every patch on it:

```
main ──●──●──●──●──●──●──▶
        \              \
         \              release/v0.4 ── v0.4.0
          release/v0.3 ─┬─ v0.3.0   (cut)
                        ├─ v0.3.1   (hotfix)
                        └─ v0.3.2   (hotfix)
```

Cut with the **Cut Release** workflow (Actions → Run workflow). Blank version cuts the next minor; supply one to bump deliberately. A hotfix is a commit on the existing `release/vX.Y` branch and a press of the same button with the patch version typed in — then **merge that branch back into main**, or the next release reverts the fix.

`cut-release.yml` creates the tag and CALLS `release.yml` rather than relying on the tag push to trigger it. A ref pushed with `GITHUB_TOKEN` does not start another workflow — GitHub's recursion guard — which is why a release button usually needs a PAT or an App. A `workflow_call` is not an event, so billet needs no repository secrets.

## Updating a running host

**No release has been cut, so there is no package to install yet** — the pipeline is built and untriggered. Until then this is a `go build` and a restart.

```bash
sudo dpkg -i billet_NEW_linux_amd64.deb
sudo systemctl restart billet-server
```

The restart is safe because billet drains — SIGTERM stops it taking new work and waits for the jobs already running. It is NOT instant: it takes as long as the longest job still running, up to `drain_timeout`. A second signal (`systemctl kill --kill-whom=main --signal=SIGTERM`) stops the waiting and tears down properly — which DESTROYS the jobs still running and fails those builds, because GitHub does not requeue a job whose runner vanished mid-execution. A third gives up where it stands.

## Deployment

`deploy/` holds the systemd units and the packaged config; GoReleaser's `nfpms` section turns them into `.deb` and `.rpm` (not `.apk`: Alpine uses BusyBox `adduser` and OpenRC, so a package built from these scripts and units could not install). Three things there are load-bearing and were each found by installing a package rather than by reading one:

- **`--config /etc/billet/billet.yaml` is explicit in both units.** billet's default config path is per-user and deliberately never reads the working directory; a unit that relied on the default would find nothing.
- **`lock_dir` is set in the packaged config.** billet derives its default from `HOME`, which a service does not have, so without this billet refuses to start rather than run without the lock that stops two processes managing one host's containers.
- **`TimeoutStopSec` is sized from `drain_timeout` plus the teardown.** systemd's expiry is a SIGKILL through the middle of the shutdown. Lower `drain_timeout` first, then the unit; never the unit alone.

The package **does not enable or start anything**. `/var/lib/billet` is created by postinstall rather than shipped, so a package removal cannot delete the deployment identity and the mTLS CA.
