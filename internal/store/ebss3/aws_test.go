package ebss3

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/awssig"
	"github.com/junioryono/billet/internal/config"
)

type staticCredentials struct{}

func (staticCredentials) Credentials(context.Context) (awssig.Credentials, error) {
	return awssig.Credentials{
		AccessKeyID: "AKIDEXAMPLE", SecretAccessKey: "secret", SessionToken: "session",
	}, nil
}

func TestS3StateUsesConditionalSignedEncryptedWrites(t *testing.T) {
	t.Parallel()

	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path != "/state/key.json" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if !strings.Contains(r.Header.Get("Authorization"), "/s3/aws4_request") ||
			r.Header.Get("X-Amz-Security-Token") != "session" {
			t.Errorf("request is not signed with the session credential: %v", r.Header)
		}
		switch requests {
		case 1:
			if r.Method != http.MethodPut || r.Header.Get("If-None-Match") != "*" ||
				r.Header.Get("X-Amz-Server-Side-Encryption") != "AES256" ||
				r.Header.Get("X-Amz-Server-Side-Encryption-Aws-Kms-Key-Id") != "" {
				t.Errorf("create headers = %v", r.Header)
			}
			w.Header().Set("ETag", `"one"`)
			w.WriteHeader(http.StatusOK)
		case 2:
			if r.Method != http.MethodPut || r.Header.Get("If-Match") != `"one"` {
				t.Errorf("replace headers = %v", r.Header)
			}
			w.WriteHeader(http.StatusPreconditionFailed)
		default:
			t.Fatalf("unexpected request %d", requests)
		}
	}))
	defer server.Close()

	api := newS3API(config.EBSS3Config{
		Region: "us-west-2", Bucket: "billet-cache-example", KMSKeyID: "alias/billet",
	}, staticCredentials{}, server.Client(), server.URL, func() time.Time {
		return time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	})
	if etag, err := api.Put(t.Context(), "state/key.json", []byte(`{"one":1}`), ""); err != nil || etag != `"one"` {
		t.Fatalf("initial Put = %q, %v", etag, err)
	}
	if _, err := api.Put(t.Context(), "state/key.json", []byte(`{"two":2}`), `"one"`); !errors.Is(err, errObjectConflict) {
		t.Fatalf("conditional Put error = %v", err)
	}
}

func TestIndeterminateS3WritesAreAmbiguous(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		status int
	}{
		{name: "server error", status: http.StatusInternalServerError},
		{name: "accepted without etag", status: http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
			}))
			defer server.Close()

			api := newS3API(config.EBSS3Config{Region: "us-west-2", Bucket: "billet-cache-example"},
				staticCredentials{}, server.Client(), server.URL, time.Now)
			if _, err := api.Put(t.Context(), "state/key.json", []byte(`{"state":1}`), ""); !errors.Is(err, errObjectAmbiguous) {
				t.Fatalf("Put error = %v, want an ambiguous outcome", err)
			}
		})
	}
}

func TestNewRefusesATypedNilCredentialSource(t *testing.T) {
	t.Parallel()

	var credentials CredentialSourceFunc
	_, err := New(config.EBSS3Config{
		Region: "us-west-2", AvailabilityZone: "us-west-2a", Bucket: "billet-cache-example",
	}, "deployment/site", credentials)
	if err == nil {
		t.Fatal("New accepted a typed-nil credential source")
	}
}

