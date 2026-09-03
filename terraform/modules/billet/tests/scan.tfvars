# Feature-gated resources are invisible to a static config scan at defaults:
# trivy expands count, and spot/KMS default off. This tfvars turns every
# feature on so the scan sees the whole surface, not the default subset.
enable_spot = true
enable_kms  = true
