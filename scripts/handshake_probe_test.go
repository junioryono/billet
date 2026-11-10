package scripts_test

import (
	"encoding/csv"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// THE PROBE RECORDS EVERY REQUEST, A FAILED ONE INCLUDED, AND KEEPS GOING. It is
// executed, against a server that answers and an address that refuses, for two
// seconds: both URLs have rows, the refused one with curl's connect failure (7)
// and the answered one with its status and timings.
func TestTheHandshakeProbeRecordsEveryRequestAndKeepsGoing(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl is not installed here")
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)

	// AN ADDRESS NOTHING LISTENS ON, closed before the probe runs.
	var config net.ListenConfig
	listener, err := config.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	refused := "http://" + listener.Addr().String() + "/"
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}

	out := t.TempDir()
	cmd := exec.CommandContext(t.Context(), "bash", "handshake-probe.sh", out, "2", server.URL+"/", refused)
	cmd.Env = append(os.Environ(), "PING_HOST=")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the probe failed: %v\n%s", err, output)
	}

	rows := readCSV(t, filepath.Join(out, "handshakes.csv"))
	if len(rows) < 3 || strings.Join(rows[0], ",") !=
		"utc,url,exit,http_code,remote_ip,dns_s,connect_s,tls_s,first_byte_s,total_s" {
		t.Fatalf("handshakes.csv = %v, want its header and rows", rows)
	}

	answered, failed := 0, 0
	for _, row := range rows[1:] {
		if len(row) != 10 {
			t.Fatalf("a row has %d fields, want 10: %v", len(row), row)
		}
		switch row[1] {
		case server.URL + "/":
			if row[2] != "0" || row[3] != "204" || row[4] != "127.0.0.1" {
				t.Errorf("the answered request recorded %v", row)
			}
			answered++
		case refused:
			if row[2] != "7" {
				t.Errorf("the refused request recorded exit %s, want curl's connect failure (7): %v", row[2], row)
			}
			failed++
		}
	}
	if answered == 0 || failed == 0 || answered != failed {
		t.Fatalf("recorded %d answered and %d refused request(s); want both, one each per tick", answered, failed)
	}

	if pings := readCSV(t, filepath.Join(out, "ping.csv")); len(pings) != 1 {
		t.Fatalf("ping.csv has %d rows with the ping off, want only its header", len(pings))
	}
}

// A DURATION THAT IS NOT A WHOLE NUMBER IS REFUSED, before anything is written.
func TestTheHandshakeProbeRefusesADurationThatIsNotANumber(t *testing.T) {
	t.Parallel()

	out := filepath.Join(t.TempDir(), "never")
	cmd := exec.CommandContext(t.Context(), "bash", "handshake-probe.sh", out, "60; rm -rf /", "http://127.0.0.1:1/")
	output, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "whole number") {
		t.Fatalf("the probe took a duration of %q: %v\n%s", "60; rm -rf /", err, output)
	}
	if _, statErr := os.Stat(out); !os.IsNotExist(statErr) {
		t.Fatalf("the refused probe created its output directory: %v", statErr)
	}
}

func readCSV(t *testing.T, path string) [][]string {
	t.Helper()

	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	rows, err := csv.NewReader(f).ReadAll()
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	return rows
}
