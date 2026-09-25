package main

import (
	"strings"
	"testing"
)

// GIT IS ANSWERED ONLY FOR THE NODE, with the session bearer and checkout's own
// header, the last one when several are configured, or "-" when there is none.
func TestTheGitCredentialHelperHandsTheNodeCheckoutsHeader(t *testing.T) {
	t.Parallel()

	const endpoint, token = "http://172.31.0.1:7718", "session"
	node := "protocol=http\nhost=172.31.0.1:7718\nwwwauth[]=Basic realm=\"billet\"\n\n"
	for name, tc := range map[string]struct {
		request string
		headers []string
		want    string
	}{
		"checkout's header": {node, []string{"AUTHORIZATION: basic eDpzZWNyZXQ="},
			"username=session\npassword=basic eDpzZWNyZXQ=\n"},
		"the last of several": {node, []string{"AUTHORIZATION: basic b2xk", "x-other: y", "authorization: basic bmV3"},
			"username=session\npassword=basic bmV3\n"},
		"no header":    {node, []string{""}, "username=session\npassword=-\n"},
		"another host": {"protocol=https\nhost=github.com\n\n", []string{"AUTHORIZATION: basic eDpzZWNyZXQ="}, ""},
		"another port": {"protocol=http\nhost=172.31.0.1:7719\n\n", []string{"AUTHORIZATION: basic eDpzZWNyZXQ="}, ""},
	} {
		got, err := gitCredential(strings.NewReader(tc.request), endpoint, token, tc.headers)
		if err != nil || got != tc.want {
			t.Errorf("%s: answered %q, %v; want %q", name, got, err, tc.want)
		}
	}
	if got, err := gitCredential(strings.NewReader(node), endpoint, "", nil); err != nil || got != "" {
		t.Errorf("with no session the helper answered %q, %v", got, err)
	}
}

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

	answer, err := cacheCredential(strings.NewReader(`{"uri":"http://172.31.0.1:7718/x"}`), endpoint, "")
	if err != nil || len(answer.Headers) != 0 {
		t.Errorf("with no token the helper answered %v, %v", answer.Headers, err)
	}
}
