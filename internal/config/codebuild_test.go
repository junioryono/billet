package config

import (
	"strings"
	"testing"
)

// codeBuildNode is a complete, valid CodeBuild node — on-demand Linux, which is
// the inexpensive default the terraform example leads with.
//
// The tiers in validConfig pin themselves to firecracker, which is fine: a node
// declaring a backend no tier accepts is a deployment that has not finished being
// written, not an error.
const codeBuildNode = `node:
  name: aws-cb-1
  server_addr: 127.0.0.1:7717
  provider: codebuild
  state_dir: /var/lib/billet/node
  max_vcpu: 64
  max_memory: 256GiB
` + codeBuildBlock

// codeBuildBlock is the backend's own section, kept separate so a case can remove
// all of it at once.
const codeBuildBlock = `  codebuild:
    region: us-west-2
    project: billet-linux
    environment_type: LINUX_CONTAINER
    privileged_mode: true
    accept_external_build_ceiling: true
    jit_parameter_path: /billet/jit
    compute_types:
      - type: BUILD_GENERAL1_MEDIUM
        vcpu: 4
        memory: 7GiB
        price_usd_per_hour: 0.01
`

// codeBuildConfig is validConfig with its node replaced by a CodeBuild one,
// optionally with a single substitution applied.
func codeBuildConfig(t *testing.T, old, replacement string) string {
	t.Helper()

	body := strings.Replace(validConfig, firecrackerNode, codeBuildNode, 1)
	if !strings.Contains(body, "provider: codebuild") {
		t.Fatal("the node block in validConfig has changed, so these cases patch nothing")
	}

	if old == "" {
		return body
	}

	patched := strings.Replace(body, old, replacement, 1)
	if patched == body {
		t.Fatalf("substituting %q changed nothing; the fixture has drifted", old)
	}

	return patched
}

// THE FIXTURE ITSELF HAS TO LOAD, or every case below passes for the wrong reason.
//
// A negative-only suite is the failure this repository keeps finding: each case
// asserts "this was refused", and a fixture that is broken for an unrelated reason
// satisfies all of them at once while proving nothing about the rule each names.
func TestAValidCodeBuildNodeLoads(t *testing.T) {
	cfg, err := Load(writeConfig(t, codeBuildConfig(t, "", "")))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Node.CodeBuild == nil {
		t.Fatal("the codebuild block did not survive loading")
	}

	// THE DEFAULTS ARE CODEBUILD'S OWN MAXIMA, so a config that says nothing gets
	// the longest job the service permits rather than a shorter one billet chose.
	if got := cfg.Node.CodeBuild.BuildTimeoutMinutes; got != CodeBuildBuildCeilingMinutes {
		t.Errorf("build_timeout_minutes defaulted to %d, want %d",
			got, CodeBuildBuildCeilingMinutes)
	}

	if got := cfg.Node.CodeBuild.QueuedTimeoutMinutes; got != CodeBuildQueuedCeilingMinutes {
		t.Errorf("queued_timeout_minutes defaulted to %d, want %d",
			got, CodeBuildQueuedCeilingMinutes)
	}
}

// A CODEBUILD NODE HAS NO MACHINE TO MEASURE, SO IT HAS TO SAY — the ec2 rule, and
// it has to hold for the second remote backend or detection reports whatever small
// instance holds the process as the capacity of a managed fleet.
func TestACodeBuildNodeMustSayWhatItWillBuy(t *testing.T) {
	for name, tc := range map[string]struct{ old, want string }{
		"no cores":  {old: "  max_vcpu: 64\n", want: "max_vcpu"},
		"no memory": {old: "  max_memory: 256GiB\n", want: "max_memory"},
	} {
		t.Run(name, func(t *testing.T) {
			got := loadErr(t, codeBuildConfig(t, tc.old, ""))

			if !strings.Contains(got, tc.want) {
				t.Errorf("the error does not name %s: %s", tc.want, got)
			}

			if !strings.Contains(got, "detect") {
				t.Errorf("the error does not explain that there is nothing to detect: %s", got)
			}
		})
	}
}

