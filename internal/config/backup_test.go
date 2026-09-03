package config

import (
	"strings"
	"testing"
)

// withBackup appends a backup block to the known-good config, so the diff
// between a passing and a failing case is the only thing on screen.
func withBackup(body string) string {
	return validConfig + "backup:\n  s3:\n" + body
}

// A backup block loads, normalizes, and defaults its prefix.
func TestABackupBlockLoadsAndDefaultsItsPrefix(t *testing.T) {
	cfg, err := Load(writeConfig(t, withBackup(
		"    bucket: '  billet-backups  '\n    region: '  us-west-2  '\n")))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Backup == nil || cfg.Backup.S3 == nil {
		t.Fatal("the backup block did not load")
	}

	// NORMALIZED AND WRITTEN BACK, because these are addresses rather than
	// identities: a bucket handed to a request with its padding is a bucket that
	// does not exist, and validation reading a trimmed COPY while the consumer
	// reads the raw string is a bug this package has produced three times.
	if cfg.Backup.S3.Bucket != "billet-backups" || cfg.Backup.S3.Region != "us-west-2" {
		t.Errorf("the loaded config kept its padding: %+v", *cfg.Backup.S3)
	}

	if cfg.Backup.S3.Prefix != "billet-backups" {
		t.Errorf("prefix defaulted to %q, want billet-backups", cfg.Backup.S3.Prefix)
	}
}

// AN EMPTY `backup:` IS A MISTAKE, NOT A DEFAULT.
//
// It reads as "billet is looking after this" and does nothing at all, which is
// the one failure mode a backup must not have: the operator finds out on the day
// they need the archive that was never uploaded.
//
// `backup: {}` IS THE FORM THIS CAN CATCH, and the reason is MEASURED rather
// than reasoned about: yaml.v3 does not call UnmarshalYAML for a null value, on
// either a pointer field or a value one — a bare `backup:` with nothing under it
// leaves the field untouched and is indistinguishable from the key being absent.
// Catching that would mean decoding the whole document into a yaml.Node and
// walking it. What covers the bare form instead is the backup command itself,
// which says on EVERY run that the archive is still on the disk it protects.
func TestAnEmptyBackupBlockIsRefused(t *testing.T) {
	_, err := Load(writeConfig(t, validConfig+"backup: {}\n"))
	if err == nil {
		t.Fatal("an empty backup block loaded")
	}

	if !strings.Contains(err.Error(), "names no destination") {
		t.Errorf("the refusal does not say what is wrong: %v", err)
	}
}

// A backup is OF a control plane, so a node-only host declaring one has written
// down somewhere nothing will ever put anything.
func TestANodeOnlyHostCannotDeclareABackup(t *testing.T) {
	nodeOnly := `
node:
  name: epyc-1
  server_addr: 127.0.0.1:7717
  provider: docker
  state_dir: /var/lib/billet/node
backup:
  s3:
    bucket: billet-backups
    region: us-west-2
`

	_, err := Load(writeConfig(t, nodeOnly))
	if err == nil {
		t.Fatal("a node-only config declared a backup destination")
	}

	if !strings.Contains(err.Error(), "no server") {
		t.Errorf("the refusal does not name the reason: %v", err)
	}
}

// Everything the store must refuse, each for a reason worth the diagnostic.
func TestABackupDestinationIsValidated(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want string
	}{{
		name: "a bucket name that breaks TLS",
		body: "    bucket: billet.backups\n    region: us-west-2\n",
		want: "TLS-compatible",
	}, {
		name: "no region, with no endpoint to sign for",
		body: "    bucket: billet-backups\n",
		want: "look like an aws region",
	}, {
		name: "a region that is not one, with no endpoint",
		body: "    bucket: billet-backups\n    region: somewhere\n",
		want: "look like an aws region",
	}, {
		// A SIGNING REGION IS STILL REQUIRED WITH AN ENDPOINT. SigV4 signs one
		// into every request and the store on the far side compares it.
		name: "an endpoint with no region",
		body: "    bucket: billet-backups\n    endpoint: https://rgw.example\n",
		want: "required even with an endpoint",
	}, {
		// The rule the EC2 endpoint follows, and the same one: billet signs each
		// request and sends a session token with it, so plaintext hands an
		// on-path observer a replayable request.
		name: "a plaintext endpoint",
		body: "    bucket: billet-backups\n    region: us-east-1\n" +
			"    endpoint: http://rgw.example\n",
		want: "must use https",
	}, {
		name: "an endpoint carrying a credential",
		body: "    bucket: billet-backups\n    region: us-east-1\n" +
			"    endpoint: https://alice:secret@rgw.example\n",
		want: "username or password",
	}, {
		// A PREFIX IS WHAT AN IAM POLICY IS SCOPED TO, and a wildcard there
		// widens that grant to every sibling prefix — which is every other
		// deployment's archives, each of which is two private keys.
		name: "a wildcard prefix",
		body: "    bucket: billet-backups\n    region: us-west-2\n    prefix: 'billet/*'\n",
		want: "no wildcard",
	}, {
		name: "an absolute prefix",
		body: "    bucket: billet-backups\n    region: us-west-2\n    prefix: /billet\n",
		want: "no leading or trailing slash",
	}, {
		name: "a dot-dot segment in the prefix",
		body: "    bucket: billet-backups\n    region: us-west-2\n    prefix: billet/../other\n",
		want: "dot-dot segment",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, withBackup(tc.body)))
			if err == nil {
				t.Fatal("the config loaded")
			}

			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the refusal was %v, want it to name %q", err, tc.want)
			}
		})
	}
}

// A LOOPBACK ENDPOINT MAY USE http, which is the same exception the EC2 endpoint
// makes and for the same reason: there the trust boundary is the machine.
//
// Both directions matter. A rule that refused this would refuse every test and
// every MinIO on the same host; one that allowed plaintext anywhere would hand
// an on-path observer a replayable signed request.
func TestALoopbackEndpointMayUsePlaintext(t *testing.T) {
	if _, err := Load(writeConfig(t, withBackup(
		"    bucket: billet-backups\n    region: us-east-1\n"+
			"    endpoint: http://127.0.0.1:9000\n"))); err != nil {
		t.Fatalf("a loopback endpoint was refused: %v", err)
	}
}

// A non-AWS region is accepted WITH an endpoint, because the store on the far
// side decides what it accepts and RGW deployments use names AWS never issued.
func TestANonAWSRegionIsAcceptedWithAnEndpoint(t *testing.T) {
	if _, err := Load(writeConfig(t, withBackup(
		"    bucket: billet-backups\n    region: default\n"+
			"    endpoint: https://rgw.example\n"))); err != nil {
		t.Fatalf("a Ceph RGW region was refused: %v", err)
	}
}
