package ebss3

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
)

// attachEC2 is an EC2 for the self-attaching calls: one volume per id, with its tags and
// attachment, and the refusal each action answers, in EC2's own shape.
type attachEC2 struct {
	t       *testing.T
	mu      sync.Mutex
	actions []url.Values
	// owned volumes carry both of the store's ownership tags.
	owned map[string]bool
	// attachedTo is each volume's instance.
	attachedTo map[string]string
	// refuse answers an action with this code and message.
	refuse map[string][2]string
}

func (f *attachEC2) handler(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		f.t.Fatal(err)
	}
	values, err := url.ParseQuery(string(body))
	if err != nil {
		f.t.Fatal(err)
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	f.actions = append(f.actions, values)
	action := values.Get("Action")
	if refusal, ok := f.refuse[action]; ok {
		w.WriteHeader(http.StatusBadRequest)
		if _, err := io.WriteString(w, "<Response><Errors><Error><Code>"+refusal[0]+"</Code><Message>"+refusal[1]+
			"</Message></Error></Errors></Response>"); err != nil {
			f.t.Error(err)
		}

		return
	}

	w.Header().Set("Content-Type", "text/xml")
	switch action {
	case "DescribeVolumes":
		var items strings.Builder
		for id := range f.owned {
			if want := values.Get("VolumeId.1"); want != "" && want != id {
				continue
			}
			if want := values.Get("Filter.1.Value.1"); values.Get("Filter.1.Name") == "attachment.instance-id" &&
				f.attachedTo[id] != want {
				continue
			}
			items.WriteString("<item><volumeId>" + id + "</volumeId><status>in-use</status><tagSet>")
			items.WriteString("<item><key>" + ownerTag + "</key><value>deployment</value></item>")
			if f.owned[id] {
				items.WriteString("<item><key>" + cacheOwnerTag + "</key><value>deployment/site</value></item>")
			}
			items.WriteString("</tagSet><attachmentSet>")
			if instance := f.attachedTo[id]; instance != "" {
				items.WriteString("<item><instanceId>" + instance + "</instanceId><status>attached</status>" +
					"<attachTime>2026-11-09T12:00:00.000Z</attachTime></item>")
			}
			items.WriteString("</attachmentSet></item>")
		}
		if values.Get("VolumeId.1") != "" && items.Len() == 0 {
			w.WriteHeader(http.StatusBadRequest)
			if _, err := io.WriteString(w, "<Response><Errors><Error><Code>InvalidVolume.NotFound</Code></Error></Errors></Response>"); err != nil {
				f.t.Error(err)
			}

			return
		}
		writeAWSResponse(f.t, w, "<DescribeVolumesResponse><volumeSet>"+items.String()+"</volumeSet></DescribeVolumesResponse>")
	case "AttachVolume", "DetachVolume":
		writeAWSResponse(f.t, w, "<Response><status>attaching</status></Response>")
	default:
		f.t.Fatalf("unexpected action %q", action)
	}
}

func (f *attachEC2) sent(action string) []url.Values {
	f.mu.Lock()
	defer f.mu.Unlock()

	var out []url.Values
	for _, values := range f.actions {
		if values.Get("Action") == action {
			out = append(out, values)
		}
	}

	return out
}

func newAttachEC2(t *testing.T) (*attachEC2, *ebsAPI) {
	t.Helper()

	f := &attachEC2{t: t, owned: map[string]bool{}, attachedTo: map[string]string{}, refuse: map[string][2]string{}}
	server := httptest.NewServer(http.HandlerFunc(f.handler))
	t.Cleanup(server.Close)

	api := newEBSAPI(config.EBSS3Config{Region: "us-west-2", AvailabilityZone: "us-west-2a"},
		"deployment/site", staticCredentials{}, server.Client(), server.URL, time.Now,
		func(context.Context, time.Duration) error { return nil })

	return f, api
}

