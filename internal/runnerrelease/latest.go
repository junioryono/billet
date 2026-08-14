package runnerrelease

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// LatestURL is the release feed billet reads.
//
// THE PUBLIC RELEASES ENDPOINT, UNAUTHENTICATED, because this asks a question about
// an open-source project rather than about the operator's own account — and needing
// a token to find out whether your fleet is about to stop working would put this
// check behind exactly the credential most likely to be missing on the machine
// running it.
const LatestURL = "https://api.github.com/repos/actions/runner/releases/latest"

// release is the half of GitHub's response this needs.
type release struct {
	TagName     string    `json:"tag_name"`
	PublishedAt time.Time `json:"published_at"`
	Draft       bool      `json:"draft"`
	Prerelease  bool      `json:"prerelease"`
}

// Latest asks GitHub what the current runner release is.
//
// A FAILURE HERE IS NOT A VERDICT. Every caller has to be able to tell "billet is
// out of date" apart from "billet could not find out", because the second is an
// ordinary thing that happens to a machine with no egress and must not be reported
// as a fleet about to stop working.
func Latest(ctx context.Context, client *http.Client) (Status, error) {
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}

	return latestFrom(ctx, client, LatestURL)
}

func latestFrom(ctx context.Context, client *http.Client, url string) (Status, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return Status{}, fmt.Errorf("runnerrelease: build the request: %w", err)
	}

	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-Github-Api-Version", "2022-11-28")

	resp, err := client.Do(req)
	if err != nil {
		return Status{}, fmt.Errorf("runnerrelease: ask github for the latest runner: %w", err)
	}

	defer resp.Body.Close()

	// BOUNDED, because this is a response from somewhere else and the alternative is
	// letting an unexpected one decide how much memory a scheduled check allocates.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Status{}, fmt.Errorf("runnerrelease: read github's answer: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		// RATE LIMITING IS NAMED, because it is the answer an unauthenticated check
		// gets on a busy machine, and "403" alone sends somebody looking for a
		// permissions problem that is not there.
		if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests {
			return Status{}, fmt.Errorf("runnerrelease: github refused the request (%s), which "+
				"unauthenticated is usually its rate limit rather than a permission: %s",
				resp.Status, firstLine(string(body)))
		}

		return Status{}, fmt.Errorf("runnerrelease: github answered %s: %s",
			resp.Status, firstLine(string(body)))
	}

	var rel release
	if err := json.Unmarshal(body, &rel); err != nil {
		return Status{}, fmt.Errorf("runnerrelease: parse github's answer: %w", err)
	}

	version := strings.TrimPrefix(strings.TrimSpace(rel.TagName), "v")
	if version == "" {
		return Status{}, errors.New("runnerrelease: github's answer names no release")
	}

	if rel.PublishedAt.IsZero() {
		return Status{}, fmt.Errorf("runnerrelease: github's answer does not say when %s was "+
			"published, and the deadline is counted from that", version)
	}

	return Status{
		Pinned:    Pinned(),
		Latest:    version,
		Published: rel.PublishedAt,
		Deadline:  rel.PublishedAt.Add(Grace),
	}, nil
}

// firstLine keeps a foreign error message to something that fits in a log line.
func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}

	if len(s) > 200 {
		s = s[:200] + "…"
	}

	return s
}
