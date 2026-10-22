#!/usr/bin/env bash
# terraform.sh STEP...: the Terraform gates, each step over every top-level module
# terraform-modules.sh discovers. They are outside check.sh because they need
# terraform, tflint and trivy installed, which the Go gate must not depend on;
# CI runs them on every pull request a .tf change selects (scripts/ci-scope.sh).
#
#   fmt       terraform fmt -check, recursively over terraform/
#   validate  terraform validate of every module (init downloads providers, no
#             credentials)
#   test      the plan tests of every module that has a tests/ directory, against
#             mocked providers
#   classify  prove the plan classification describes every module resource and
#             nothing else (Go only: no terraform needed)
#   lint      tflint, with the aws ruleset pinned in terraform/.tflint.hcl
#   scan      trivy's config scan; every ignore carries its justification
#
# validate and test also reach each module's CHILDREN, because a root loads them
# as called modules; that was once the whole story, and it is why a sibling module
# could be added and never gated (see terraform-modules.sh).
set -euo pipefail

here=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd -P)
. "$here/env.sh"

if [ "$#" -eq 0 ]; then
	printf 'usage: %s fmt|validate|test|classify|lint|scan...\n' "${0##*/}" >&2
	exit 2
fi

# READ ONCE, and a refusal ends the run: an empty or partial list would make
# every step below pass without examining anything.
modules=$("$here/terraform-modules.sh")
# AN ARRAY, not a list read line by line beside each step, so a command a step
# runs cannot read the rest of the list from its standard input.
mods=()
while IFS= read -r m; do
	mods+=("$m")
done <<<"$modules"
printf 'terraform modules: %s\n' "${mods[*]}"

for step in "$@"; do
	case "$step" in
	fmt)
		terraform fmt -check -recursive terraform
		;;
	validate)
		for m in "${mods[@]}"; do
			printf 'validate %s\n' "$m"
			terraform -chdir="$m" init -input=false -backend=false >/dev/null
			terraform -chdir="$m" validate
		done
		;;
	test)
		for m in "${mods[@]}"; do
			if [ -d "$m/tests" ]; then
				printf 'test %s\n' "$m"
				terraform -chdir="$m" init -input=false >/dev/null
				terraform -chdir="$m" test
			else
				printf 'test %s: no tests/ directory, skipped\n' "$m"
			fi
		done
		;;
	classify)
		# WHAT A CHANGE COSTS A RUNNING DEPLOYMENT is committed beside the module as
		# classification.json, because `terraform plan` cannot say it: ADR-004 keeps
		# live billet nodes outside Terraform, so a plan does not know these hosts
		# are running somebody's build and "1 to change" reads the same for a tag
		# and for the instance holding the ledger. THIS IS THE VACUITY GATE, not the
		# classifier: a resource with no entry is one `tfclassify` refuses at apply
		# time, far too late. Classifying an actual plan is the operator's
		# invocation:
		#   terraform -chdir=terraform/modules/billet plan -out=tfplan
		#   terraform -chdir=terraform/modules/billet show -json tfplan > plan.json
		#   go run ./scripts/tfclassify -plan plan.json
		go test -count=1 ./internal/tfclass/
		;;
	lint)
		# --config TAKES AN ABSOLUTE PATH. tflint resolves a relative config against
		# --chdir, so a shared file named relatively would silently not be found and
		# the module would be linted with the bundled rules only.
		cfg=$gates_root/terraform/.tflint.hcl
		for m in "${mods[@]}"; do
			printf 'tflint %s\n' "$m"
			tflint --chdir="$m" --config="$cfg" --init
			tflint --chdir="$m" --config="$cfg" --recursive
		done
		;;
	scan)
		# --skip-check-update pins the rego bundle to the one embedded in the pinned
		# binary; without it trivy fetches new checks daily and an unrelated pull
		# request goes red, the exact failure every other pin exists to prevent.
		# The second pass scans with every feature flag on: trivy expands count,
		# and spot and KMS default off, so the default pass alone never sees that
		# surface. Each module carries its OWN ignorefile when it needs one,
		# because an ignore is a justification about that module's resources and
		# borrowing another's silences a finding nobody argued for.
		for m in "${mods[@]}"; do
			printf 'trivy %s\n' "$m"
			ignore=()
			if [ -f "$m/.trivyignore" ]; then
				ignore=(--ignorefile "$m/.trivyignore")
			fi
			trivy config "$m" ${ignore[@]+"${ignore[@]}"} --skip-check-update --exit-code 1
			if [ -f "$m/tests/scan.tfvars" ]; then
				trivy config "$m" ${ignore[@]+"${ignore[@]}"} --skip-check-update --tf-vars "$m/tests/scan.tfvars" --exit-code 1
			fi
		done
		;;
	*)
		printf 'terraform: no step %q (fmt, validate, test, classify, lint or scan)\n' "$step" >&2
		exit 2
		;;
	esac
done