func TestEBSVolumesAndSnapshotsCarryOwnershipAndEncryption(t *testing.T) {
	t.Parallel()

	var actions []url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		values, err := url.ParseQuery(string(body))
		if err != nil {
			t.Fatal(err)
		}
		actions = append(actions, values)
		if !strings.Contains(r.Header.Get("Authorization"), "/ec2/aws4_request") {
			t.Errorf("request is not signed for EC2: %v", r.Header)
		}
		w.Header().Set("Content-Type", "text/xml")
		switch values.Get("Action") {
		case "CreateVolume":
			writeAWSResponse(t, w, `<CreateVolumeResponse><volumeId>vol-123</volumeId><status>creating</status></CreateVolumeResponse>`)
		case "DescribeVolumes":
			writeAWSResponse(t, w, `<DescribeVolumesResponse><volumeSet><item><volumeId>vol-123</volumeId><status>available</status></item></volumeSet></DescribeVolumesResponse>`)
		case "CreateSnapshot":
			writeAWSResponse(t, w, `<CreateSnapshotResponse><snapshotId>snap-123</snapshotId><status>pending</status></CreateSnapshotResponse>`)
		case "DescribeSnapshots":
			writeAWSResponse(t, w, `<DescribeSnapshotsResponse><snapshotSet><item><snapshotId>snap-123</snapshotId><status>completed</status><startTime>2026-08-16T12:00:00.000Z</startTime></item></snapshotSet></DescribeSnapshotsResponse>`)
		default:
			t.Fatalf("unexpected action %q", values.Get("Action"))
		}
	}))
	defer server.Close()

	api := newEBSAPI(config.EBSS3Config{
		Region: "us-west-2", AvailabilityZone: "us-west-2a", KMSKeyID: "alias/billet",
	}, "deployment/site", staticCredentials{}, server.Client(), server.URL,
		func() time.Time { return time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC) },
		func(context.Context, time.Duration) error { return nil })
	volume, err := api.CreateVolume(t.Context(), "", 10<<30, "volume-token")
	if err != nil || volume != "vol-123" {
		t.Fatalf("CreateVolume = %q, %v", volume, err)
	}
	snapshot, err := api.CreateSnapshot(t.Context(), volume,
		time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC), "snapshot-token")
	if err != nil || snapshot != "snap-123" {
		t.Fatalf("CreateSnapshot = %q, %v", snapshot, err)
	}

	createVolume := actions[0]
	if createVolume.Get("AvailabilityZone") != "us-west-2a" ||
		createVolume.Get("Encrypted") != "true" || createVolume.Get("KmsKeyId") != "alias/billet" ||
		createVolume.Get("Size") != "10" || createVolume.Get("VolumeType") != "gp3" ||
		createVolume.Get("TagSpecification.1.Tag.1.Value") != "deployment" ||
		createVolume.Get("TagSpecification.1.Tag.2.Key") != cacheOwnerTag ||
		createVolume.Get("TagSpecification.1.Tag.2.Value") != "deployment/site" ||
		createVolume.Get("ClientToken") != "volume-token" {
		t.Fatalf("CreateVolume params = %v", createVolume)
	}
	createSnapshot := actions[2]
	if createSnapshot.Get("VolumeId") != volume ||
		createSnapshot.Get("TagSpecification.1.ResourceType") != "snapshot" ||
		createSnapshot.Get("TagSpecification.1.Tag.1.Value") != "deployment" ||
		createSnapshot.Get("TagSpecification.1.Tag.2.Key") != cacheOwnerTag ||
		createSnapshot.Get("TagSpecification.1.Tag.2.Value") != "deployment/site" ||
		createSnapshot.Get("ClientToken") != "snapshot-token" {
		t.Fatalf("CreateSnapshot params = %v", createSnapshot)
	}
}

func TestTargetedDeletesRefuseResourcesWithoutThisStoresOwnershipTags(t *testing.T) {
	t.Parallel()

	var deleted bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		values, err := url.ParseQuery(string(body))
		if err != nil {
			t.Fatal(err)
		}
		switch values.Get("Action") {
		case "DescribeVolumes":
			writeAWSResponse(t, w, `<DescribeVolumesResponse><volumeSet><item><volumeId>vol-foreign</volumeId><tagSet><item><key>sh.billet.owner</key><value>other</value></item><item><key>sh.billet.cache-owner</key><value>other/site</value></item></tagSet></item></volumeSet></DescribeVolumesResponse>`)
		case "DescribeSnapshots":
			writeAWSResponse(t, w, `<DescribeSnapshotsResponse><snapshotSet><item><snapshotId>snap-foreign</snapshotId><status>completed</status><tagSet><item><key>sh.billet.owner</key><value>other</value></item><item><key>sh.billet.cache-owner</key><value>other/site</value></item></tagSet></item></snapshotSet></DescribeSnapshotsResponse>`)
		case "DeleteVolume", "DeleteSnapshot":
			deleted = true
		default:
			t.Fatalf("unexpected action %q", values.Get("Action"))
		}
	}))
	defer server.Close()

	api := newEBSAPI(config.EBSS3Config{Region: "us-west-2", AvailabilityZone: "us-west-2a"},
		"deployment/site", staticCredentials{}, server.Client(), server.URL, time.Now,
		func(context.Context, time.Duration) error { return nil })
	if err := api.DeleteVolume(t.Context(), "vol-foreign"); err == nil {
		t.Fatal("DeleteVolume accepted a resource owned by another store")
	}
	if err := api.DeleteSnapshot(t.Context(), "snap-foreign"); err == nil {
		t.Fatal("DeleteSnapshot accepted a resource owned by another store")
	}
	if deleted {
		t.Fatal("a targeted delete reached EC2 for a foreign cache resource")
	}
}

