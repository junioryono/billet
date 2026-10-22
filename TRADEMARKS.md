# Trademarks

billet's code is yours to use under its licenses; its name is not. This file says what you may do with the name without asking, and what needs permission first. It grants nothing the licenses withhold, and it is not legal advice.

## What it covers

The mark is the word **billet**, in any capitalisation, as the name of this software and of anything offered around it. It belongs to Junior Yono. It is not registered: where a symbol is used it is billet™, and never ®, which is reserved for a registered mark.

Neither license passes it on. The Apache License 2.0 says so itself (section 6), and [the billet Enterprise License](ee/LICENSE) grants no right to the name either. The policy also covers names close enough to be mistaken for it: obvious variations, misspellings and abbreviations, and "billet" combined with other words.

## The one rule

Anyone who meets the name where you used it must be able to tell what they are getting and who made it. Every case below follows from that. When one is not listed, apply the rule, and ask if you are unsure.

## What you may do without asking

- **Run it.** Use billet, unmodified or modified, for yourself or your organisation, and say so: "our CI runs on billet".
- **Redistribute an unmodified release under its name.** The binaries, packages and images billet publishes, or the source at a tagged commit, with `LICENSE` and `NOTICE` kept.
- **Package it for a distribution.** A package may keep the name when its only changes are the ones packaging needs (install paths, build flags, backported security fixes), provided it says it is a packaged build and where to report packaging bugs.
- **Keep the technical identifiers in a fork.** The `billet` command, `billet.yaml` and its keys, the `BILLET_` environment variables, the systemd unit and launchd job names, the runner labels and names billet generates, the Go module path, the Ansible collection name and the Actions' paths are interfaces other software depends on, not branding. A fork may keep them so that it stays compatible; what has to change is the name the fork presents itself by.
- **Describe a relationship truthfully.** Put the name after a word that says how your work relates to billet: "compatible with billet", "a plugin for billet", "forked from billet", "based on billet v0.4", "a hosted service that runs billet". The name before your own ("billet Dashboard by Acme") reads as billet's product, so do not use that form.
- **Write about it.** Articles, talks, books, courses, comparisons and benchmarks may use the name, as long as they do not suggest that billet published, reviewed or endorsed them.

## What needs written permission

- **Using the name for your own product.** A fork, a modified distribution or another product may not be called billet, or by a name containing it ("billet Pro", "OpenBillet", "billet-ng").
- **A hosted or managed service under the name.** "billet Cloud", "Managed billet", or any name that suggests the official service. A service may say it runs billet, as above.
- **The name in your own identity.** A company, domain, registry package, organisation, account or app name built on the name, beyond the packaging and the technical identifiers above.
- **Merchandise for sale.**
- **Any claim of endorsement, partnership or certification.**

## Forking

Give the fork its own name, and use it in the repository's description, its documentation and its releases. Keep `LICENSE` and `NOTICE` as the Apache License requires, and say where it came from: "forked from billet", with a link. The technical identifiers may stay.

## Asking for permission

Open an issue at https://github.com/junioryono/billet/issues titled "Trademark: " followed by what you want to do. Permission is given in writing, case by case, and may be withdrawn if the use stops following the one rule.

## Changes

This policy may change. The version on the `main` branch is the current one.

It was written after reading the [Model Trademark Guidelines](https://modeltrademarkguidelines.org) and the [Commonhaus Foundation's trademark policy](https://github.com/commonhaus/foundation/blob/main/policies/trademark-policy.md), and none of their text is copied. [Upstream references](docs/reference/upstream-references.md) records what it took from them.
