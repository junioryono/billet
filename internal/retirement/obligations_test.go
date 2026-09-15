package retirement

import (
	"slices"
	"strings"
	"testing"
)

func TestRemainingServiceActionsFollowTheExecutableDecision(t *testing.T) {
	if got := RemainingServiceActions(VariantRetainedNode, PhaseIntent, Decision{}); len(got) != 0 {
		t.Fatalf("an unrecorded decision authorized service work: %v", got)
	}
	for _, c := range []struct {
		variant Variant
		phase   Phase
		facts   Facts
		want    []Action
	}{
		{VariantServerOnly, PhaseIntent, Facts{Identity: IdentityConfigured, Config: ConfigInstalled, Stage: StageAbsent, Backup: BackupInactive}, []Action{ActionStop}},
		{VariantRetainedNode, PhaseIntent, Facts{Identity: IdentityConfigured, Config: ConfigInstalled, Stage: StageRecorded, Backup: BackupInactive}, []Action{ActionStop, ActionRestart}},
		{VariantRetainedNode, PhaseStopped, Facts{Identity: IdentityArchive, Config: ConfigInstalled, Stage: StageRecorded, Backup: BackupInactive}, []Action{ActionRestart}},
		{VariantRetainedNode, PhaseConfigRewritten, Facts{Identity: IdentityArchive, Config: ConfigStaged, Stage: StageRecorded, NodeChanged: VerdictFalse}, []Action{ActionRestart}},
		{VariantRetainedNode, PhaseNodeRestarted, Facts{Identity: IdentityArchive, Config: ConfigStaged, Stage: StageRecorded, NodeUnit: NodeInactive}, []Action{ActionRestart}},
		{VariantRetainedNode, PhaseNodeRestarted, Facts{Identity: IdentityArchive, Config: ConfigStaged, Stage: StageRecorded, NodeUnit: NodeReady}, nil},
		{VariantRetainedNode, PhaseDone, Facts{}, nil},
		{VariantRetainedNode, PhaseIntent, Facts{}, nil},
	} {
		t.Run(string(c.variant)+"/"+string(c.phase)+"/"+string(c.facts.NodeUnit), func(t *testing.T) {
			d := Decide(c.variant, c.phase, c.facts)
			if got := RemainingServiceActions(c.variant, c.phase, d); !slices.Equal(got, c.want) {
				t.Fatalf("decision %+v: remaining %v, want %v", d, got, c.want)
			}
		})
	}
}

func TestRetainedInvocationEvidenceCannotNameAnUnprovedResource(t *testing.T) {
	for _, problem := range []string{"healthy", "zero pid", "bad invocation", "no resources", "relative", "duplicate", "absent with inode", "no inode", "relative config", "disposable guest network", "foreign identity directory", "replacement path", "replacement absent", "replacement disposable", "replacement unresolved", "replacement no inode"} {
		t.Run(problem, func(t *testing.T) {
			j := Journal{Variant: VariantRetainedNode, Deployment: "deployment", RetainedInvocation: &RetainedInvocation{
				InvocationID: strings.Repeat("a", 32), MainPID: "42", Deployment: "deployment", Node: "node", Incarnation: "incarnation", Endpoint: "https://example.test:7717", Resources: []RetainedResource{{Path: "/run/billet/registration", Inode: 1}},
			}}
			if strings.HasPrefix(problem, "replacement") {
				j.RetainedInvocation.ConfigPath = "/etc/billet/billet.yaml"
				j.RetainedInvocation.ConfigReplacement = &RetainedResource{Path: "/etc/billet/billet.yaml", ResolvedPath: "/etc/billet/billet.yaml", Inode: 2}
			}
			switch problem {
			case "foreign identity directory":
				j.RetainedInvocation.IdentityDir = "/var/lib/other"
			case "replacement path":
				j.RetainedInvocation.ConfigReplacement.Path = "/etc/other.yaml"
			case "replacement absent":
				j.RetainedInvocation.ConfigReplacement.Absent = true
			case "replacement disposable":
				j.RetainedInvocation.ConfigReplacement.Runtime = true
			case "replacement unresolved":
				j.RetainedInvocation.ConfigReplacement.ResolvedPath = ""
			case "replacement no inode":
				j.RetainedInvocation.ConfigReplacement.Inode = 0
			case "zero pid":
				j.RetainedInvocation.MainPID = "0"
			case "bad invocation":
				j.RetainedInvocation.InvocationID = "unknown"
			case "no resources":
				j.RetainedInvocation.Resources = nil
			case "relative":
				j.RetainedInvocation.Resources[0].Path = "relative"
			case "duplicate":
				j.RetainedInvocation.Resources = append(j.RetainedInvocation.Resources, j.RetainedInvocation.Resources[0])
			case "absent with inode":
				j.RetainedInvocation.Resources[0].Absent = true
			case "no inode":
				j.RetainedInvocation.Resources[0].Inode = 0
			case "relative config":
				j.RetainedInvocation.ConfigPath = "billet.yaml"
			case "disposable guest network":
				j.RetainedInvocation.Resources[0].Runtime = true
				j.RetainedInvocation.Resources[0].GuestNetwork = true
			}
			err := j.retainedInvocationWellFormed()
			if problem == "healthy" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), "journal") {
				t.Fatalf("malformed evidence admitted: %v", err)
			}
		})
	}
}