// THE CEILING ACKNOWLEDGEMENT HAS NO DEFAULT, AND ITS ABSENCE IS THE REFUSAL.
//
// The same shape as node.firecracker.untrusted_bridge and
// node.ec2.untrusted_security_group_ids. It is not a feature flag — nothing about
// billet's behaviour changes when it is set — so the only thing it can buy is that
// somebody read the sentence before a tier advertised capacity. A default would
// spend that for nothing.
func TestACodeBuildNodeMustAcknowledgeTheExternalCeilings(t *testing.T) {
	for name, body := range map[string]string{
		"absent":         codeBuildConfig(t, "    accept_external_build_ceiling: true\n", ""),
		"explicitly off": codeBuildConfig(t, "accept_external_build_ceiling: true", "accept_external_build_ceiling: false"),
	} {
		t.Run(name, func(t *testing.T) {
			got := loadErr(t, body)

			if !strings.Contains(got, "accept_external_build_ceiling") {
				t.Errorf("the error does not name the field: %s", got)
			}

			// BOTH CEILINGS, because the queued one is the surprise. An operator who
			// only ever hears about 36 hours meets the 8-hour queued failure as an
			// unexplained red build on a busy fleet.
			for _, want := range []string{"2160", "480", "36 hours", "8 hours"} {
				if !strings.Contains(got, want) {
					t.Errorf("the error does not mention %q, so an operator cannot see which "+
						"limit they are accepting: %s", want, got)
				}
			}

			// AND WHERE TO PUT WORK THAT CANNOT ACCEPT THEM, or the refusal is a dead
			// end rather than a decision.
			if !strings.Contains(got, "owned EC2 or Mac capacity") {
				t.Errorf("the error does not say where longer work belongs: %s", got)
			}
		})
	}
}

// THE BLOCK IS REQUIRED FOR THIS BACKEND AND REFUSED FOR EVERY OTHER, the rule
// node.ec2, node.firecracker and node.ceph all follow: nothing else reads it, so
// elsewhere it is a project, a fleet and a parameter path that look configured and
// are consulted by nothing.
func TestTheCodeBuildBlockBelongsToItsOwnBackend(t *testing.T) {
	t.Run("required", func(t *testing.T) {
		got := loadErr(t, codeBuildConfig(t, codeBuildBlock, ""))

		if !strings.Contains(got, "node.codebuild is required") {
			t.Errorf("the error does not say the block is required: %s", got)
		}
	})

	t.Run("refused elsewhere", func(t *testing.T) {
		// A firecracker node carrying a leftover codebuild block, which is exactly
		// what switching a host's provider and forgetting the old section produces.
		body := strings.Replace(validConfig, firecrackerNode, firecrackerNode+codeBuildBlock, 1)

		got := loadErr(t, body)
		if !strings.Contains(got, "node.codebuild is set but this node's provider is firecracker") {
			t.Errorf("a leftover codebuild block on another backend was not refused by name: %s", got)
		}

		if !strings.Contains(got, "nothing consults") {
			t.Errorf("the error does not explain that the block would be inert: %s", got)
		}
	})
}

// THE ENVIRONMENT TYPE DECIDES THE GUEST OS, so it cannot be omitted or guessed.
func TestACodeBuildNodeNeedsAnEnvironmentType(t *testing.T) {
	got := loadErr(t, codeBuildConfig(t, "    environment_type: LINUX_CONTAINER\n", ""))

	if !strings.Contains(got, "environment_type is required") {
		t.Errorf("an absent environment_type was not refused: %s", got)
	}
}

// EACH EXCLUDED ENVIRONMENT GETS ITS OWN REASON, because both are things an
// operator would reasonably reach for and the two are wrong for different reasons.
func TestCodeBuildRefusesEnvironmentsThatCannotRunAJob(t *testing.T) {
	for name, tc := range map[string]struct{ value, want string }{
		// Lambda compute has no container privilege, so `docker build`, service
		// containers and compose all fail — the same reason ADR-002 disqualified
		// Lambda outright.
		"lambda": {value: "LINUX_LAMBDA_CONTAINER", want: "no container privilege"},
		// CodeBuild really does offer Windows, and billet ships no Windows runner.
		"windows": {value: "WINDOWS_SERVER_2022_CONTAINER", want: "no Windows runner"},
		"unknown": {value: "MOON_CONTAINER", want: "is not one of"},
	} {
		t.Run(name, func(t *testing.T) {
			got := loadErr(t, codeBuildConfig(t,
				"environment_type: LINUX_CONTAINER", "environment_type: "+tc.value))

			if !strings.Contains(got, tc.want) {
				t.Errorf("the error does not explain the exclusion (%q): %s", tc.want, got)
			}
		})
	}
}

