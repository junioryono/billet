terraform {
  # Matches the consumer floor the rest of this repository's modules declare.
  required_version = ">= 1.9"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = ">= 6.0"
    }
  }
}