func TestS3ListParsesEveryStateObject(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("list-type") != "2" || r.URL.Query().Get("prefix") != "state/" {
			t.Errorf("query = %v", r.URL.Query())
		}
		response := struct {
			XMLName     xml.Name `xml:"ListBucketResult"`
			Contents    []string `xml:"Contents>Key"`
			IsTruncated bool     `xml:"IsTruncated"`
		}{Contents: []string{"state/a.json", "state/b.json"}}
		if err := xml.NewEncoder(w).Encode(response); err != nil {
			t.Errorf("encode response: %v", err)
		}
	}))
	defer server.Close()

	api := newS3API(config.EBSS3Config{Region: "us-west-2", Bucket: "billet-cache-example"},
		staticCredentials{}, server.Client(), server.URL, time.Now)
	keys, err := api.List(t.Context(), "state/")
	if err != nil || strings.Join(keys, ",") != "state/a.json,state/b.json" {
		t.Fatalf("List = %v, %v", keys, err)
	}
}

func TestEveryAWSPaginationCycleIsRefused(t *testing.T) {
	t.Parallel()

	t.Run("S3", func(t *testing.T) {
		t.Parallel()

		var page int
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			tokens := []string{"A", "B", "A"}
			writeAWSResponse(t, w, `<ListBucketResult><IsTruncated>true</IsTruncated><NextContinuationToken>`+
				tokens[page]+`</NextContinuationToken></ListBucketResult>`)
			page++
		}))
		defer server.Close()

		api := newS3API(config.EBSS3Config{Region: "us-west-2", Bucket: "billet-cache-example"},
			staticCredentials{}, server.Client(), server.URL, time.Now)
		if _, err := api.List(t.Context(), "state/"); err == nil {
			t.Fatal("S3 listing accepted a continuation-token cycle")
		}
	})

	for _, action := range []string{"DescribeSnapshots", "DescribeVolumes"} {
		t.Run(action, func(t *testing.T) {
			t.Parallel()

			var page int
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Fatal(err)
				}
				values, err := url.ParseQuery(string(body))
				if err != nil {
					t.Fatal(err)
				}
				if values.Get("Action") != action {
					t.Fatalf("action = %q, want %q", values.Get("Action"), action)
				}
				tokens := []string{"A", "B", "A"}
				writeAWSResponse(t, w, "<"+action+"Response><nextToken>"+tokens[page]+
					"</nextToken></"+action+"Response>")
				page++
			}))
			defer server.Close()

			api := newEBSAPI(config.EBSS3Config{Region: "us-west-2", AvailabilityZone: "us-west-2a"},
				"deployment/site", staticCredentials{}, server.Client(), server.URL, time.Now,
				func(context.Context, time.Duration) error { return nil })
			var err error
			if action == "DescribeSnapshots" {
				_, err = api.ListSnapshots(t.Context())
			} else {
				_, err = api.ListAvailableVolumes(t.Context())
			}
			if err == nil {
				t.Fatal("EBS listing accepted a pagination-token cycle")
			}
		})
	}
}

type leakyCredentialSource struct {
	Secret string
}

func (l leakyCredentialSource) Credentials(context.Context) (awssig.Credentials, error) {
	return awssig.Credentials{AccessKeyID: "AKID", SecretAccessKey: l.Secret}, nil
}

func TestCredentialHoldingAWSStoresRedactEveryRenderingPath(t *testing.T) {
	t.Parallel()

	const secret = "must-never-render"
	creds := leakyCredentialSource{Secret: secret}
	httpClient := &http.Client{}
	cfg := config.EBSS3Config{
		Region: "us-west-2", AvailabilityZone: "us-west-2a", Bucket: "billet-cache-example",
	}
	s3 := newS3API(cfg, creds, httpClient, "https://s3.example", time.Now)
	ebs := newEBSAPI(cfg, "deployment/site", creds, httpClient, "https://ec2.example",
		time.Now, func(context.Context, time.Duration) error { return nil })
	store := newStore(cfg, "deployment/site", ebs, s3)

	for name, value := range map[string]any{
		"s3 pointer": s3, "s3 value": *s3,
		"ebs pointer": ebs, "ebs value": *ebs,
		"store pointer": store, "store value": *store,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			jsonBody, err := json.Marshal(value)
			if err != nil {
				t.Fatalf("json: %v", err)
			}
			var log bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&log, nil))
			logger.Info("value", "value", value)
			for path, rendered := range map[string]string{
				"fmt":  fmt.Sprintf("%+v %#v %q", value, value, value),
				"json": string(jsonBody),
				"slog": log.String(),
			} {
				if strings.Contains(rendered, secret) {
					t.Errorf("%s exposed the credential source: %s", path, rendered)
				}
			}
		})
	}
}

func writeAWSResponse(t *testing.T, w io.Writer, response string) {
	t.Helper()
	if _, err := io.WriteString(w, response); err != nil {
		t.Errorf("write response: %v", err)
	}
}