// MACOS IS RESERVED CAPACITY ONLY, measured against AWS's own documentation: an
// on-demand fleet does not offer macOS at all. Without this the launch is refused
// per job, which reads as a transient failure rather than a config that can never
// work.
func TestAMacOSCodeBuildNodeNeedsAFleet(t *testing.T) {
	body := codeBuildConfig(t, "environment_type: LINUX_CONTAINER", "environment_type: MAC_ARM")
	// privileged_mode is meaningless on a Mac and would produce a second, unrelated
	// diagnostic that this case is not about.
	body = strings.Replace(body, "    privileged_mode: true\n", "", 1)

	got := loadErr(t, body)
	if !strings.Contains(got, "fleet_arn is required") {
		t.Errorf("a macOS node without a fleet was not refused: %s", got)
	}

	if !strings.Contains(got, "reserved capacity") {
		t.Errorf("the error does not say why: %s", got)
	}
}

// AND THE OTHER DIRECTION: a reserved macOS node with a fleet loads, and reports
// macOS as its guest OS. A one-directional suite would pass against a validator
// that refused every macOS node.
func TestAMacOSCodeBuildNodeWithAFleetLoads(t *testing.T) {
	body := codeBuildConfig(t, "environment_type: LINUX_CONTAINER",
		"environment_type: MAC_ARM\n    fleet_arn: arn:aws:codebuild:us-west-2:000000000000:fleet/macs")
	body = strings.Replace(body, "    privileged_mode: true\n", "", 1)

	cfg, err := Load(writeConfig(t, body))
	if err != nil {
		t.Fatalf("a reserved macOS node was refused: %v", err)
	}

	if got := cfg.Node.CodeBuild.EnvironmentType.GuestOS(); got != GuestMacOS {
		t.Errorf("MAC_ARM reports guest OS %q, want %q", got, GuestMacOS)
	}

	if cfg.Node.CodeBuild.EnvironmentType.Container() {
		t.Error("MAC_ARM reports itself as a container environment, so privileged_mode " +
			"would be accepted where it means nothing")
	}
}

// A FLEET IN ANOTHER REGION CANNOT SERVE THESE BUILDS, and without this check the
// failure is a per-job refusal from a request billet signed for a different region
// than the fleet lives in — naming neither field.
func TestACodeBuildFleetMustBeInTheSignedRegion(t *testing.T) {
	got := loadErr(t, codeBuildConfig(t, "    project: billet-linux\n",
		"    project: billet-linux\n    fleet_arn: arn:aws:codebuild:eu-west-1:000000000000:fleet/macs\n"))

	if !strings.Contains(got, "names region") || !strings.Contains(got, "us-west-2") {
		t.Errorf("a cross-region fleet was not refused with both regions named: %s", got)
	}
}

func TestACodeBuildFleetMustLookLikeAFleetARN(t *testing.T) {
	for name, value := range map[string]string{
		"not an arn":     "my-fleet",
		"wrong service":  "arn:aws:ec2:us-west-2:000000000000:fleet/macs",
		"wrong resource": "arn:aws:codebuild:us-west-2:000000000000:project/macs",
	} {
		t.Run(name, func(t *testing.T) {
			got := loadErr(t, codeBuildConfig(t, "    project: billet-linux\n",
				"    project: billet-linux\n    fleet_arn: "+value+"\n"))

			if !strings.Contains(got, "is not a codebuild fleet arn") {
				t.Errorf("%q was not refused as a fleet arn: %s", value, got)
			}
		})
	}
}

// PRIVILEGE IS REFUSED WHERE IT MEANS NOTHING rather than ignored. A setting that
// reads as "Docker will work" and does nothing is worse than its absence.
func TestPrivilegedModeIsRefusedOutsideAContainer(t *testing.T) {
	body := codeBuildConfig(t, "environment_type: LINUX_CONTAINER",
		"environment_type: LINUX_EC2\n    fleet_arn: arn:aws:codebuild:us-west-2:000000000000:fleet/linux")

	got := loadErr(t, body)
	if !strings.Contains(got, "privileged_mode is set") {
		t.Errorf("privileged_mode on a non-container environment was not refused: %s", got)
	}

	if !strings.Contains(got, "directly on the machine") {
		t.Errorf("the error does not explain why there is nothing to privilege: %s", got)
	}
}

