# Action versioning

Billet publishes four composite actions — `build-push-action`, `setup-docker-builder`, `stickydisk` and `stop-docker-builder` — and there are three ways to reference one. They are not ranked, because the thing that makes one better is exactly what makes it worse.

## The three references

**`@v0` moves.** It is a tag that is force-pushed to each accepted release, so a workflow written once keeps getting fixes without anybody editing it. This is what it means for an ordinary compatible release not to require edits to workflow files. It is the only Billet reference that ever changes under you.

**`@v0.4.1` never moves.** GitHub release immutability is enabled on this repository, so a published tag and its assets are frozen and carry GitHub's own release attestation. A semver tag names one release forever.

**A commit SHA never moves either**, and is the only form that does not depend on the tag itself being trustworthy. A tag is a pointer; a SHA is the content.

## What you are actually trading

The honest version is short. A moving major means Billet can fix a bug in an action you are running without asking you, and it means Billet can break your build without asking you, and those are the same sentence. A commit SHA means neither happens.

There is no configuration that gets both. What narrows the gap is that `@v0` only ever advances to a release that has already been proved: the `advance-major` job in `cut-release.yml` runs after `release.yml`, which proves the release immutable and verifies GitHub's attestation for the tag before it finishes. A tag whose build failed is never pointed at. That bounds *which* releases the major can reach; it does not make a release you have not read into one you have.

If your threat model includes this repository being compromised, use a SHA. If it includes you forgetting to update a pin for two years, use `@v0`. Most people running self-hosted CI have the second problem.

## Nested actions resolve to one release

`build-push-action` composes `setup-docker-builder`, which composes `stickydisk` and `stop-docker-builder`. A composite action's `uses:` is static YAML — GitHub gives it no way to compute a reference at runtime — so each of those is a literal, and a literal is exactly the kind of thing that goes stale without anybody noticing.

It did. Between v0.3.26 and this change, `build-push-action` on `main` composed `setup-docker-builder@v0.3.26`, so anybody following `@main` ran the outer action from `main` and the inner ones from a release several versions behind. Nobody had done anything wrong and nothing said so.

The rule now is that a bundled action resolves to the same Billet as the action that called it, and it is enforced in two places because the tree has two lives:

- **In the repository**, siblings are `@main`, so `main` composes `main`. `TestBundledActionsComposeMainInTheTree` runs in `make check`.
- **In a release**, `cut-release.yml` rewrites every internal reference to the tag being cut, before it creates that tag, and `check-release-metadata.sh` refuses to tag if any of them did not land.

`TestEveryFileWithASiblingRefIsRewrittenAtReleaseTime` covers the gap between the two: a new composite action that composes a sibling and is not listed in the rewrite step would ship inside an immutable tag still pointing at `main`, which quietly removes the property the pin was chosen for.

## What does not follow a channel

Billet's binary follows a signed release channel and can update itself. The actions cannot: a `uses:` reference is resolved by GitHub before any Billet code runs, so the only mechanism available is which ref you wrote. `@v0` is that mechanism.
