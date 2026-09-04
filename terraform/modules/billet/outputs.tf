# THE NON-SECRET FACTS the configuration-management layer needs to render
# billet.yaml (ADR-004: this module never writes billet.yaml, certificates or
# units). Hand these to the junioryono.billet.host Ansible role's billet_config.
#
# EVERY PRE-SPLIT OUTPUT SURVIVES with its name and meaning — the root's
# contract is what existing deployments and the Ansible examples consume, so
# the split behind it must be invisible here.

output "region" {
  description = "The region billet runs in (node.ec2.region)."
  value       = local.region
}

output "availability_zone" {
  description = "The subnet's availability zone (node.ebs_s3.availability_zone — an EBS cache volume must be in the same zone as its instance). Read THROUGH the child, so asserting it proves the resolved zone crossed the module call rather than only that the root resolved it — the ledger volume is created in this zone."
  value       = module.control_plane.availability_zone
}

output "vpc_id" {
  description = "The VPC billet is in (adopted or created)."
  value       = local.vpc_id
}

output "subnet_id" {
  description = "The subnet the control plane and runners launch in (node.ec2.subnet_id)."
  value       = local.subnet_id
}

output "runner_security_group_id" {
  description = "The trusted-runner security group (node.ec2.security_group_ids)."
  value       = module.fleet.runner_security_group_id
}

output "control_plane_security_group_id" {
  description = "The control plane's security group."
  value       = module.control_plane.security_group_id
}

output "control_plane_instance_id" {
  description = "The control-plane EC2 instance id."
  value       = module.control_plane.instance_id
}

output "control_plane_private_ip" {
  description = "The control plane's private IP: the one control_plane_private_ip declared, otherwise the one AWS observed at launch."
  value       = module.control_plane.private_ip
}

output "node_wire_address" {
  description = "The host:port other hosts dial for the node wire — bind it as server.listen, and use it as node.server_addr on a remote node. billet requires host:port, not a bare IP. This is the PRIVATE address and the right default; a node outside the VPC reaches it over a private network, a VPN or overlay, a reverse tunnel, or the public address below (ADR-001). Plan-known once control_plane_private_ip is declared."
  value       = module.control_plane.node_wire_address
}

output "public_node_wire_address" {
  description = "The control plane's public host:port, or empty when the instance has no public IP. Exposing the node wire here is supported: it requires a client certificate in the handshake and serves no route without one, so anonymous traffic cannot take the connections enrolled nodes hold. The budget is charged only after a certificate verifies, so anonymous traffic cannot displace a connected node; a handshake slot stays best effort, so front a public port with a rate limiter. Widening node_ingress_cidrs to reach it is your decision; set server.node_tls_hosts to the name nodes will dial, since that list becomes the certificate's subject names."
  value       = module.control_plane.public_node_wire_address
}

output "bootstrap_wire_address" {
  description = "The control plane's PRIVATE host:port for enrollment — bind it as server.bootstrap_listen, and pass it to `billet node --enroll --bootstrap-addr` from inside the VPC. It carries only /v1/ca and /v1/enroll, has a connection budget of its own so saturating it cannot reach the fleet, and is unreachable until bootstrap_ingress_cidrs names something. Open it while adding a machine and close it afterwards."
  value       = module.control_plane.bootstrap_wire_address
}

output "public_bootstrap_wire_address" {
  description = "The control plane's PUBLIC host:port for enrollment, or empty when the instance has no public IP — what a node OUTSIDE the VPC passes to `billet node --enroll --bootstrap-addr`. This is the address the spare-room posture needs, since such a node cannot dial the private one. It still needs bootstrap_ingress_cidrs to reach it, and the name dialled must be in server.node_tls_hosts."
  value       = module.control_plane.public_bootstrap_wire_address
}

output "node_role_arn" {
  description = "The IAM role the control plane assumes to run billet (its instance profile)."
  value       = module.fleet.node_role_arn
}

output "control_plane_instance_profile" {
  description = "The instance profile attached to the control-plane EC2 — how billet obtains its AWS credentials from IMDS to launch and reap compute. It is the instance's OWN identity, set by this module on the instance; it is NOT node.ec2.instance_profile, which is the (separate, optional) role trusted JOB instances receive."
  value       = module.control_plane.instance_profile_name
}

output "cache_bucket" {
  description = "The S3 cache bucket name (node.ebs_s3.bucket), or empty when the cache is disabled."
  value       = module.fleet.cache_bucket
}

output "cache_prefix" {
  description = "The cache object prefix (node.ebs_s3.prefix)."
  value       = module.fleet.cache_prefix
}

output "cache_kms_key_arn" {
  description = "The customer-managed KMS key ARN for the cache's EBS volumes (node.ebs_s3.kms_key_id), or empty."
  value       = module.fleet.cache_kms_key_arn
}

output "interruption_queue_url" {
  description = "The spot interruption queue URL (node.ec2.interruption_queue_url), or empty when spot is disabled."
  value       = module.fleet.interruption_queue_url
}