// THE PARAMETER PATH IS AN IAM BOUNDARY, NOT A NAMING PREFERENCE.
func TestTheJITParameterPathIsRequiredAndBounded(t *testing.T) {
	t.Run("required", func(t *testing.T) {
		got := loadErr(t, codeBuildConfig(t, "    jit_parameter_path: /billet/jit\n", ""))

		if !strings.Contains(got, "jit_parameter_path is required") {
			t.Errorf("an absent parameter path was not refused: %s", got)
		}

		// It has to say WHY it cannot be defaulted, or an operator reads billet as
		// obtuse about a value it could obviously have picked.
		if !strings.Contains(got, "IAM policy is scoped") {
			t.Errorf("the error does not explain that the path is an IAM boundary: %s", got)
		}
	})

	for name, tc := range map[string]struct{ value, want string }{
		// THE WILDCARD IS THE DANGEROUS ONE: it reads as a harmless glob and is a
		// widened IAM Resource, and on a shared account the sibling paths it admits
		// are other deployments' runner registrations.
		"wildcard":       {value: "/billet/*", want: "widens the node's parameter grant"},
		"relative":       {value: "billet/jit", want: "absolute"},
		"trailing slash": {value: "/billet/jit/", want: "trailing slash"},
		"reserved aws":   {value: "/aws/billet", want: "namespace AWS reserves"},
		"reserved ssm":   {value: "/ssm/billet", want: "namespace AWS reserves"},

		// THE RESERVED NAMESPACES ARE CASE-INSENSITIVE, and the check was not — the
		// path grammar admits uppercase, so `/AWS/billet` loaded and then failed on
		// every registration, which is a tier advertising capacity it can never
		// serve. MEASURED in us-west-2 on 2026-08-31: PutParameter refuses `/AWS/…`
		// and `/Aws/…` with AccessDeniedException ("No access to reserved parameter
		// name") and `/SSM/…` with a ValidationException whose own text says the rule
		// is case-insensitive.
		"reserved AWS":   {value: "/AWS/billet", want: "namespace AWS reserves"},
		"reserved Aws":   {value: "/Aws/billet", want: "namespace AWS reserves"},
		"reserved SSM":   {value: "/SSM/billet", want: "namespace AWS reserves"},
		"reserved mixed": {value: "/sSm/billet", want: "namespace AWS reserves"},
	} {
		t.Run(name, func(t *testing.T) {
			got := loadErr(t, codeBuildConfig(t,
				"jit_parameter_path: /billet/jit", "jit_parameter_path: "+tc.value))

			if !strings.Contains(got, tc.want) {
				t.Errorf("%q was not refused with %q: %s", tc.value, tc.want, got)
			}
		})
	}
}

// A WILDCARD KMS KEY WOULD WIDEN THE NODE'S GRANT TO EVERY KEY IT MATCHES, which
// is the same rule the parameter path follows and for the same reason: the value
// lands in an IAM Resource.
func TestAWildcardKMSKeyIsRefused(t *testing.T) {
	got := loadErr(t, codeBuildConfig(t, "    project: billet-linux\n",
		"    project: billet-linux\n    jit_kms_key_id: alias/billet-*\n"))

	if !strings.Contains(got, "jit_kms_key_id") || !strings.Contains(got, "widen") {
		t.Errorf("a wildcard key was not refused: %s", got)
	}
}

// A CEILING CODEBUILD WOULD REJECT IS REFUSED AT LOAD, with the service named as
// the party imposing it — because it is, and telling an operator that billet
// refuses a 40-hour build reads as billet's choice.
func TestCodeBuildTimeoutsAreBoundedByTheService(t *testing.T) {
	for name, tc := range map[string]struct{ old, new_, want string }{
		"build too long": {
			old:  "    project: billet-linux\n",
			new_: "    project: billet-linux\n    build_timeout_minutes: 4000\n",
			want: "36 hours",
		},
		"build too short": {
			old:  "    project: billet-linux\n",
			new_: "    project: billet-linux\n    build_timeout_minutes: 1\n",
			want: "build_timeout_minutes",
		},
		"queued too long": {
			old:  "    project: billet-linux\n",
			new_: "    project: billet-linux\n    queued_timeout_minutes: 900\n",
			want: "8 hours",
		},
	} {
		t.Run(name, func(t *testing.T) {
			got := loadErr(t, codeBuildConfig(t, tc.old, tc.new_))

			if !strings.Contains(got, tc.want) {
				t.Errorf("the error does not mention %q: %s", tc.want, got)
			}
		})
	}
}

