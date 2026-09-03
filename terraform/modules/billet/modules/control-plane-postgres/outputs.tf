output "instance_id" {
  description = "The control-plane EC2 instance id."
  value       = aws_instance.control_plane.id
}

output "private_ip" {
  description = "The control plane's private IP."
  value       = aws_instance.control_plane.private_ip
}

output "node_wire_address" {
  description = "The host:port other hosts dial for the node wire — bind it as server.listen, and use it as node.server_addr on a remote node. billet requires host:port, not a bare IP."
  value       = "${aws_instance.control_plane.private_ip}:${var.listen_port}"
}

output "public_ip" {
  description = "The control plane's public IP, or empty when it has none."
  value       = aws_instance.control_plane.public_ip
}

output "public_node_wire_address" {
  description = "The control plane's public host:port, or empty when the instance has no public IP. Exposing the node wire here is supported: it requires a client certificate in the handshake and serves no route without one, and the connection budget is charged only after a certificate verifies, so anonymous traffic cannot displace a connected node. A handshake slot stays best effort, so put a rate limiter in front as you would for any internet-facing TLS listener. Set server.node_tls_hosts to whatever name nodes will dial."
  value       = aws_instance.control_plane.public_ip != "" ? "${aws_instance.control_plane.public_ip}:${var.listen_port}" : ""
}

output "bootstrap_wire_address" {
  description = "The control plane's PRIVATE host:port for enrollment — bind it as server.bootstrap_listen, and pass it to `billet node --enroll --bootstrap-addr` from inside the VPC. Reachable only from bootstrap_ingress_cidrs, which is empty by default."
  value       = "${aws_instance.control_plane.private_ip}:${var.bootstrap_port}"
}

output "public_bootstrap_wire_address" {
  description = "The control plane's PUBLIC host:port for enrollment, or empty when the instance has no public IP. This is what a node outside the VPC passes to `billet node --enroll --bootstrap-addr`. Whatever name is dialled must also be in server.node_tls_hosts, since both listeners present that certificate."
  value       = aws_instance.control_plane.public_ip != "" ? "${aws_instance.control_plane.public_ip}:${var.bootstrap_port}" : ""
}

# THE GROUP THE LEDGER GRANTS INGRESS TO. state-rds-postgres takes this as
# client_security_group_ids, so the database's permission is expressed in terms
# of WHO rather than of an address — which matters more here than on any other
# profile, because the whole promise of this one is that the controller can be
# replaced, and a replaced instance does not keep its address.
output "security_group_id" {
  description = "The control plane's security group. Pass it to state-rds-postgres as client_security_group_ids so the ledger admits this controller by identity rather than by CIDR."
  value       = aws_security_group.control_plane.id
}

output "instance_profile_name" {
  description = "The instance profile the controller runs with: the one the caller supplied, or this child's own minimal profile."
  value       = local.profile_name
}

output "role_name" {
  description = "This child's own IAM role, or empty when the caller supplied a profile. Attach anything else the controller needs to it — the backup grant and the state-secret grant already are."
  value       = var.create_instance_profile ? aws_iam_role.this[0].name : ""
}

output "backup_bucket" {
  description = "The bucket billet copies its identity archive to — the one this child created, the one it was told to adopt, or empty when there is no off-site copy. Set backup.s3.bucket in billet.yaml to it. On this profile that archive is the ONLY copy of the deployment identity and the node-wire CA, because there is no retained volume."
  value       = local.backup_bucket
}

output "backup_prefix" {
  description = "The object prefix archives land under. Set backup.s3.prefix in billet.yaml to it, or the controller writes somewhere its IAM grant does not cover."
  value       = local.backup_prefix
}

output "vpc_cidr" {
  description = "The vpc_cidr this child received — re-exported so a composing root's tests can prove the resolved CIDR crossed the module call; this child's own suite binds it to the node-wire ingress."
  value       = var.vpc_cidr
}
