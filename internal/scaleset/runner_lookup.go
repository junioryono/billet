package scaleset

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/hashicorp/go-retryablehttp"
)

const maxRunnerLookupResponse = 1 << 20

func newValidatedRetryableHTTPClient() *retryablehttp.Client {
	client := retryablehttp.NewClient()
	// The hook runs after the retry decision and before the response reaches the
	// upstream decoder. Turning an invalid 200 into a non-retryable 422 therefore
	// makes GetRunnerByName return an error instead of accepting false absence or
	// indexing a contradictory value array, while leaving the concrete transport
	// type the upstream credential-refresh path requires untouched.
	client.ResponseLogHook = func(_ retryablehttp.Logger, resp *http.Response) {
		validateRunnerLookupResponse(resp)
	}

	return client
}

func validateRunnerLookupResponse(resp *http.Response) {
	if resp == nil || resp.Request == nil || resp.Request.URL == nil ||
		resp.StatusCode != http.StatusOK || resp.Request.Method != http.MethodGet ||
		!strings.HasSuffix(resp.Request.URL.Path, "/_apis/distributedtask/pools/0/agents") ||
		resp.Request.URL.Query().Get("agentName") == "" {
		return
	}

	var body []byte
	var readErr error
	if resp.Body == nil {
		readErr = fmt.Errorf("response body is missing")
	} else {
		body, readErr = io.ReadAll(io.LimitReader(resp.Body, maxRunnerLookupResponse+1))
		if closeErr := resp.Body.Close(); readErr == nil && closeErr != nil {
			readErr = closeErr
		}
	}
	if readErr == nil && len(body) <= maxRunnerLookupResponse {
		readErr = validateRunnerLookup(body, resp.Request.URL.Query().Get("agentName"))
	}
	if readErr == nil && len(body) > maxRunnerLookupResponse {
		readErr = fmt.Errorf("response exceeds %d bytes", maxRunnerLookupResponse)
	}
	if readErr == nil {
		resp.Body = io.NopCloser(bytes.NewReader(body))
		return
	}

	message, marshalErr := json.Marshal(map[string]string{
		"message": "billet refused malformed Actions runner lookup: " + readErr.Error(),
	})
	if marshalErr != nil {
		message = []byte(`{"message":"billet refused malformed Actions runner lookup"}`)
	}
	resp.StatusCode = http.StatusUnprocessableEntity
	resp.Status = fmt.Sprintf("%d %s", http.StatusUnprocessableEntity,
		http.StatusText(http.StatusUnprocessableEntity))
	resp.Header = make(http.Header)
	resp.Header.Set("Content-Type", "application/json")
	resp.Uncompressed = false
	resp.ContentLength = int64(len(message))
	resp.Body = io.NopCloser(bytes.NewReader(message))
}

func validateRunnerLookup(body []byte, runnerName string) error {
	type runnerRecord struct {
		ID               *int    `json:"id"`
		Name             *string `json:"name"`
		RunnerScaleSetID *int    `json:"runnerScaleSetId"`
	}
	var listed struct {
		Count *int            `json:"count"`
		Value *[]runnerRecord `json:"value"`
	}
	if err := json.Unmarshal(body, &listed); err != nil {
		return fmt.Errorf("decode envelope: %w", err)
	}
	if listed.Count == nil || listed.Value == nil || *listed.Count < 0 ||
		*listed.Count != len(*listed.Value) {
		return fmt.Errorf("incomplete envelope")
	}
	for _, runner := range *listed.Value {
		if runner.ID == nil || runner.Name == nil || runner.RunnerScaleSetID == nil {
			return fmt.Errorf("incomplete runner")
		}
		if *runner.ID <= 0 || *runner.Name != runnerName || *runner.RunnerScaleSetID <= 0 {
			return fmt.Errorf("invalid runner identity")
		}
	}

	return nil
}
