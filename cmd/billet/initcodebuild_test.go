package main

import (
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/config"
)

// baseCodeBuildFlags is an invocation that should succeed, so a case can break
// exactly one thing.
func baseCodeBuildFlags() codeBuildInitFlags {
	return codeBuildInitFlags{
		region:        "us-west-2",
		project:       "billet-runners",
		environment:   "LINUX_CONTAINER",
		jitPath:       "/billet/jit",
		computeTypes:  []string{"BUILD_GENERAL1_MEDIUM=4,7GiB,0.01"},
		maxVCPU:       16,
		maxMemory:     "32GiB",
		acceptCeiling: true,
	}
}

// EVERY VALUE BILLET CANNOT DETECT IS ASKED FOR BY NAME.
//
// A codebuild node has no machine to measure and no API that reports what a
// compute type holds, so each of these is a decision somebody has to make — and
// a refusal that does not name the flag leaves an operator reading a config-load
// error about a file billet wrote itself.
func TestCodeBuildInitAsksForWhatItCannotKnow(t *testing.T) {
	t.Parallel()

	for flag, break_ := range map[string]func(*codeBuildInitFlags){
		"--region":                func(f *codeBuildInitFlags) { f.region = "" },
		"--codebuild-project":     func(f *codeBuildInitFlags) { f.project = "" },
		"--codebuild-environment": func(f *codeBuildInitFlags) { f.environment = "" },
		"--jit-parameter-path":    func(f *codeBuildInitFlags) { f.jitPath = "" },
		"--max-vcpu":              func(f *codeBuildInitFlags) { f.maxVCPU = 0 },
		"--max-memory":            func(f *codeBuildInitFlags) { f.maxMemory = "" },
		"--compute-type":          func(f *codeBuildInitFlags) { f.computeTypes = nil },
	} {
		t.Run(flag, func(t *testing.T) {
			t.Parallel()

			f := baseCodeBuildFlags()
			break_(&f)

			_, _, _, err := codeBuildInitParams(f)
			if err == nil {
				t.Fatalf("a generation was accepted with %s missing", flag)
			}

			if !strings.Contains(err.Error(), flag) {
				t.Errorf("the refusal does not name %s: %v", flag, err)
			}
		})
	}
}

// A COMPUTE TYPE DECLARES ALL THREE OF ITS NUMBERS, OR NONE OF THEM.
//
// billet ships no table of what a CodeBuild compute type holds and no API
// reports one, so a shape smaller than the lease chosen for it overcommits a
// budget nobody can see. A malformed entry is refused rather than skipped, for
// the reason --price already gives: ignoring it would let billet write a
// catalogue the operator believes they specified.
func TestAComputeTypeMustDeclareWhatItHolds(t *testing.T) {
	t.Parallel()

	for name, entry := range map[string]string{
		"no equals":         "BUILD_GENERAL1_MEDIUM",
		"no name":           "=4,7GiB,0.01",
		"two fields":        "BUILD_GENERAL1_MEDIUM=4,7GiB",
		"four fields":       "BUILD_GENERAL1_MEDIUM=4,7GiB,0.01,extra",
		"vcpu not a number": "BUILD_GENERAL1_MEDIUM=four,7GiB,0.01",
		"zero vcpu":         "BUILD_GENERAL1_MEDIUM=0,7GiB,0.01",
		"negative vcpu":     "BUILD_GENERAL1_MEDIUM=-4,7GiB,0.01",
		"bad memory":        "BUILD_GENERAL1_MEDIUM=4,seven,0.01",
		"zero price":        "BUILD_GENERAL1_MEDIUM=4,7GiB,0",
		"bad price":         "BUILD_GENERAL1_MEDIUM=4,7GiB,free",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if _, err := parseComputeTypes([]string{entry}); err == nil {
				t.Fatalf("%q was accepted as a compute type", entry)
			}
		})
	}
}