// A DECLARED CEILING SURVIVES, which is what makes the inventory window the
// operator's number rather than billet's.
func TestADeclaredCeilingSizesTheInventoryWindow(t *testing.T) {
	body := codeBuildConfig(t, "    project: billet-linux\n",
		"    project: billet-linux\n    build_timeout_minutes: 60\n    queued_timeout_minutes: 30\n")

	cfg, err := Load(writeConfig(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// A TIGHTER CEILING IS A CHEAPER WALK, and the arithmetic is the sum plus one
	// hour of slack. Asserted as a value rather than as "smaller than the default",
	// because the latter passes for a window of one minute — which would read a
	// running build as gone.
	if got, want := cfg.Node.CodeBuild.InventoryWindowMinutes(), 60+30+60; got != want {
		t.Errorf("InventoryWindowMinutes() = %d, want %d", got, want)
	}

	// And the default is the pair of service ceilings plus the same slack, so a
	// deployment that declares nothing still walks a window CodeBuild guarantees is
	// long enough.
	def := &CodeBuildConfig{}
	def.applyDefaults()

	if got, want := def.InventoryWindowMinutes(),
		CodeBuildBuildCeilingMinutes+CodeBuildQueuedCeilingMinutes+60; got != want {
		t.Errorf("the default window is %d, want %d", got, want)
	}
}

// THE SHAPE CATALOGUE IS DECLARED FOR THE ec2 REASON: billet ships no table of
// compute types, and a shape that understates itself starts a build smaller than
// the lease the allocator already escrowed.
func TestACodeBuildNodeNeedsDeclaredComputeTypes(t *testing.T) {
	got := loadErr(t, codeBuildConfig(t, `    compute_types:
      - type: BUILD_GENERAL1_MEDIUM
        vcpu: 4
        memory: 7GiB
        price_usd_per_hour: 0.01
`, ""))

	// THE DIAGNOSTIC MUST NAME THE OPERATOR'S OWN FIELD. Before CheckRemoteShapes
	// took a provider, this said `node.ec2.instance_types` — a key that is not in a
	// codebuild config at all.
	if !strings.Contains(got, "node.codebuild.compute_types") {
		t.Errorf("the error names the wrong config key: %s", got)
	}

	if strings.Contains(got, "node.ec2.instance_types") {
		t.Errorf("the error names the ec2 key at a codebuild operator: %s", got)
	}
}

// AND THE PER-SHAPE DIAGNOSTICS TOO, since they are the ones carrying an index.
func TestCodeBuildShapeProblemsNameTheCodeBuildField(t *testing.T) {
	for name, tc := range map[string]struct{ old, new_, want string }{
		"no vcpu":   {old: "        vcpu: 4\n", new_: "        vcpu: 0\n", want: "vcpu must be more than zero"},
		"no memory": {old: "        memory: 7GiB\n", new_: "        memory: 0\n", want: "memory must be more than zero"},
		"no price": {
			old:  "        price_usd_per_hour: 0.01\n",
			new_: "        price_usd_per_hour: 0\n",
			want: "price_usd_per_hour must be more than zero",
		},
		// The compute-type half of the environment-type refusal: two separate
		// fields, and a Lambda compute type is just as unable to run a job.
		"lambda compute": {
			old:  "      - type: BUILD_GENERAL1_MEDIUM\n",
			new_: "      - type: BUILD_LAMBDA_2GB\n",
			want: "Lambda compute",
		},
	} {
		t.Run(name, func(t *testing.T) {
			got := loadErr(t, codeBuildConfig(t, tc.old, tc.new_))

			if !strings.Contains(got, tc.want) {
				t.Errorf("the error does not say %q: %s", tc.want, got)
			}

			if !strings.Contains(got, "node.codebuild.compute_types[0]") {
				t.Errorf("the error does not name the codebuild field and index: %s", got)
			}
		})
	}
}

// A REGION IS SIGNED INTO EVERY REQUEST AND INTERPOLATED INTO THE DEFAULT
// ENDPOINT, so it decides which host a signed request reaches.
func TestACodeBuildRegionIsValidatedLikeEC2s(t *testing.T) {
	got := loadErr(t, codeBuildConfig(t, "region: us-west-2", "region: x@attacker.example/?"))

	if !strings.Contains(got, "node.codebuild.region") {
		t.Errorf("a hostile region was not refused by name: %s", got)
	}
}

// AND A PLAINTEXT ENDPOINT HANDS AN ON-PATH OBSERVER A REPLAYABLE SIGNED REQUEST
// AND A SESSION TOKEN. Loopback is the exception, which is billet's existing rule
// rather than a new one.
func TestACodeBuildEndpointMustBeHTTPSOrLoopback(t *testing.T) {
	t.Run("plaintext refused", func(t *testing.T) {
		got := loadErr(t, codeBuildConfig(t, "    project: billet-linux\n",
			"    project: billet-linux\n    endpoint: http://codebuild.internal\n"))

		if !strings.Contains(got, "node.codebuild.endpoint must use https") {
			t.Errorf("a plaintext endpoint was not refused: %s", got)
		}

		// NOTHING RENDERS THE ENDPOINT, because every attempt to render it safely
		// was wrong in a new way — see CheckEC2Endpoint. A host in the message is
		// the beginning of a leaked password.
		if strings.Contains(got, "codebuild.internal") {
			t.Errorf("the error rendered the endpoint: %s", got)
		}
	})

	t.Run("loopback allowed", func(t *testing.T) {
		body := codeBuildConfig(t, "    project: billet-linux\n",
			"    project: billet-linux\n    endpoint: http://127.0.0.1:4566\n")

		if _, err := Load(writeConfig(t, body)); err != nil {
			t.Errorf("a loopback endpoint was refused: %v", err)
		}
	})
}

// A PROJECT NAME AWS WILL REFUSE IS REFUSED AT LOAD, rather than on the first
// launch. Pinned to the documented rule rather than to a guess about URL safety —
// the mistake the runner-group validator made in both directions.
func TestACodeBuildProjectNameIsCheckedAgainstAWSsRule(t *testing.T) {
	t.Run("required", func(t *testing.T) {
		got := loadErr(t, codeBuildConfig(t, "    project: billet-linux\n", ""))

		if !strings.Contains(got, "project is required") {
			t.Errorf("an absent project was not refused: %s", got)
		}

		// THE OWNERSHIP REASON IS THE ONE THAT MATTERS, because a shared project is
		// how billet stops somebody else's build.
		if !strings.Contains(got, "cannot be tagged") {
			t.Errorf("the error does not explain that the project is the ownership "+
				"boundary: %s", got)
		}
	})

	for name, value := range map[string]string{
		"space":     "billet linux",
		"dot":       "billet.linux",
		"too short": "b",
	} {
		t.Run(name, func(t *testing.T) {
			got := loadErr(t, codeBuildConfig(t, "project: billet-linux", "project: "+value))

			if !strings.Contains(got, "is not a project name AWS accepts") {
				t.Errorf("%q was not refused: %s", value, got)
			}
		})
	}
}

// NEITHER STORAGE BLOCK BELONGS ON THIS BACKEND, and both are refused rather than
// ignored: a build has nowhere to attach a block device and its compute runs in a
// region that cannot reach a Ceph cluster, so a storage section here reads as a
// working cache right up to the first job that expected one.
func TestStorageBlocksAreRefusedOnACodeBuildNode(t *testing.T) {
	t.Run("ceph", func(t *testing.T) {
		got := loadErr(t, codeBuildConfig(t, codeBuildBlock, codeBuildBlock+`  ceph:
    image_pool: billet-images
    cache_pool: billet-cache
`))

		if !strings.Contains(got, "node.ceph is set but this node's provider is codebuild") {
			t.Errorf("a ceph block on a codebuild node was not refused: %s", got)
		}

		// THE REASON IS PER-BACKEND, not one sentence covering all of them.
		if !strings.Contains(got, "nowhere to attach a block device") {
			t.Errorf("the refusal does not give codebuild's own reason: %s", got)
		}
	})

	t.Run("ebs_s3", func(t *testing.T) {
		got := loadErr(t, codeBuildConfig(t, codeBuildBlock, codeBuildBlock+`  ebs_s3:
    region: us-west-2
    availability_zone: us-west-2a
    bucket: billet-cache
`))

		if !strings.Contains(got, "node.ebs_s3 is set but this node's provider is codebuild") {
			t.Errorf("an ebs_s3 block on a codebuild node was not refused: %s", got)
		}
	})
}

// THE PROVIDER IS REMOTE, so nothing about the machine underneath it is compared
// against what it offers. Reading the two alike makes an honest `max_vcpu: 512`
// look like a typo worth warning about on every boot — and once an operator sees
// that warning daily on the orchestrator, they stop reading it on the EPYC box.
func TestACodeBuildNodeIsNotComparedAgainstItsOwnHardware(t *testing.T) {
	n := &NodeConfig{Provider: ProviderCodeBuild, MaxVCPU: 512, MaxMemory: 2 << 40}

	got := n.Contribution(2, 4<<30)
	if got.VCPU != 512 || got.Memory != 2<<40 {
		t.Errorf("the declared contribution did not survive: %+v", got)
	}

	if len(got.Warnings) != 0 {
		t.Errorf("a remote backend was warned about the hardware it does not use: %v",
			got.Warnings)
	}

	// AND THE OTHER DIRECTION, or this test passes against a Contribution that
	// never warns about anything.
	host := &NodeConfig{Provider: ProviderFirecracker, MaxVCPU: 512}
	if len(host.Contribution(2, 4<<30).Warnings) == 0 {
		t.Error("a host-backed backend was not warned about overcommitting its own hardware")
	}
}

// RunsOnHost IS AN ALLOWLIST, which is why a second remote backend needed no
// change to be treated as remote. Asserted for every provider so a third one
// cannot be added to the allowlist by accident.
func TestOnlyHostBackedProvidersRunWorkOnTheHost(t *testing.T) {
	for _, tc := range []struct {
		kind ProviderKind
		want bool
	}{
		{ProviderFirecracker, true},
		{ProviderTart, true},
		{ProviderDocker, true},
		{ProviderEC2, false},
		{ProviderCodeBuild, false},
		{ProviderKind("something-nobody-implements"), false},
	} {
		if got := tc.kind.RunsOnHost(); got != tc.want {
			t.Errorf("%q.RunsOnHost() = %v, want %v", tc.kind, got, tc.want)
		}
	}
}

// serverOnlyMacOSConfig is a control-plane config with NO node block — the shape a
// separate controller has — whose macOS tier is pinned to a policy that names no
// provider. What the policy's unset macos_vm_limit means then depends on the tiers.
func serverOnlyMacOSConfig(t *testing.T, tierProviders string) string {
	t.Helper()

	body := strings.Replace(validConfig, firecrackerNode, "", 1)
	if strings.Contains(body, "provider: firecracker\n  state_dir") {
		t.Fatal("the node block in validConfig has changed, so this fixture still carries one")
	}

	body = strings.Replace(body, "tiers:\n", "nodes:\n  - name: cb\ntiers:\n", 1)

	return body + `  - label: billet-macos
    ` + tierProviders + `
    guest_os: macos
    node: cb
    vcpu: 8
    memory: 24GiB
    image: aws/codebuild/macos-arm-base:14
    trust: trusted
    runner_group: billet-mac
    workflows:
      - acme/repo/.github/workflows/mac.yml@refs/heads/main
`
}

// A POLICY THAT NAMES NO PROVIDER IS STILL A FLEET WHEN ITS TIERS SAY SO.
//
// A server-only config carries no node block, and nodes[].provider is optional, so
// `{name: cb}` under a tier `provider: codebuild, node: cb` used to resolve to no
// provider at all — and an unset macos_vm_limit then inherited Apple's two, which
// advertised two jobs against a one-Mac fleet. A review round found it. The tiers
// pinned to a node can only be placed on a node running one of their providers, so
// a tier accepting only codebuild has said what the node is.
func TestAServerOnlyPolicyIsReadThroughTheTiersPinnedToIt(t *testing.T) {
	// The tier that reaches macOS through a fleet: the limit is required.
	_, err := Load(writeConfig(t, serverOnlyMacOSConfig(t, "provider: codebuild")))
	if err == nil || !strings.Contains(err.Error(), "set macos_vm_limit for it") {
		t.Errorf("a codebuild macOS tier pinned to a provider-less policy inherited Apple's "+
			"default instead of requiring the fleet's capacity: %v", err)
	}

	// A tier both on owned Apple hardware and on a fleet: the provider is asked for,
	// because the two agreements give the unset limit different meanings.
	_, err = Load(writeConfig(t, serverOnlyMacOSConfig(t, "providers: [tart, codebuild]")))
	if err == nil || !strings.Contains(err.Error(), "set nodes[].provider for it") {
		t.Errorf("a policy pinned by tiers on both a host-backed and a remote backend was "+
			"not asked for its provider: %v", err)
	}

	// AND A TART-ONLY TIER STILL GETS APPLE'S DEFAULT, or the fix has turned a
	// legitimate omission on owned hardware into a refusal.
	cfg, err := Load(writeConfig(t, serverOnlyMacOSConfig(t, "provider: tart")))
	if err != nil {
		t.Fatalf("a tart macOS tier pinned to a provider-less policy was refused: %v", err)
	}

	if got := cfg.MacOSLimitForNode("cb"); got != DefaultMacOSVMLimit {
		t.Errorf("a tart-only node's limit = %d, want Apple's default %d", got, DefaultMacOSVMLimit)
	}

	if got := cfg.MacOSFleetProvider("cb"); got != "" {
		t.Errorf("a tart-only node reports a fleet provider %q", got)
	}

	// The fleet provider is what `billet check` asks, and it answers from the tiers.
	cfg, err = Load(writeConfig(t, strings.Replace(serverOnlyMacOSConfig(t, "provider: codebuild"),
		"  - name: cb\n", "  - name: cb\n    macos_vm_limit: 1\n", 1)))
	if err != nil {
		t.Fatalf("a codebuild macOS tier with the limit declared was refused: %v", err)
	}

	if got := cfg.MacOSFleetProvider("cb"); got != ProviderCodeBuild {
		t.Errorf("MacOSFleetProvider(cb) = %q, want codebuild from the pinned tier", got)
	}
}

// ServesMacOS IS THE macOS ALLOWLIST, ASKED BY NAME. `billet check` carried a second
// copy written as `!= tart`, which reported "codebuild cannot run macOS guests" beside
// a node whose fleet had just run an Xcode job. Asserted for every provider so the
// two lists cannot drift again.
func TestOnlyAppleBackedProvidersServeMacOS(t *testing.T) {
	for _, tc := range []struct {
		kind ProviderKind
		want bool
	}{
		{ProviderTart, true},
		{ProviderCodeBuild, true},
		{ProviderFirecracker, false},
		{ProviderDocker, false},
		{ProviderEC2, false},
		{ProviderKind("something-nobody-implements"), false},
	} {
		if got := tc.kind.ServesMacOS(); got != tc.want {
			t.Errorf("%q.ServesMacOS() = %v, want %v", tc.kind, got, tc.want)
		}
	}

	// AND IT IS THE SAME LIST VALIDATION USES, not a parallel one: a tier's
	// GuestOSProviderErrors must agree with it for every provider.
	for _, kind := range []ProviderKind{ProviderTart, ProviderCodeBuild, ProviderFirecracker,
		ProviderDocker, ProviderEC2} {
		tier := Tier{Label: "l", Provider: kind, GuestOS: GuestMacOS}
		refused := len(tier.GuestOSProviderErrors("tiers[0]")) > 0

		if refused == kind.ServesMacOS() {
			t.Errorf("%q: ServesMacOS() = %v but a macOS tier on it refused = %v",
				kind, kind.ServesMacOS(), refused)
		}
	}
}

// A REMOTE BACKEND'S DIAGNOSTICS NAME ITS OWN CONFIG KEY, which is the whole
// reason CheckRemoteShapes takes a provider instead of hard-coding one.
func TestShapeFieldNamesEachBackendsOwnKey(t *testing.T) {
	for kind, want := range map[ProviderKind]string{
		ProviderEC2:       "node.ec2.instance_types",
		ProviderCodeBuild: "node.codebuild.compute_types",
	} {
		if got := kind.ShapeField(); got != want {
			t.Errorf("%q.ShapeField() = %q, want %q", kind, got, want)
		}
	}
}
