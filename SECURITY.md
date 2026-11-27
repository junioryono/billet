# Security policy

## Reporting a vulnerability

Report it privately through GitHub's private vulnerability reporting: the repository's **Security** tab, then **Report a vulnerability**. Do not open a public issue or pull request for it.

Say what an attacker needs (a job on a tier, a machine on the node network, a GitHub account), what they gain, and the release you saw it on (`billet version`).

## What is not a vulnerability

A job can affect the host it runs on wherever its backend is not a real boundary, or its tier lacks a separate network for untrusted work. That holds on the Docker backend, on a CodeBuild reserved-capacity fleet or non-container environment, and on any backend without the network [Trust and isolation](docs/concepts/trust-and-isolation.md) describes. It is documented behaviour, and so is what a cache shares between jobs of one scope.

These boundaries billet does claim, and a way out of one is a vulnerability:

- a Firecracker or tart guest;
- an EC2 instance;
- an on-demand CodeBuild container build;
- the node wire's mutual TLS;
- a cache's publication policy.

## Which releases get fixes

billet is pre-alpha. A fix ships in the next release, and releases before the newest are not patched. A deployment takes up each release by itself, within its maintenance window, unless `release.automatic` is `false` or `release.version` pins an exact release ([Upgrades](docs/operating/upgrades.md)). A pinned or manual deployment takes up a fix only when an operator moves it.

## What billet holds, and how it is protected

[The security overview](docs/concepts/security-overview.md) is the one page for deciding whether to run billet. It covers:

- the threat model;
- the GitHub App's exact permissions, and what they reach;
- what runs as root;
- how releases, guest images and the node wire are authenticated, and the waivers an operator can give;
- where each piece of data lives, who can read it, and for how long.
