# terraform

The AWS infrastructure modules. Load `billet-infra-terraform-ansible` before changing anything here, and `billet-providers-aws` for the IAM and the CodeBuild facts the modules encode.

- Terraform creates cloud resources and returns narrow outputs; it never manages live jobs, leases, identity or drains.
- Modules pin to release tags, and every layer names the same version.
- The module's IAM stays equal to what `billet init iam` generates (`internal/tfpolicy`).
- `terraform destroy` refuses under a running CodeBuild build, and it is the module that refuses.

Gates: `make tf-fmt-check tf-validate tf-test tf-lint tf-scan` before pushing (`make tools` installs the pins), and `make tf-classify` to prove the committed `classification.json` describes every module resource. What a real plan costs a running deployment is `go run ./scripts/tfclassify -plan plan.json`, on the JSON of `terraform show -json <planfile>`; passing `make tf-classify` does not evaluate any plan.
