# ADR-023: Installation tokens from a broker

**Status:** Accepted (2026-11-03).

## Context

Every control plane held its targets' GitHub App private keys and minted installation tokens with them. That is right for one deployment. An operator running many deployments under one App would have to put the same key on every control plane, and a compromise of any one of them could then mint tokens for every installation of that App: every other deployment's organizations and repositories.

## Decision

**A control plane's tokens come from a `github.TokenSource`, and there are two.** The App key is today's behaviour and stays the default. A broker, reached over mutual TLS, holds the key instead, and a deployment configured for one (`server.token_broker`) holds none: a target may not name a key beside it, `billet github-app create` and `store-key` refuse it, and a backup carries none (archive schema 5, whose manifest says so, so a truncated archive never reads as a deliberate one).

**The broker is `billet broker serve`, in the public binary, with a file of its own.** The process holding the keys holds nothing else, and a billet.yaml names no key of its. It has one route, and no route could hand out a key.

**A caller is who its key says, and nothing it writes.** The listener requires a client certificate and verifies no chain: each deployment registers the fingerprint of a key its host made with `billet broker client-key`, the way enrollment compares two fingerprints, and the handshake proves the caller holds it. A dedicated key rather than the node-wire CA, for two reasons. The CA signs every node, so a compromised node could have authenticated as its deployment, and telling the two apart would have rested on a name in a certificate. And a broker pinning a CA has to follow that CA's rotations. A key per host also needs no store: an active-passive pair registers two, and a restored host makes and registers its own.

**The broker mints only what a deployment's registration names**, scoped to what that deployment needs: exactly billet's permissions for the target, and for a repository target that repository alone. It refuses anything else in the same words whether it is another deployment's or no one's, so an answer says nothing about who else it serves, and it never asks GitHub for a caller it refused. An installation registered as a whole organization may belong to one deployment only, because its token reaches every repository the installation does; two deployments may share an installation by repository.

**The broker proves an installation before minting**, at most hourly: a registry names ids an operator typed, and a wrong one would mint another owner's token for this caller. It uses `github.VerifyAppAt`, the check `billet check` runs on a key it holds.

**Could-not-tell is not no.** A broker that cannot answer, or GitHub not answering the broker, is `github.ErrTokenUnavailable`, which `github.Undecided` reads as could-not-tell. Only GitHub's own word says the App is not installed. A held token rides out silence until its last usable minute; a definite refusal is reported at once.

**The scale-set client takes a fixed token, and an installation token lasts an hour.** The vendored client either signs its own JWTs with a key it holds or carries one static token for life, and its transport must stay an `*http.Transport` (actions/scaleset v0.4.0, `common_client.go`), so nothing can be wrapped around it. Its one reader of that token is the registration-token request in its hourly refresh. So a client on a source is built with a placeholder, and retryablehttp's `RequestLogHook`, called with the request before every attempt, replaces it with what the source holds at that moment. A poll carries the Actions service's own token and never asks, so a broker outage costs a deployment nothing until a refresh is due. A hook cannot return an error, and a request sent with no credential comes back as a 401 that reads as a verdict about the App, so a source that cannot answer stops the request with a scheme the transport refuses, which retryablehttp does not retry, and the client names why.

## What it does not do

- It does not rotate an App's key, or manage its keys at all: an operator installs them, under the rules a key file always had.
- It does not authenticate a deployment beyond the registered key. The registry is the trust decision, and the broker's operator makes it.
- A deployment's own App key path stays exactly what it was, including the vendored client signing its own JWTs on it.

## Alternatives considered

- **Client certificates from each deployment's node-wire CA.** Above.
- **A Go package and no command.** A self-hoster with several deployments wants this too, and a package with no process to run is a design rather than a feature.
- **Rebuilding the vendored client on each new token.** A message session holds its client, so old sessions would keep a client whose token had expired.
- **Fetching a token before every vendored call.** A poll would then fail while a broker was down, though it needs nothing from one.