// AND A DUPLICATE HAS NO MEANING IN AN ORDERED LIST.
//
// The catalogue is a PREFERENCE — placement charges the first entry that fits —
// so a name appearing twice is either a mistake or an operator expecting
// something the order cannot express.
func TestADuplicateComputeTypeIsRefused(t *testing.T) {
	t.Parallel()

	_, err := parseComputeTypes([]string{
		"BUILD_GENERAL1_MEDIUM=4,7GiB,0.01",
		"BUILD_GENERAL1_MEDIUM=4,7GiB,0.02",
	})
	if err == nil {
		t.Fatal("a compute type listed twice was accepted into an ordered preference list")
	}

	if !strings.Contains(err.Error(), "twice") {
		t.Errorf("the refusal does not say what is wrong: %v", err)
	}
}

// THE ORDER SURVIVES PARSING, because it is the decision: placement charges the
// first declared shape that fits, so a catalogue parsed into a different order
// buys a different machine.
func TestComputeTypesKeepTheOrderTheyWereGivenIn(t *testing.T) {
	t.Parallel()

	shapes, err := parseComputeTypes([]string{
		"BUILD_GENERAL1_LARGE=8,15GiB,0.02",
		"BUILD_GENERAL1_SMALL=2,3GiB,0.005",
	})
	if err != nil {
		t.Fatalf("parseComputeTypes: %v", err)
	}

	if len(shapes) != 2 ||
		shapes[0].Type != "BUILD_GENERAL1_LARGE" || shapes[1].Type != "BUILD_GENERAL1_SMALL" {
		t.Fatalf("the catalogue was reordered: %+v", shapes)
	}

	if shapes[0].VCPU != 8 || shapes[0].Memory != 15*config.GiB {
		t.Errorf("a shape lost its declared size: %+v", shapes[0])
	}
}

// A CODEBUILD-ONLY FLAG ON ANOTHER BACKEND IS REFUSED, NOT DISCARDED.
//
// PRESENCE, NOT VALUE — the rule refuseEC2OnlyFlags and refuseTartOnlyFlags both
// follow. On this backend the discarded value would be a project, a fleet or an
// IAM path, none of which reads as absent from the file that comes out.
func TestCodeBuildOnlyFlagsAreRefusedElsewhere(t *testing.T) {
	t.Parallel()

	for _, name := range codeBuildOnlyFlagNames {
		err := refuseCodeBuildOnlyFlags(config.ProviderDocker, map[string]bool{name: true})
		if err == nil {
			t.Errorf("--%s was accepted on a docker generation, where it does nothing", name)

			continue
		}

		if !strings.Contains(err.Error(), "--"+name) {
			t.Errorf("the refusal for --%s does not name it: %v", name, err)
		}
	}

	// ...and it is not a refusal of everything. WITH EVERY FLAG SET, not with
	// none: a review found the first version passing `nil`, which holds whatever
	// the function does, because there is nothing there to refuse. The counterpart
	// has to be the same input the refusal above rejects, on the backend that owns
	// it.
	all := make(map[string]bool, len(codeBuildOnlyFlagNames))
	for _, name := range codeBuildOnlyFlagNames {
		all[name] = true
	}

	if err := refuseCodeBuildOnlyFlags(config.ProviderCodeBuild, all); err != nil {
		t.Errorf("a codebuild generation was refused its own flags: %v", err)
	}
}

// AND AN EC2 PLACEMENT FLAG IS REFUSED ON CODEBUILD.
//
// The two backends share a region, a budget and a shape catalogue, and share
// none of the network: a CodeBuild build runs on a machine in AWS's own account,
// so a subnet or a security group written into a codebuild invocation is
// somebody configuring a network the compute will never be in.
func TestEC2PlacementFlagsAreRefusedOnCodeBuild(t *testing.T) {
	t.Parallel()

	for _, name := range ec2PlacementFlagNames {
		err := refuseEC2PlacementFlags(config.ProviderCodeBuild, map[string]bool{name: true})
		if err == nil {
			t.Errorf("--%s was accepted on a codebuild generation", name)
		}
	}

	// The flags codebuild DOES share are not swept up with them. --region,
	// --max-vcpu and --max-memory all mean something here, and a refusal that
	// took the whole ec2 list would make the backend ungeneratable.
	for _, name := range []string{"region", "max-vcpu", "max-memory"} {
		if err := refuseEC2PlacementFlags(config.ProviderCodeBuild,
			map[string]bool{name: true}); err != nil {
			t.Errorf("--%s is refused on codebuild, which needs it: %v", name, err)
		}
	}
}
