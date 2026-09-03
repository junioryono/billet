terraform {
  required_version = ">= 1.9"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = ">= 6.0"
    }
  }
}

# The co-located deployment: one controller that also runs billet with the node
# role, in a module-created network. This example is what the root exists for;
# an operator with an existing network passes vpc_id/subnet_id instead.

provider "aws" {
  region = "us-west-2" # the module takes its region from the provider
}

module "billet" {
  source = "../.."

  name              = "billet"
  enable_cache      = true
  enable_spot       = false
  ssh_ingress_cidrs = ["203.0.113.0/24"] # your admin range, for the Ansible role
}

# Hand these to the junioryono.billet.host role's billet_config.
output "node_wire_address" {
  value = module.billet.node_wire_address
}

output "ledger_volume_id" {
  value = module.billet.ledger_volume_id
}

output "cache_bucket" {
  value = module.billet.cache_bucket
}
