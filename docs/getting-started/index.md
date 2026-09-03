# Get started

billet runs your GitHub Actions jobs on machines you control. This tutorial takes one Linux machine from nothing to a workflow completing on it, using the Docker backend because its only prerequisite is Docker. It takes about fifteen minutes, and roughly a third of that is in a browser.

Every step below was walked on a throwaway Ubuntu 24.04 host against a real organization. Where something is easy to get wrong, the page says so.

## What you need

- **A GitHub organization you own.** billet cannot onboard a personal account: the App is created against the organization, and the runners it manages are organization runners. If you are not an owner, an owner has to do the two browser steps.
- **A Linux machine with Docker** that you can reach a shell on. A laptop is fine. `billet check` deliberately never touches Docker, so run `docker ps` yourself first: a missing daemon or a socket your user cannot reach only shows up when `billet node` starts.
- **A browser that can reach the machine's loopback.** App registration completes through a callback on `127.0.0.1`. For a remote machine, forward the port with `ssh -L 8765:127.0.0.1:8765 <host>` and pass `--port 8765` when you create the App. Publishing the port does not work: the callback binds loopback only.

## The pages

| Page | Time | What you will have |
|---|---|---|
| [Installation](installation.md) | 1 min | the `billet` binary |
| [The GitHub side](github-side.md) | 5 min | a workflow-restricted runner group, and a GitHub App billet created for you |
| [Your first config](first-config.md) | 2 min | a `billet.yaml` that fits this machine |
| [Your first job](first-job.md) | 5 min | a workflow that ran on your own hardware |
| [Next steps](next-steps.md) | 1 min | where to go for a real deployment |

## What this is not

A single machine running both roles is the simplest shape and the right place to start. It is not highly available (the machine is the deployment), and jobs run in containers that share the host kernel, which is why the trust restrictions in the GitHub step are not optional. When you outgrow it, [Choose a shape](../deploying/choose-a-shape.md) covers a Firecracker host with real kernel isolation, a control plane on its own machine with nodes dialling in over mTLS, a Mac, and cloud fallback.
