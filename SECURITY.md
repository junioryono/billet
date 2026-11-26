# Security policy

## Reporting a vulnerability

Report it privately through GitHub's private vulnerability reporting: the repository's **Security** tab, then **Report a vulnerability**. Do not open a public issue or pull request for it.

Say what an attacker needs (a job on a tier, a machine on the node network, a GitHub account), what they gain, and the release you saw it on (`billet version`).

## What is not a vulnerability

A job can affect the host it runs on wherever its backend is not a real boundary, or its tier lacks a separate network for untrusted work. That holds on the Docker backend, and on any backend without the network [Trust and isolation](docs/concepts/trust-and-isolation.md) describes. It is documented behaviour. A way out of a boundary billet does claim (a Firecracker or tart guest, an EC2 instance, a CodeBuild build, the node wire's mutual TLS) is a vulnerability.

## Which releases get fixes

billet is pre-alpha. A fix ships in the next release, and a deployment takes up each release by itself unless `release.automatic` is set to `false` ([Upgrades](docs/operating/upgrades.md)). Releases before the newest are not patched.

## What billet holds, and how it is protected

[The security overview](docs/concepts/security-overview.md) is the one page for deciding whether to run billet. It covers the threat model, the GitHub App's exact permissions, what runs as root, how releases and the node wire are authenticated, and where each piece of data lives and for how long.
