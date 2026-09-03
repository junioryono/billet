package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/provider/codebuild"
)

// THE FLEET'S CAPACITY IS JUDGED AGAINST THE DECLARED macOS LIMIT, AND ONLY UNDER
// THREE CONDITIONS.
//
// Each case here is a deployment that is CORRECT and would be refused if its
// condition were dropped (ADR-005: a check that refuses correct deployments gets
// deleted): a Linux fleet beside a policy that legally declares a macOS limit no
// tier uses; a node-only config with no policy at all; a policy that leaves the
// limit unset, whose fallback is Apple's per-host allowance and describes no fleet.
// And the one case that must refuse, so the three guards cannot be satisfied by
// returning nothing.
func TestTheFleetConcurrencyJudgementAppliesOnlyToADeclaredMacOSLimit(t *testing.T) {
	two := 2
	fleet := codebuild.FleetReport{Name: "macs", BaseCapacity: 1}

	deployment := func(env config.CodeBuildEnvironment, nodes ...config.NodePolicy) *config.Config {
		return &config.Config{
			Node: &config.NodeConfig{
				Name:      "cb",
				Provider:  config.ProviderCodeBuild,
				CodeBuild: &config.CodeBuildConfig{EnvironmentType: env},
			},
			Nodes: nodes,
		}
	}

	quiet := map[string]*config.Config{
		"a Linux fleet beside a declared limit": deployment(config.CodeBuildLinuxContainer,
			config.NodePolicy{Name: "cb", Provider: config.ProviderCodeBuild, MacOSVMLimit: &two}),
		"a node-only config with no policy": deployment(config.CodeBuildMacARM),
		"a policy for a different node": deployment(config.CodeBuildMacARM,
			config.NodePolicy{Name: "other", Provider: config.ProviderCodeBuild, MacOSVMLimit: &two}),
		"a policy with no explicit limit": deployment(config.CodeBuildMacARM,
			config.NodePolicy{Name: "cb", Provider: config.ProviderCodeBuild}),
	}

	for name, cfg := range quiet {
		fatal, warnings := judgeCodeBuildFleetConcurrency(cfg, fleet)
		if len(fatal)+len(warnings) != 0 {
			t.Errorf("%s produced a verdict against a fleet it does not use:\nfatal=%q\nwarnings=%q",
				name, fatal, warnings)
		}
	}

	fatal, _ := judgeCodeBuildFleetConcurrency(deployment(config.CodeBuildMacARM,
		config.NodePolicy{Name: "cb", Provider: config.ProviderCodeBuild, MacOSVMLimit: &two}), fleet)

	var refused bool

	for _, f := range fatal {
		if strings.Contains(f, "macos_vm_limit 2") && strings.Contains(f, "set nodes[].macos_vm_limit to 1") {
			refused = true
		}
	}

	if !refused {
		t.Errorf("a MAC_ARM node declaring 2 against a fleet of 1 was not refused: %q", fatal)
	}
}

// AND THE LIVE CHECK ASKS THAT JUDGEMENT, WITH THE FLEET IT DESCRIBED.
//
// PROVING THE MECHANISM IS NOT PROVING IT IS USED. The judgement above is a plain
// function; what turns the measurement in docs/reference/records/aws-acceptance.md
// into a refusal an
// operator meets is one call in checkCodeBuildLive. Delete it and every suite
// stays green while a tier advertises two Macs to GitHub against a fleet that has
// one. A structural test because the hazard is an absence: exercising it at run
// time needs a fake CodeBuild, a node certificate and a policy-carrying config, all
// to observe one string. The ARGUMENTS are asserted as well as the call, because
// judging a report other than the one DescribeFleet returned judges nothing.
func TestTheLiveCodeBuildCheckJudgesTheDeclaredMacOSLimitAgainstTheFleet(t *testing.T) {
	fset := token.NewFileSet()

	file, err := parser.ParseFile(fset, "codebuildcheck.go", nil, 0)
	if err != nil {
		t.Fatalf("parse codebuildcheck.go: %v", err)
	}

	var found bool

	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "checkCodeBuildLive" {
			continue
		}

		ast.Inspect(fn, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}

			ident, ok := call.Fun.(*ast.Ident)
			if !ok || ident.Name != "judgeCodeBuildFleetConcurrency" || len(call.Args) != 2 {
				return true
			}

			cfgArg, ok := call.Args[0].(*ast.Ident)
			if !ok || cfgArg.Name != "cfg" {
				return true
			}

			fleetArg, ok := call.Args[1].(*ast.Ident)
			if ok && fleetArg.Name == "fleet" {
				found = true
			}

			return true
		})
	}

	if !found {
		t.Fatal("checkCodeBuildLive does not judge the described fleet against the config's " +
			"declared macOS limit; a macos_vm_limit above the fleet advertises Macs that do not exist")
	}
}
