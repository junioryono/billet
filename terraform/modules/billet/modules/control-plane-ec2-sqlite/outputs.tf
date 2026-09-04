output "instance_id" {
  description = "The control-plane EC2 instance id."
  value       = aws_instance.control_plane.id
}

output "private_ip" {
  description = "The control plane's private IP: the one private_ip declared, otherwise the one AWS observed at launch."
  value       = aws_instance.control_plane.private_ip
}

output "node_wire_address" {
  description = "The host:port other hosts dial for the node wire — bind it as server.listen, and use it as node.server_addr on a remote node. billet requires host:port, not a bare IP. Plan-known once private_ip is declared, so a root can assert it and a consumer can write it into configuration before the apply."
  value       = "${aws_instance.control_plane.private_ip}:${var.listen_port}"
}

# THE PUBLIC ADDRESS, when the instance has one. Empty otherwise — a subnet
# without `map_public_ip_on_launch`, or one you supplied.
#
# EXPOSING THE NODE WIRE ON IT IS SUPPORTED: that listener requires a
# client certificate in the handshake and serves no route without one, so a caller
# with nothing to present is refused before it can occupy a connection slot. The
# two routes that must serve an unenrolled machine are on the enrollment port
# instead, which `bootstrap_ingress_cidrs` leaves closed.
#
# Widening `node_ingress_cidrs` to 0.0.0.0/0 is therefore a decision you may make
# rather than one this module refuses. billet charges the node wire's connection
# budget only AFTER a client certificate verifies, so anonymous traffic cannot
# displace a connected node; what stays best effort is a handshake slot, which no
# server can reserve for a caller it has not identified. Put a rate limiter in
# front, as you would for any internet-facing TLS listener.
output "public_ip" {
  description = "The control plane's public IP, or empty when it has none."
  value       = aws_instance.control_plane.public_ip
}

output "public_node_wire_address" {
  description = "The control plane's public host:port, or empty when the instance has no public IP. Exposing the node wire here is supported: it requires a client certificate in the handshake and serves no route without one, so anonymous traffic cannot consume the connections enrolled nodes hold. The budget is charged only after a certificate verifies, so anonymous traffic cannot displace a connected node; a handshake slot stays best effort, so put a rate limiter in front as you would for any internet-facing TLS listener. Set server.node_tls_hosts to whatever name nodes will dial, since the certificate is minted for that list."
  value       = aws_instance.control_plane.public_ip != "" ? "${aws_instance.control_plane.public_ip}:${var.listen_port}" : ""
}

# THE ENROLLMENT ADDRESS, IN BOTH FORMS. Reported whether or not anything may
# reach it: what opens it is `bootstrap_ingress_cidrs`, empty by default, and
# what serves it is `server.bootstrap_listen` in billet.yaml, also unset by
# default. Both have to be set deliberately, and both should be closed again once
# the machine has been approved.
#
# THE PUBLIC ONE EXISTS BECAUSE THE PRIVATE ONE CANNOT SERVE THE CASE THIS IS
# FOR. A machine in a spare room with no static address is exactly the node that
# has to enroll from outside the VPC, and handing it the controller's private IP
# is an address it cannot dial.
output "bootstrap_wire_address" {
  description = "The control plane's PRIVATE host:port for enrollment — bind it as server.bootstrap_listen, and pass it to `billet node --enroll --bootstrap-addr` from inside the VPC. Reachable only from bootstrap_ingress_cidrs, which is empty by default. A node enrolling from outside the VPC needs public_bootstrap_wire_address."
  value       = "${aws_instance.control_plane.private_ip}:${var.bootstrap_port}"
}

output "public_bootstrap_wire_address" {
  description = "The control plane's PUBLIC host:port for enrollment, or empty when the instance has no public IP. This is what a node outside the VPC passes to `billet node --enroll --bootstrap-addr`, and it needs bootstrap_ingress_cidrs to name that node's address — which is the one thing a spare-room machine cannot promise, so open it to a wider range only while you are enrolling and close it afterwards. Whatever name is dialled must also be in server.node_tls_hosts, since both listeners present that certificate."
  value       = aws_instance.control_plane.public_ip != "" ? "${aws_instance.control_plane.public_ip}:${var.bootstrap_port}" : ""
}

output "security_group_id" {
  description = "The control plane's security group."
  value       = aws_security_group.control_plane.id
}

output "instance_profile_name" {
  description = "The instance profile the controller runs with: the one the caller supplied, or this child's own minimal profile."
  value       = local.profile_name
}

output "ledger_volume_id" {
  description = "The dedicated ledger EBS volume id. The config layer mounts it by stable identity (its NVMe serial carries this id) so the SQLite ledger survives the instance — set the host role's billet_ledger_volume_id to it."
  value       = aws_ebs_volume.ledger.id
}

output "ledger_device_name" {
  description = "The requested attachment device (/dev/sdf); on a Nitro instance the kernel renames it, so mount by ledger_volume_id, not this name."
  value       = aws_volume_attachment.ledger.device_name
}

output "vpc_cidr" {
  description = "The vpc_cidr this child received — re-exported so a composing root's tests can prove the resolved CIDR crossed the module call; this child's own suite binds it to the node-wire ingress."
  value       = var.vpc_cidr
}

output "backup_bucket" {
  description = "The bucket billet copies its deployment archives to — the one this child created, the one it was told to adopt, or empty when there is no off-site copy. Set backup.s3.bucket in billet.yaml to it."
  value       = local.backup_bucket
}

output "backup_prefix" {
  description = "The object prefix archives land under. Set backup.s3.prefix in billet.yaml to it, or the controller writes somewhere its IAM grant does not cover."
  value       = local.backup_prefix
}

# FROM THE GRANT, NOT FROM THE LOCAL THAT FEEDS IT: reporting the local would
# name a role whether or not a policy was attached to it, and the composing
# root's test would then pass with the grant's count regressed to the own
# role -- the exact defect it exists to catch.
output "backup_role_name" {
  description = "The IAM role the backup grant is attached to — this child's own, or the instance_profile_role_name it was given — or empty when there is no off-site copy. Plan-known, so a composing root's tests can prove the grant landed on the identity the controller runs with."
  value       = length(aws_iam_role_policy.backups) > 0 ? aws_iam_role_policy.backups[0].role : ""
}

output "availability_zone" {
  description = "The availability_zone this child received — re-exported so a composing root's tests can prove the resolved zone crossed the module call; the ledger volume is created in it."
  value       = var.availability_zone
}

output "subnet_cidr" {
  description = "The subnet_cidr this child received, or empty — re-exported so a composing root's tests can prove the resolved CIDR crossed the module call; it is what a declared private_ip is checked against."
  value       = var.subnet_cidr
}
