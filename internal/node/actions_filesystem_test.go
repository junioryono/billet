package node

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type linkedActionsVolume struct {
	*fakeActionsVolumeManager
	target string
}

func (v *linkedActionsVolume) MountReadOnly(_ context.Context, _, target string) error {
	if err := os.MkdirAll(target, 0o700); err != nil {
		return err
	}

	return os.Symlink(v.target, filepath.Join(target, "archive"))
}

func TestActionsLookupRefusesAnArchiveLinkOutsideTheVolume(t *testing.T) {
	t.Parallel()

	service, storage, session, volumes := testActionsService(t)
	canary := filepath.Join(t.TempDir(), "canary")
	if err := os.WriteFile(canary, []byte("private fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	storage.current = "published"
	service.actionIO = &linkedActionsVolume{fakeActionsVolumeManager: volumes, target: canary}
	response, err := service.findActionsCache(t.Context(),
		strings.NewReader(`{"key":"key","version":"v1"}`), session, "")
	if response != nil {
		response.Body.Close()
	}
	if err == nil {
		t.Fatal("lookup issued a download URL for a file outside the mounted volume")
	}
	if len(session.actions) != 0 || storage.discarded != 1 {
		t.Fatalf("rejected volume remained in custody: archives=%d discarded=%d",
			len(session.actions), storage.discarded)
	}
}

func TestActionsDownloadRechecksTheArchiveFile(t *testing.T) {
	t.Parallel()

	service, _, session, _ := testActionsService(t)
	archive := &actionsArchive{ID: "fixture", Mode: actionsModeDownload}
	mount := service.actionsMountPath(session, archive)
	if err := os.MkdirAll(mount, 0o700); err != nil {
		t.Fatal(err)
	}
	canary := filepath.Join(t.TempDir(), "canary")
	if err := os.WriteFile(canary, []byte("private fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(canary, service.actionsArchivePath(session, archive)); err != nil {
		t.Fatal(err)
	}
	request := actionsRequestForTest(t, http.MethodGet, "https://"+actionsResultsHost+actionsBlobPrefix+archive.ID, "")
	response, err := service.downloadActionsBlob(request, session, archive)
	if response != nil {
		response.Body.Close()
	}
	if err == nil {
		t.Fatal("download accepted an archive replaced by a link after lookup")
	}
}

func TestActionsDownloadRefusesLinksWithinTheVolume(t *testing.T) {
	t.Parallel()

	service, _, session, _ := testActionsService(t)
	archive := &actionsArchive{ID: "fixture", Mode: actionsModeDownload}
	mount := service.actionsMountPath(session, archive)
	if err := os.MkdirAll(mount, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mount, "other"), []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("other", service.actionsArchivePath(session, archive)); err != nil {
		t.Fatal(err)
	}
	request := actionsRequestForTest(t, http.MethodGet, "https://"+actionsResultsHost+actionsBlobPrefix+archive.ID, "")
	response, err := service.downloadActionsBlob(request, session, archive)
	if response != nil {
		response.Body.Close()
	}
	if err == nil {
		t.Fatal("download accepted a link within the mounted filesystem")
	}
}
