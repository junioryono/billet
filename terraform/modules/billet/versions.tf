terraform {
  # The CONSUMER floor: a caller's terraform enforces a child module's
  # required_version, so raising this rejects deployments that would work.
  # Measured on 1.9.8 with the tests stripped, because a consumer's init never
  # parses a child's tests/. DEVELOPING this repo needs 1.11.4: the test
  # suite's override_during fails init on 1.10.5, and CI pins that floor.
  required_version = ">= 1.9"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = ">= 6.0"
    }
  }
}
