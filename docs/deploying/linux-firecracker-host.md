# A Linux Firecracker host

One job, one microVM, on bare metal. Every guest boots its own kernel under the jailer (chrooted, dropped to its own unprivileged uid, in a cgroup, under seccomp), from a copy-on-write clone of a golden image that lives in a Ceph pool on the host's own disks, and is destroyed with the job. A guest boots, takes its registration and runs a container in about ten seconds.

## What the host needs

- Linux with `/dev/kvm`, on hardware you control. The [reference host](../reference/reference-hardware.md) is what every number in these docs is measured against.
- Firecracker and the jailer, installed as a versioned binary behind a stable symlink.
- A Ceph cluster the node can map RBD images from, with two pools (images and caches) and clone v2 enabled: `ceph osd set-require-min-compat-client mimic`. `billet check` refuses a cluster still cloning the old way, because a cache generation any running job holds would be undeletable there ([ADR-003](../reference/decisions/adr-003-ceph-rbd.md)).
- Two bridges: one for trusted guests, and a separate `untrusted_bridge` for fork pull requests that cannot reach the host, private networks, link-local addresses or the Ceph cluster. A microVM isolates the kernel, not the network.
- A guest kernel and a pulled guest image ([Guest images](../operating/guest-images.md)).

## Use the Ansible role

`junioryono.billet.host` turns an installed Linux machine into a control plane and Firecracker compute host over SSH, and it is the only supported way to build one repeatably. It converges the billet binary, the service account, the units, verified Firecracker binaries, the Ceph client (or, when you explicitly name the monitor address and every disk it may consume, a single-host bootstrap), the guest networks and their firewall policy, the configuration, `billet check`, and transactional host upgrades. It can enable the server and node independently, refuses to start either before `billet check` passes, and never replaces an existing App key implicitly.

You supply what it cannot safely guess:

| Variable | What it is |
|---|---|
| `billet_version` or `billet_binary_src` | an exact release the host fetches and verifies, or a binary you built; `latest` is refused |
| `billet_config` | the whole `billet.yaml`, which `billet init --provider firecracker --emit ansible` prints for you |
| `billet_github_private_key_src` | the App key created by `billet github-app create` |
| `billet_networks`, `billet_guest_dns_servers` | the bridges and what guests may reach |
| `billet_ceph_*` | client credentials, or the explicit bootstrap facts |
| `billet_ledger_volume_id` | on AWS, the module's ledger volume, mounted fail-closed |
| `billet_firecracker_version` and checksums | the Firecracker release to install |

The collection's [README](https://github.com/junioryono/billet/blob/main/ansible_collections/junioryono/billet/README.md) documents every variable and the [firecracker-host example](https://github.com/junioryono/billet/tree/main/ansible_collections/junioryono/billet/examples/firecracker-host) is a complete inventory. The first converge against a fresh machine is the documented first step (`--check --diff`, then apply).

## The config

```yaml
node:
  provider: firecracker
  site: home
  state_dir: /var/lib/billet/node
  firecracker:
    binary_path: /usr/local/bin/firecracker
    jailer_path: /usr/local/bin/jailer
    kernel_image: /var/lib/billet/kernels/vmlinux-…     # fallback; a pulled generation boots its own
    bridge: br-billet
    untrusted_bridge: br-billet-untrusted
  ceph:
    conf_path: /etc/ceph/ceph.conf
    user: billet              # never admin
    keyring_path: /etc/ceph/ceph.client.billet.keyring
    image_pool: billet-images
    cache_pool: billet-cache
  cache:
    listen: 10.200.0.1:7719   # the guest-facing cache endpoint, on the bridge

sites:
  - name: home
    store: ceph

tiers:
  - label: billet-8vcpu-ubuntu-2404
    provider: firecracker
    trust: untrusted
    vcpu: 8
    memory: 32GiB
    disk: 160GiB
    image: ubuntu-2404-x64@verified
    intercept: true           # transparent Actions cache, Linux Firecracker only
    cache_scope: repository
```

`node.ceph` is a sibling of `node.firecracker` rather than a field inside it, because two hosts in one place map the same pools. `node.ceph.user` defaults to `billet` and `admin` is refused, since an admin key can delete a pool. `image: …@verified` resolves to the newest generation proved to boot, so a fleet takes up a new image with no config edit; `disk` grows the job's clone before boot and the golden image stays small.

## Images

```bash
billet images pull ubuntu-2404-x64          # signed manifest, staged and verified, then imported
billet images verify ubuntu-2404-x64@<gen>  # boot one, make the guest prove it, record it
billet images list
```

A pull verifies the manifest's Sigstore signature against billet's publication workflow before parsing it, checks every asset against the digests the manifest names, stages and verifies on disk before importing into the cluster, and installs the paired kernel durably before publishing the generation. [Guest images](../operating/guest-images.md) covers promotion, the kernel pair, reaping and the runner-release deadline.

## Two nodes at one site

A second Linux host at the same site maps the same pools, reuses the generations the first one published, and takes overflow under the same label. That sharing is the whole reason Ceph replaced ZFS, and it was proved on real hosts: two nodes at one site reuse one generation, a second site starts cold, and the same label falls back across sites ([Site acceptance](../reference/records/site-acceptance.md)). Give each host its own node name and its own certificate ([Adding and removing nodes](../operating/nodes.md)).

## What a node costs the host

`billet-node` runs as root: Firecracker needs TAP devices, block-device nodes, cgroups, chroots and the ability to signal a VMM after proving the pid still names that microVM. The jailer drops every VMM to its own uid before Firecracker reads anything the guest controls. The unit keeps `ProtectSystem=strict` with only `/srv/jailer`, the state directory and the runtime directory writable. A guest's root disk grows per job to the tier's `disk`; the cache reserves five hot-attach slots per guest.
