# Your first config

`billet init` writes a `billet.yaml` that runs, with nothing to hand-edit afterwards.

```bash
billet init \
  --org your-org \
  --runner-group billet-trusted \
  --workflow 'your-org/your-repo/.github/workflows/ci.yml@refs/heads/main' \
  --config ~/billet.yaml
```

It measures the machine and writes a capacity ceiling below what it found, leaving room for the kernel, Docker and your shell. It then keeps every generated tier that fits **individually** under that ceiling. The definitions are alternatives: admitted jobs share the bounded capacity, and discovery turns rotate across the catalogue. It picks a runner image that is actually pullable, points the state directories somewhere writable, and describes both roles in one file.

The App ids are `0` because the App does not exist yet; [creating the App](github-side.md) fills them in.

## What the file says

```yaml
server:
  listen: 127.0.0.1:7717      # loopback: the two processes talk over plain HTTP
  state_dir: ~/.local/state/billet/server
  max_vcpu: 6                 # the ceiling init measured, minus headroom
  max_memory: 12GiB

github:
  org: your-org
  app_id: 0                   # written by billet github-app create
  installation_id: 0
  private_key_path: ~/.local/state/billet/app-private-key.pem

node:
  server_addr: 127.0.0.1:7717
  provider: docker
  state_dir: ~/.local/state/billet/node

tiers:
  - label: billet-2vcpu       # the runs-on value
    trust: trusted
    runner_group: billet-trusted
    workflows:
      - your-org/your-repo/.github/workflows/ci.yml@refs/heads/main
    vcpu: 2
    memory: 4GiB
    image: ghcr.io/actions/actions-runner:latest
```

The shape is the whole product in miniature: a `server` block for the control plane, a `github` block for the App, a `node` block for this machine as a compute host, and `tiers`, where each tier is one `runs-on` label and one GitHub scale set. [Configuration](../reference/configuration.md) documents every key.

## Accept the licensed image components (Linux)

A Linux guest boots GitHub's own runner image, built from its template, except for five parts whose licenses forbid billet redistributing them: the Android SDK, the CodeQL bundle, Firefox, Google Chrome and Microsoft Edge. Your deployment installs the ones it accepts, from their vendors, on its own nodes. **Decide this while you set up, before the first image is pulled**: a component you do not accept is missing from every image this deployment builds from billet's published image, and a job that uses it fails, or downloads it itself on every run.

Read each license in the [table of components](../operating/guest-images.md#components-the-image-leaves-out). To boot the same image GitHub's hosted `ubuntu-24.04` runner does, accept all five, if you agree to them:

```yaml
images:
  accept: [android-sdk, codeql, firefox, google-chrome, microsoft-edge]
```

Writing a name is accepting that component's license for this deployment, so billet never writes one for you: `billet init` writes the list empty, with each name and its license above it. Put it in the control plane's file, which hands the list to every node when it registers; a node also installs what its own file names.

- **Firecracker.** A node installs what is accepted in a microVM before it imports each new generation, and `billet check` names what each tier's image left out that is not accepted. To accept more later, add the names and restart `billet server`, so that each node registers again and records the new list (or write them in the node's own file as well). The node's next `billet images pull --verify`, or its daily refresh where automatic updates are on, then completes a new generation with what was added.
- **EC2.** `billet ami build` installs what is accepted into the AMI. To accept more later, build a new AMI and point the tiers at it; nothing refreshes an AMI by itself.

A Mac is different. billet publishes no macOS image, and a Mac's tart image is whatever you give it ([Run jobs on a Mac](../deploying/mac-tart.md)), so `images.accept` installs nothing there.

## Two rules about the file

**Re-running `init` converges what it owns and refuses to guess about the rest.** If the file carries content it cannot merge for you (edited values, sites, extra tiers), it writes `billet.yaml.new` beside it for a deliberate comparison instead of touching the original; `--force` overwrites outright. `billet github-app create --config` follows the opposite rule: it edits a single block into a file you already have. Each command states which rule it is following before it acts.

**`--config` is not optional.** billet does not read a `billet.yaml` from the working directory, because a server started from a directory someone else can write to would otherwise adopt their config, which chooses the state directory, the App key path and every tier's resources. Without the flag it reads your user config directory; `billet check -h` prints the path.

## Other generations

- `--profile local-service` writes the packaged paths (`/etc/billet`, `/var/lib/billet`) for a machine that will run the systemd units or launch agents.
- `--provider tart --node-name <this-mac>` writes a Mac config, and is refused anywhere but on the Mac itself, because the ceiling, the paths and the images all come from the machine running the command.
- `--provider firecracker`, `--provider ec2` and `--provider codebuild` write the block each backend needs; the [deploying](../deploying/choose-a-shape.md) guides show them. A Firecracker config also carries `images.accept: []`, which you fill in as [above](#accept-the-licensed-image-components-linux): nothing is installed unless it is named.
- `--emit ansible` prints the `billet_config` block for an Ansible inventory instead of writing a file.

## Next

Back to [the GitHub side](github-side.md) to create the App, then [your first job](first-job.md).
