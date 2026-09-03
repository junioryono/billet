# SHARED BY EVERY MODULE, AND PASSED BY ABSOLUTE PATH.
#
# This used to live in terraform/modules/billet, and tflint resolves its config
# relative to --chdir. So every sibling module was linted with the bundled
# terraform rules only and never saw the AWS ruleset at all -- a gate reporting
# success for work it had not done, which is the same failure that let two
# unexamined modules merge in the first place.
# Pinned, like every other linter in this repo: a ruleset that changes under you
# turns an unrelated PR red and trains people to ignore the signal.
plugin "aws" {
  enabled = true
  version = "0.48.0"
  source  = "github.com/terraform-linters/tflint-ruleset-aws"
}
