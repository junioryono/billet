# Persistent Docker builds

`setup-docker-builder` starts one privileged BuildKit container inside the job's microVM and places its entire `/var/lib/buildkit` on a sticky disk. Layers and `RUN --mount=type=cache` contents therefore survive together; no cache export is involved. The builder is stopped before the disk is committed, and failed or cancelled jobs publish nothing.

```yaml
- id: billet-builder
  uses: junioryono/billet/actions/setup-docker-builder@v0.1.0
  with:
    cache-key: Dockerfile
- uses: docker/build-push-action@v7
  with:
    builder: ${{ steps.billet-builder.outputs.name }}
    context: .
```

The combined `junioryono/billet/actions/build-push-action@v0.1.0` wrapper is available for common `docker/build-push-action` inputs and follows upstream v7 rather than vendoring its implementation. Use an exact Billet release tag: the release workflow rewrites every sibling reference to that same immutable tag before it creates the tag, while `main` deliberately composes the current `main` actions. Use the setup action directly when an upstream input is not exposed by the wrapper.

BuildKit garbage collection keeps records unused for eight days and constrains the 100 GiB default disk to 80–90 GiB of retained state. The tier's `buildkit_cache_mount_limit` independently caps each `RUN --mount=type=cache` record: the post hook reports its exact size and growth, resets only a mount that exceeds the ceiling, and discards the whole publication if the ceiling cannot be enforced. Set `reset: true` on one trusted run to replace a poisoned or bloated key with fresh BuildKit state.

When the Firecracker node configures `registry_mirrors`, the builder routes `docker.io`, `ghcr.io`, and `quay.io` to their three site-local pull-through caches. The endpoints are public-cache infrastructure, not credential stores: every guest at the site can reach them.

Concurrent successful jobs serialize publication and the last post hook to publish becomes current; a few runs may be needed for unrelated concurrent layer sets to accumulate. Scope a key as broadly as the images share layers, and no broader.
