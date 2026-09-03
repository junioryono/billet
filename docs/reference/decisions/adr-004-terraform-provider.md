# ADR-004 — Do not build a Terraform provider before billet has a configuration API

Status: accepted.

## Decision

billet will not ship a Terraform provider in pre-alpha. `billet.yaml` remains the one declarative authority for tiers, node policies, sites and deployment capacity, and a server restart remains the point at which those objects change.

Revisit this decision only after billet has a versioned, authenticated configuration API with transactional replacement of the whole catalogue, stable object identities, an explicit ownership rule between file and API configuration, and at least one released version whose users have asked for Terraform integration. Build the provider with HashiCorp's Plugin Framework at that point, as a separately versioned binary with acceptance tests against a real billet server.

## Why resources are wrong today

A Terraform resource is a lifecycle manager: it must read current state and create, update or delete the remote object during plan and apply. billet has no API for any of those operations. Tiers and node policies are read from a file at process construction; sites are part of that same catalogue; changing any of them requires replacing the file and restarting the server. A provider would therefore need to edit an operator-owned YAML file remotely, invent a second mutable store, or add the missing configuration API as an incidental detail. The first loses comments and ordering and races configuration management, the second creates two authoritative writers, and the third is the actual product feature disguised as an integration.

The apparently clean resources are also not independent. A tier refers to sites and node policy, provider order changes placement and spend, and reserved floors are validated across the entire catalogue and deployment ceiling. Applying `billet_tier` objects one at a time creates intermediate catalogues that billet correctly refuses. The natural API operation is compare-and-swap replacement of one validated catalogue, not CRUD on four resource types.

`billet_node` is not a resource at all. A node registers itself and its certificate identity proves what machine is speaking; Terraform neither creates that machine nor owns its runtime lifetime. Modeling a registered node as managed would make deletion ambiguous between draining, revoking identity, removing server-side policy and destroying compute.

Enrollment must remain outside apply. Its approval is a human comparison of two independently observed fingerprints. Automating approval through a provider would remove the property the ceremony exists to establish. A future provider may expose pending enrollments as a read-only data source, because Terraform data sources are explicitly side-effect free, but it must not approve them.

## Prior art and maintenance cost

[HashiCorp describes providers](https://developer.hashicorp.com/terraform/plugin/how-terraform-works) as separately distributed plugins that translate Terraform's resource lifecycle into a service API, while [data sources](https://developer.hashicorp.com/terraform/plugin/framework/data-sources) read external state without managing it. That distinction supports the decision here: billet currently has useful read-only facts, but no remote configuration lifecycle to translate. The [Plugin Framework](https://developer.hashicorp.com/terraform/plugin/framework) is the recommended implementation for a future provider, and it brings a separate release cadence, schemas, state migrations, generated documentation and acceptance testing. That maintenance commitment is unjustified before billet has its first release or evidence of an audience for the integration.

## What to do meanwhile

Manage `billet.yaml`, certificates and service units with the configuration-management tool that provisions the host, then run `billet check` before restarting. Terraform can still provision the EC2 control plane, networking, IAM, EBS volume and DNS; hand the rendered billet configuration to cloud-init or the host-management layer as one file. Read-only automation should call stable operator commands such as `billet status` and `billet nodes pending` rather than pretending their output is a managed lifecycle.

## Revisit checklist

- A released billet version and user demand for Terraform integration.
- A versioned authenticated API that reads and atomically replaces the whole validated catalogue.
- One documented authority: API-managed or file-managed configuration, never both for the same deployment.
- Stable IDs, optimistic concurrency, import behavior and deletion semantics for every proposed resource.
- Enrollment remains read-only; identity approval is not automated.
- Plugin Framework implementation, separate semantic versioning, state migration tests and live acceptance tests.
