package main

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/initconfig"
)

// codeBuildInitFlags is the codebuild-only flag set `billet init --provider
// codebuild` reads.
type codeBuildInitFlags struct {
	project       string
	environment   string
	fleetARN      string
	fleetCapacity int
	computeTypes  []string
	jitPath       string
	jitKMSKeyID   string
	logGroup      string
	privileged    bool
	buildTimeout  int
	queuedTimeout int
	acceptCeiling bool
	nodeName      string
	region        string
	maxVCPU       int
	maxMemory     string
}

// codeBuildInitParams turns the flags into initconfig inputs and returns the
// declared cloud budget, which IS the ceiling.
//
// NOTHING IS FETCHED, and that is the difference from ec2. There is a
// DescribeInstanceTypes for EC2 shapes and there is no equivalent for CodeBuild
// compute types — no API reports what BUILD_GENERAL1_MEDIUM holds — so the sizes
// are declared in the same breath as the names. Which means the operator states
// what they are buying, which is what the config would have required anyway.
func codeBuildInitParams(
	f codeBuildInitFlags,
) (*initconfig.CodeBuildParams, int, config.ByteSize, error) {
	if f.region == "" {
		return nil, 0, 0, errRequiredFlag("--region", "codebuild",
			"billet cannot choose which region to build in, and it is signed into every request")
	}

	// Validated before anything else uses it: the region is interpolated into the
	// API endpoint host, so a malformed one would otherwise send a signed request
	// to a host derived from it.
	if err := config.CheckCodeBuildRegion(f.region); err != nil {
		return nil, 0, 0, fmt.Errorf("--region: %w", err)
	}

	if f.project == "" {
		return nil, 0, 0, errRequiredFlag("--codebuild-project", "codebuild",
			"billet cannot choose which project to start builds in, and the project is half of "+
				"what tells its own builds from somebody else's — a CodeBuild build cannot be "+
				"tagged. Point it at a project DEDICATED to this deployment")
	}

	if f.environment == "" {
		return nil, 0, 0, errRequiredFlag("--codebuild-environment", "codebuild",
			"it decides whether this node runs Linux or macOS builds, which billet reports at "+
				"registration rather than taking a second answer from the config")
	}

	if f.jitPath == "" {
		return nil, 0, 0, errRequiredFlag("--jit-parameter-path", "codebuild",
			"it is an IAM boundary rather than a naming preference: the node's policy grants "+
				"PutParameter and DeleteParameter on exactly this path, so a value billet "+
				"guessed would either be unwritable or wider than the grant you reviewed")
	}

	if f.maxVCPU <= 0 {
		return nil, 0, 0, errRequiredFlag("--max-vcpu", "codebuild",
			"it is the cloud budget billet may run at once, and there is no host to detect it "+
				"from. Size it against your CONCURRENCY QUOTA as well as your wallet: "+
				"CodeBuild's default is one concurrent build per compute type")
	}

	if f.maxMemory == "" {
		return nil, 0, 0, errRequiredFlag("--max-memory", "codebuild",
			"it is the cloud budget billet may run at once, e.g. 64GiB, and there is no host "+
				"to detect it from")
	}

	budgetMemory, err := config.ParseByteSize(f.maxMemory)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("--max-memory: %w", err)
	}

	if len(f.computeTypes) == 0 {
		return nil, 0, 0, errRequiredFlag("--compute-type", "codebuild",
			"at least once: billet ships no table of CodeBuild compute types and no API "+
				"reports what one holds, so it must be told which ones it may buy and what "+
				"each is. They are ORDERED, most preferred first, and placement charges the "+
				"first that fits")
	}

	shapes, err := parseComputeTypes(f.computeTypes)
	if err != nil {
		return nil, 0, 0, err
	}

	return &initconfig.CodeBuildParams{
		Region:               f.region,
		Project:              f.project,
		Environment:          config.CodeBuildEnvironment(strings.TrimSpace(f.environment)),
		FleetARN:             f.fleetARN,
		FleetCapacity:        f.fleetCapacity,
		ComputeTypes:         shapes,
		JITParameterPath:     f.jitPath,
		JITKMSKeyID:          f.jitKMSKeyID,
		LogGroup:             f.logGroup,
		PrivilegedMode:       f.privileged,
		AcceptCeiling:        f.acceptCeiling,
		BuildTimeoutMinutes:  f.buildTimeout,
		QueuedTimeoutMinutes: f.queuedTimeout,
		NodeName:             f.nodeName,
	}, f.maxVCPU, budgetMemory, nil
}

