# terraform

The AWS infrastructure modules. Load `billet-infra-terraform-ansible` before changing anything here, and `billet-providers-aws` for the IAM and the CodeBuild facts the modules encode.

- Terraform creates cloud resources and returns narrow outputs; it never manages live jobs, leases, identity or drains.
- Modules pin to release tags, and every layer names the same version.
- The module's IAM stays equal to what `billet init iam` generates (`internal/tfpolicy`).
- `terraform destroy` refuses under a running CodeBuild build, and it is the module that refuses.

Gates: `scripts/gates/terraform.sh fmt validate test lint scan` before pushing (`scripts/gates/tools.sh` installs the pins), and `scripts/gates/terraform.sh classify` to prove the committed `classification.json` describes every module resource. What a real plan costs a running deployment is `go run ./scripts/tfclassify -plan plan.json`, on the JSON of `terraform show -json <planfile>`; passing `scripts/gates/terraform.sh classify` does not evaluate any plan.
