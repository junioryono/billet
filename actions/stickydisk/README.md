# billet/stickydisk

This action hot-attaches a copy-on-write ext4 volume to a billet runner, mounts it for the job, and commits it in its post step, for the node to publish as a new immutable generation if the tier's rule allows. Every failure is a warning and a cold cache: cache availability never changes the job result.

```yaml
- uses: junioryono/billet/actions/stickydisk@v0.1.0
  with:
    key: ${{ github.repository }}-npm
    path: ~/.npm
    size-gb: 10
```

Keys are used exactly as supplied so deliberately shared caches remain possible. Prefix ordinary keys with `${{ github.repository }}` to prevent unrelated repositories from colliding. A cache keeps the size chosen on first creation; choose enough headroom up front. Five volumes may be attached to one job.

The action needs a billet guest with `sudo`, `e2fsprogs` and mount tools, and a cache endpoint: a Firecracker guest's `curl`-reachable MMDS v2 metadata, or the environment an EC2 guest's image gives it, on an AMI of contract 4 or later. On another runner it warns and continues without a cache.
