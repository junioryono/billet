# billet/stickydisk

This action hot-attaches a copy-on-write ext4 volume to a billet Firecracker runner, mounts it for the job, and publishes a new immutable generation in its post step. Every failure is a warning and a cold cache: cache availability never changes the job result.

```yaml
- uses: junioryono/billet/actions/stickydisk@v0.1.0
  with:
    key: ${{ github.repository }}-npm
    path: ~/.npm
    size-gb: 10
```

Keys are used exactly as supplied so deliberately shared caches remain possible. Prefix ordinary keys with `${{ github.repository }}` to prevent unrelated repositories from colliding. A cache keeps the size chosen on first creation; choose enough headroom up front. Five volumes may be attached to one job.

The action needs a billet Firecracker guest with `sudo`, `curl`-reachable MMDS v2 metadata, `e2fsprogs`, and mount tools. On another runner it warns and continues without a cache.