// parseComputeTypes reads the repeatable --compute-type NAME=vcpu,memory,price.
//
// THE WHOLE SHAPE IN ONE FLAG, because the three numbers are one fact and
// splitting them across three repeatable flags makes them positional — an
// operator who omitted one price would silently shift every later one onto the
// wrong compute type. A malformed entry is refused rather than skipped, for the
// reason --price already gives: ignoring it would let billet write a catalogue
// the operator believes they specified.
func parseComputeTypes(raw []string) ([]config.RemoteShape, error) {
	out := make([]config.RemoteShape, 0, len(raw))
	seen := make(map[string]bool, len(raw))

	for _, entry := range raw {
		name, spec, ok := strings.Cut(entry, "=")
		name = strings.TrimSpace(name)

		if !ok || name == "" {
			return nil, fmt.Errorf("--compute-type %q is not NAME=vcpu,memory,price, e.g. "+
				"BUILD_GENERAL1_MEDIUM=4,7GiB,0.01", entry)
		}

		if seen[name] {
			return nil, fmt.Errorf("--compute-type %q is listed twice; the list is an ORDERED "+
				"preference and a duplicate has no meaning in one", name)
		}

		seen[name] = true

		fields := strings.Split(spec, ",")
		if len(fields) != 3 {
			return nil, fmt.Errorf("--compute-type %q needs vcpu,memory,price — all three, "+
				"because billet ships no table of what a compute type holds and a shape "+
				"smaller than the lease chosen for it overcommits a budget nobody can see",
				entry)
		}

		shape, err := computeShape(name, fields)
		if err != nil {
			return nil, fmt.Errorf("--compute-type %q: %w", entry, err)
		}

		out = append(out, shape)
	}

	return out, nil
}

// computeShape parses one declaration's three numbers.
func computeShape(name string, fields []string) (config.RemoteShape, error) {
	vcpu, err := parsePositiveInt(strings.TrimSpace(fields[0]))
	if err != nil {
		return config.RemoteShape{}, fmt.Errorf("vcpu: %w", err)
	}

	memory, err := config.ParseByteSize(strings.TrimSpace(fields[1]))
	if err != nil {
		return config.RemoteShape{}, fmt.Errorf("memory: %w", err)
	}

	if memory <= 0 {
		return config.RemoteShape{}, fmt.Errorf("memory must be more than zero")
	}

	price, err := config.ParseUSDPerHour(strings.TrimSpace(fields[2]))
	if err != nil {
		return config.RemoteShape{}, fmt.Errorf("price: %w", err)
	}

	if price <= 0 {
		return config.RemoteShape{}, fmt.Errorf("price must be more than zero; it is what " +
			"`billet status` reports this fleet's exposure from")
	}

	return config.RemoteShape{Type: name, VCPU: vcpu, Memory: memory, PriceUSDPerHour: price}, nil
}

// codeBuildOnlyFlagNames are the flags meaningful only to --provider codebuild.
var codeBuildOnlyFlagNames = []string{
	"codebuild-project", "codebuild-environment", "codebuild-fleet-arn",
	"codebuild-fleet-capacity", "compute-type", "jit-parameter-path", "jit-kms-key-id",
	"codebuild-log-group", "privileged", "build-timeout-minutes", "queued-timeout-minutes",
	"accept-external-build-ceiling",
}

// refuseCodeBuildOnlyFlags stops a codebuild-only flag from being silently
// ignored on another backend, where it would read as configured and do nothing.
//
// PRESENCE, NOT VALUE, the rule refuseEC2OnlyFlags and refuseTartOnlyFlags both
// follow: `set` is what the operator actually passed, so
// `--codebuild-fleet-capacity 0` is a misuse rather than an omission and is
// caught the same as a real one.
// THE GUARD IS HERE RATHER THAN AT THE CALL SITE, which is where the sibling
// refusals put it and is why a review's counterpart test caught this: a function
// named "refuse the codebuild-only flags" that refuses them on CODEBUILD too is
// one whose correctness rests entirely on an `if` somewhere else. The caller
// still reads clearly, and the rule now lives in the function that states it.
func refuseCodeBuildOnlyFlags(kind config.ProviderKind, set map[string]bool) error {
	if kind == config.ProviderCodeBuild {
		return nil
	}

	var used []string

	for _, name := range codeBuildOnlyFlagNames {
		if set[name] {
			used = append(used, "--"+name)
		}
	}

	if len(used) > 0 {
		return fmt.Errorf("%s can only be used with --provider codebuild, but this is a %s "+
			"config", strings.Join(used, ", "), kind)
	}

	return nil
}

// parsePositiveInt reads a count, refusing a zero, a negative or anything that
// is not a plain decimal.
//
// strconv.Atoi ALONE ACCEPTS A LEADING SIGN, so "+4" and "-4" both parse; the
// second would reach config validation as a shape holding negative vCPU, which
// arithmetic on a budget reads as capacity.
func parsePositiveInt(s string) (int, error) {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("%q is not a whole number", s)
	}

	if n <= 0 {
		return 0, fmt.Errorf("%q must be more than zero", s)
	}

	return n, nil
}

// errRequiredFlag is one shape for "this backend cannot be generated without
// knowing X", so every such refusal names the flag first and then says what the
// value decides.
func errRequiredFlag(flag, backend, why string) error {
	return fmt.Errorf("%s is required for %s: %s", flag, backend, why)
}
