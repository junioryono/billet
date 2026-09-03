terraform {
  # Matches the root's consumer floor; this child uses nothing newer.
  required_version = ">= 1.9"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = ">= 6.0"
    }
    # Zips the spot-interruption router Lambda from the committed source, so the
    # module needs no external build step and no prebuilt artifact.
    archive = {
      source  = "hashicorp/archive"
      version = ">= 2.0"
    }
  }
}
