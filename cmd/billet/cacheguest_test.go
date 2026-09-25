package main

import (
	"strings"
	"testing"
)

// THE BEARER GOES ONLY TO THE NODE. Bazel asks the helper about every remote a
// build touches, and a workflow chooses those.
func TestTheCredentialHelperAnswersOnlyTheNode(t *testing.T) {
	t.Parallel()

	const endpoint, token = "http://172.31.0.1:7718", "secret"
	for uri, want := range map[string]bool{
		"http://172.31.0.1:7718/v1/cas/bazel/cas/00":  true,
		"grpc://172.31.0.1:7718":                      true,
		"https://cache.example.com/cas/00":            false,
		"http://172.31.0.1:7719/v1/cas/bazel/cas/00":  false,
		"http://172.31.0.1.example.com:7718/whatever": false,
		"not a uri at all":                            false,
	} {
		answer, err := cacheCredential(strings.NewReader(`{"uri":"`+uri+`"}`), endpoint, token)
		if err != nil {
			t.Fatalf("%s: %v", uri, err)
		}
		got := len(answer.Headers["Authorization"]) == 1 &&
			answer.Headers["Authorization"][0] == "Bearer "+token
		if got != want {
			t.Errorf("%s: bearer given = %v, want %v (%v)", uri, got, want, answer.Headers)
		}
	}

	if answer, _ := cacheCredential(strings.NewReader(`{"uri":"http://172.31.0.1:7718/x"}`), endpoint, ""); len(answer.Headers) != 0 {
		t.Errorf("with no token the helper answered %v", answer.Headers)
	}
}
