#!/usr/bin/env bash
# lambda-test.sh: unit-test the Terraform spot-router Lambda (standard library
# only, boto3 stubbed). Its error CLASSIFICATION, drop or re-raise, decides
# whether a real two-minute Spot warning is lost, and a Terraform plan test
# cannot reach it.
set -euo pipefail

here=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd -P)
. "$here/env.sh"

python3 -m unittest discover -s terraform/modules/billet/modules/fleet-ec2/lambda -p '*_test.py'