output "spot_node_name" {
  description = "The name a SPOT node must use as its node.name: billet requires the interruption queue's basename to equal the effective node name, so a spot node's node.name must be this. Empty when spot is disabled."
  value       = module.fleet.spot_node_name
}

output "ledger_volume_id" {
  description = "The dedicated ledger EBS volume id. The config layer mounts it by stable identity (its NVMe serial carries this id) at the billet state directory so the SQLite ledger survives the instance — set the host role's billet_ledger_volume_id to it."
  value       = module.control_plane.ledger_volume_id
}

output "ledger_device_name" {
  description = "The requested attachment device (/dev/sdf); on a Nitro instance the kernel renames it (e.g. /dev/nvme1n1), so mount by ledger_volume_id / filesystem UUID, not this name."
  value       = module.control_plane.ledger_device_name
}

output "backup_bucket" {
  description = "The bucket billet copies its deployment archives to (backup.s3.bucket) — created, adopted, or empty when there is no off-site copy."
  value       = module.control_plane.backup_bucket
}

output "backup_prefix" {
  description = "The object prefix archives land under (backup.s3.prefix). The grant is scoped to it literally."
  value       = module.control_plane.backup_prefix
}

output "backup_role_name" {
  description = "The IAM role the backup grant is attached to, or empty. In this root it is fleet-ec2's node role, because that is the identity the co-located controller runs with; a grant anywhere else would leave the bucket unwritable."
  value       = module.control_plane.backup_role_name
}

output "vpc_cidr" {
  description = "The VPC's resolved CIDR (created or adopted) — what the node-wire ingress defaults to. Additive post-split, and load-bearing for the tests: it reads THROUGH the child, so asserting it proves the resolved adopted CIDR actually crossed the module call rather than only that the root resolved it."
  value       = module.control_plane.vpc_cidr
}

# WHAT A CHANGE COSTS A RUNNING DEPLOYMENT, and what it will be billed for.
#
# Both are rendered so they reach `terraform output` and a plan's JSON, where an
# operator's own tooling can read them without knowing anything about billet.

output "operation_classes" {
  description = "What a change to each managed resource costs a RUNNING deployment: in_place, replacement, draining (a host that may be running jobs stops) or destructive (data does not come back). Terraform's own plan says create/update/replace/destroy, which cannot distinguish a tag from the instance holding the ledger — ADR-004 keeps live billet nodes outside Terraform, so a plan does not know these hosts are running somebody's build. `go run ./scripts/tfclassify -plan <plan.json>` joins this to a plan and fails one that needs a drain first."
  value       = local.operation_classes
}

output "cost_inputs" {
  description = "The CONSERVATIVE cost inputs this module decides: what it creates that is always on, and what it does not bound. Deliberately NOT a dollar figure — billet ships no price table on purpose (a stale one understates exposure), and the priced ceiling comes from `billet check` and `billet status`, which read each node's declared shapes. `per_job_compute_bounded_by` names what actually caps the fleet."
  value = {
    always_on = {
      control_plane_instances     = 1
      control_plane_instance_type = var.control_plane_instance_type
      control_plane_architecture  = var.control_plane_architecture
      # Two volumes, and they are not interchangeable: the root is disposable and
      # the ledger is retained, which is why recovery keeps the instance.
      root_volume_gib   = 16
      ledger_volume_gib = var.control_plane_volume_gib
      ebs_volume_type   = "gp3"
    }
    created_when_enabled = {
      cache_bucket         = var.enable_cache
      cache_kms_key        = var.enable_kms
      spot_interruption    = var.enable_spot
      backup_bucket        = var.create_backup_bucket
      nat_gateway          = false
      internet_gateway     = local.create_vpc
      cloudwatch_log_group = false
    }
    # SAID OUT LOUD RATHER THAN LEFT TO INFERENCE. Everything above is a handful
    # of dollars a month; the fleet is not, and this module does not cap it. What
    # does is billet's own configuration, and only billet can price it, because
    # only billet knows which shapes a node is allowed to buy.
    per_job_compute_bounded_by = [
      "server.max_vcpu and server.max_memory (the deployment ceiling)",
      "node.max_vcpu and node.max_memory (what one orchestrator may buy)",
      "node.ec2.instance_types / node.codebuild.compute_types (the shapes, each with its declared price)",
    ]
    priced_by = "billet check (one configured node) and billet status (the registered fleet)"
    # An NAT gateway is the cost operators most often meet unexpectedly, and this
    # module never creates one — a created VPC routes through an internet gateway,
    # which is free.
    notes = local.create_vpc ? "This deployment's created VPC routes through an internet gateway, not a NAT gateway; instances need a public IP to reach GitHub." : "This deployment adopted your network, so its egress path — and any NAT gateway charge — is yours."
  }
}

output "subnet_cidr" {
  description = "The subnet's resolved CIDR (created or adopted), read THROUGH the child so asserting it proves the range a declared control_plane_private_ip is checked against actually crossed the module call."
  value       = module.control_plane.subnet_cidr
}
