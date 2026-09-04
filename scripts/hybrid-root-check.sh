#!/bin/sh
# The Terraform root `billet init hybrid` writes validates against THIS
# checkout's module, for both trust shapes.
#
# WHY THIS EXISTS. The generator renders HCL as text and the Go tests parse
# the inventory, not the root: a typo in a module input, an output the root
# module no longer has, or an attribute the aws provider renamed would pass
# every Go test and fail an operator's first `terraform init`. Terraform is the
# only thing that can read the root, so terraform reads it.
#
# The generated source pins github.com/…?ref=<release>; the check rewrites it
# to the module in this checkout, because what is under test is whether the
# root's inputs and outputs match the module beside it.
set -eu

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT INT TERM

billet=${BILLET_HYBRID_CHECK_BINARY:-}
if [ -z "$billet" ]; then
    echo "Building billet to generate the root..."
    (cd "$repo_root" && go build -o "$work/billet" ./cmd/billet)
    billet=$work/billet
fi

module=$repo_root/terraform/modules/billet

check_shape() {
    shape=$1
    shift

    out=$work/$shape
    "$billet" init hybrid --out "$out" \
        --name hybrid-check --region us-west-2 --org acme \
        --control-plane-private-ip 10.60.0.10 \
        --local-vcpu 32 --local-memory 128GiB \
        --max-vcpu 16 --max-memory 32GiB \
        --instance-type 'c7i.xlarge=4,8GiB,0.17' \
        --instance-type 'c7i.2xlarge=8,16GiB,0.34' \
        "$@" >"$work/$shape.log" 2>&1 || {
        echo "hybrid-root-check: the $shape generation failed" >&2
        cat "$work/$shape.log" >&2
        exit 1
    }

    main=$out/terraform/main.tf
    [ -f "$main" ] || { echo "hybrid-root-check: $shape wrote no terraform/main.tf" >&2; exit 1; }

    # GENERATED HCL IS FORMATTED HCL. An operator's first `terraform fmt -check`
    # in CI would otherwise fail on a file billet wrote.
    if ! terraform fmt -check "$main" >/dev/null; then
        echo "hybrid-root-check: $shape terraform/main.tf is not terraform fmt clean" >&2
        terraform fmt -diff "$main" >&2 || true
        exit 1
    fi

    # The source line is one string; anything else on it would be a second
    # source and a rewrite that matched nothing.
    if ! grep -q 'source = "github.com/junioryono/billet//terraform/modules/billet?ref=' "$main"; then
        echo "hybrid-root-check: $shape root does not pin the billet module by ref" >&2
        exit 1
    fi
    sed -i.bak "s#\"github.com/junioryono/billet//terraform/modules/billet?ref=[^\"]*\"#\"$module\"#" "$main"
    rm -f "$main.bak"

    terraform -chdir="$out/terraform" init -backend=false -input=false >"$work/$shape.init.log" 2>&1 || {
        echo "hybrid-root-check: terraform init failed for the $shape root" >&2
        cat "$work/$shape.init.log" >&2
        exit 1
    }
    terraform -chdir="$out/terraform" validate
    echo "ok   the $shape root validates against this checkout's module"
}

check_shape untrusted
check_shape trusted --runner-group billet-trusted \
    --workflow 'acme/repo/.github/workflows/ci.yml@refs/heads/main'

echo "hybrid-root-check: both roots validate"