// AN ATTACH NAMES THE VOLUME, THE INSTANCE AND THE NAME, and only of a volume this store
// owns: a volume it may not delete it may not take either.
func TestAnAttachIsOnlyOfAnOwnedVolume(t *testing.T) {
	t.Parallel()

	f, api := newAttachEC2(t)
	f.owned["vol-1"] = true
	f.owned["vol-2"] = false

	if err := api.AttachVolume(t.Context(), "vol-1", testInstance, "/dev/sdf"); err != nil {
		t.Fatal(err)
	}
	sent := f.sent("AttachVolume")
	if len(sent) != 1 || sent[0].Get("VolumeId") != "vol-1" || sent[0].Get("InstanceId") != testInstance ||
		sent[0].Get("Device") != "/dev/sdf" {
		t.Fatalf("AttachVolume was sent %v", sent)
	}

	if err := api.AttachVolume(t.Context(), "vol-2", testInstance, "/dev/sdg"); !errors.Is(err, errNotOwned) {
		t.Fatalf("an attach of another store's volume answered %v", err)
	}
	if len(f.sent("AttachVolume")) != 1 {
		t.Fatal("AttachVolume was sent for a volume this store does not own")
	}
}

// EC2'S "ALREADY IN USE" COMES THROUGH WITH ITS MESSAGE, which is what tells it from an
// invalid name.
func TestAnAttachmentPointInUseIsRecognisedFromEC2sAnswer(t *testing.T) {
	t.Parallel()

	f, api := newAttachEC2(t)
	f.owned["vol-1"] = true
	f.refuse["AttachVolume"] = [2]string{"InvalidParameterValue",
		"Invalid value '/dev/sdf' for unixDevice. Attachment point /dev/sdf is already in use"}

	err := api.AttachVolume(t.Context(), "vol-1", testInstance, "/dev/sdf")
	if !attachmentPointInUse(err) {
		t.Fatalf("EC2's in-use refusal %v was not recognised", err)
	}
	if strings.Contains(err.Error(), "already in use") {
		t.Errorf("EC2's sentence reached the error %q, which names the code alone", err)
	}
}

// A DETACH OF A VOLUME ALREADY OFF, OR GONE, IS NOT AN ERROR; what follows waits on the
// volume's own state. A detach is never forced.
func TestADetachIsNeverForcedAndTakesAnAlreadyDetachedVolume(t *testing.T) {
	t.Parallel()

	f, api := newAttachEC2(t)
	f.owned["vol-1"] = true
	f.attachedTo["vol-1"] = testInstance

	if err := api.DetachVolume(t.Context(), "vol-1", testInstance); err != nil {
		t.Fatal(err)
	}
	sent := f.sent("DetachVolume")
	if len(sent) != 1 || sent[0].Get("VolumeId") != "vol-1" || sent[0].Get("InstanceId") != testInstance {
		t.Fatalf("DetachVolume was sent %v", sent)
	}
	if _, forced := sent[0]["Force"]; forced {
		t.Fatalf("a detach was forced: %v", sent[0])
	}

	f.refuse["DetachVolume"] = [2]string{"IncorrectState", "Volume 'vol-1' is in the 'available' state."}
	if err := api.DetachVolume(t.Context(), "vol-1", testInstance); err != nil {
		t.Fatalf("a detach of a detached volume answered %v", err)
	}

	if err := api.DetachVolume(t.Context(), "vol-9", testInstance); err != nil {
		t.Fatalf("a detach of a volume that is gone answered %v", err)
	}
}

// THE ATTACHMENTS LISTED ARE THIS STORE'S, on this instance, with when each began.
func TestAttachedToListsOnlyThisStoresVolumesHere(t *testing.T) {
	t.Parallel()

	f, api := newAttachEC2(t)
	f.owned["vol-1"] = true
	f.owned["vol-2"] = false
	f.owned["vol-3"] = true
	f.attachedTo["vol-1"] = testInstance
	f.attachedTo["vol-2"] = testInstance
	f.attachedTo["vol-3"] = "i-0fffffffffffffff0"

	attached, err := api.AttachedTo(t.Context(), testInstance)
	if err != nil {
		t.Fatal(err)
	}
	if len(attached) != 1 || attached["vol-1"].IsZero() {
		t.Fatalf("AttachedTo answered %v, want vol-1 alone with its time", attached)
	}

	attachment, found, err := api.Attachment(t.Context(), "vol-1")
	if err != nil || !found || attachment.Instance != testInstance || attachment.State != "attached" {
		t.Fatalf("Attachment answered %+v, %v, %v", attachment, found, err)
	}
	if _, found, err := api.Attachment(t.Context(), "vol-9"); err != nil || found {
		t.Fatalf("a volume EC2 does not have answered found=%v, %v", found, err)
	}
}
