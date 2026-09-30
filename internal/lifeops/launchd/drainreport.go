package launchd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/junioryono/billet/internal/lifeops"
)

// DrainSignal is the drain request: a process that publishes a drain report
// answers it exactly as it answers its first SIGTERM, and a repeat changes
// nothing, so a stop may send it on every poll and after every restart. A
// SIGTERM is not that request, because a node's second SIGTERM ends its wait.
const DrainSignal = syscall.SIGUSR1

// drainRequest is the capability a drain report names, the one spelling a
// reader accepts. A report naming anything else, or written by a release
// before this one, proves nothing, and the stop sends its one recorded SIGTERM.
const drainRequest = "SIGUSR1"

// drainReportSchema is the drain report's schema number.
const drainReportSchema = 1

// drainReport is a running process's own statement that it handles
// DrainSignal, bound to that process by its pid and kernel start time, so a
// report left by an earlier process, or by a release a rollout has since
// replaced on disk, never speaks for the process launchd runs now.
type drainReport struct {
	Schema  int    `json:"schema"`
	Label   string `json:"label"`
	PID     int    `json:"pid"`
	Started string `json:"started"`
	Request string `json:"request"`
	Release string `json:"release"`
}

// drainReportPath is where the process launchd runs for label publishes its
// drain report.
func drainReportPath(dir, label string) string {
	return filepath.Join(dir, ".drain-"+label)
}

// PublishDrainReport says, for the calling process, that it handles
// DrainSignal. Call it only once the handler is installed: before that a Go
// process drops the signal, and a stop that believed the report would wait on
// a drain nobody began.
func PublishDrainReport(label, release string) error {
	return publishDrainReport(defaultLogDir, label, release, os.Getpid(), processStart)
}

func publishDrainReport(
	dir, label, release string, pid int, startOf func(int) (string, error),
) error {
	started, err := startOf(pid)
	if err != nil {
		return fmt.Errorf("launchd: read this process's start to publish its drain report: %w", err)
	}

	body, err := json.Marshal(drainReport{
		Schema:  drainReportSchema,
		Label:   label,
		PID:     pid,
		Started: started,
		Request: drainRequest,
		Release: release,
	})
	if err != nil {
		return fmt.Errorf("launchd: encode the drain report: %w", err)
	}

	return writeOwnFile(drainReportPath(dir, label), ".drain-report-*", string(body))
}

// provesDrainRequest answers whether the process pid is proved to handle
// DrainSignal: nil only when this label's report names that pid, the start the
// kernel reports for it now and the request this build sends. Every other
// answer, a missing or unreadable report included, is an error saying why, and
// the caller takes the recorded SIGTERM path.
func (c *Converger) provesDrainRequest(label string, pid int) error {
	path := drainReportPath(c.logDir, label)

	body, err := readOwnFile(path, "drain report", ownFileLimit+1)
	if err != nil {
		return err
	}

	if len(body) > ownFileLimit {
		return fmt.Errorf("launchd: the drain report %s is larger than a report can be", path)
	}

	if strings.TrimSpace(body) == "" {
		return fmt.Errorf("launchd: %s publishes no drain report at %s", label, path)
	}

	report, err := decodeDrainReport([]byte(body))
	if err != nil {
		return fmt.Errorf("launchd: read the drain report %s: %w", path, err)
	}

	switch {
	case report.Schema != drainReportSchema:
		return fmt.Errorf("launchd: the drain report %s has schema %d, not %d", path, report.Schema,
			drainReportSchema)
	case report.Label != label:
		return fmt.Errorf("launchd: the drain report %s is for %q", path, report.Label)
	case report.Request != drainRequest:
		return fmt.Errorf("launchd: the drain report %s names the request %q", path, report.Request)
	case report.PID != pid:
		return fmt.Errorf("launchd: the drain report %s is pid %d's, not pid %d's", path, report.PID, pid)
	}

	started, err := c.processStart(pid)
	if err != nil {
		return fmt.Errorf("launchd: read when pid %d started: %w", pid, err)
	}

	if started == "" || started != report.Started {
		return fmt.Errorf("launchd: the drain report %s is an earlier process's under pid %d", path, pid)
	}

	return nil
}

// drainReportMembers is the exact member set of a drain report.
var drainReportMembers = []string{"schema", "label", "pid", "started", "request", "release"}

// decodeDrainReport reads exactly one report holding exactly its members,
// each once and spelled exactly, and nothing after it. encoding/json alone
// would accept a member twice, a member in another case and trailing bytes.
func decodeDrainReport(body []byte) (drainReport, error) {
	dec := json.NewDecoder(bytes.NewReader(body))

	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return drainReport{}, fmt.Errorf("not a JSON object (%v)", err)
	}

	seen := map[string]bool{}

	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return drainReport{}, err
		}

		name, _ := tok.(string)
		if !slices.Contains(drainReportMembers, name) || seen[name] {
			return drainReport{}, fmt.Errorf("unexpected or repeated member %q", name)
		}

		seen[name] = true

		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return drainReport{}, err
		}
	}

	if tok, err := dec.Token(); err != nil || tok != json.Delim('}') {
		return drainReport{}, fmt.Errorf("the object is not closed (%v)", err)
	}

	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return drainReport{}, errors.New("something follows the report")
	}

	if len(seen) != len(drainReportMembers) {
		return drainReport{}, fmt.Errorf("%d of its %d members", len(seen), len(drainReportMembers))
	}

	var report drainReport
	if err := json.Unmarshal(body, &report); err != nil {
		return drainReport{}, err
	}

	return report, nil
}

// requestDrain sends pid DrainSignal on every poll until the process exits,
// and reports the process launchd runs for the label afterwards, zero when it
// runs none. requested is false, with nothing sent, when the process is not
// proved to handle the request; the caller then takes the recorded path.
//
// kill(2) ON THE PROVED PID, NOT `launchctl kill`. The proof is about one
// process, and launchd delivers to whatever it runs at that moment, which a
// restart in between makes a process that never said it handles the request.
// So the proof is re-read immediately before every signal, and a process that
// stops proving it after being asked is waited for and not asked again. What
// remains is the instant between that read and the kill, in which the process
// would have to exit and its pid be handed out again; macOS has no pidfd to
// close it, and allocates pids in sequence, so that needs the whole pid space
// to wrap in between.
func (c *Converger) requestDrain(
	ctx context.Context, label string, pid int, watch func(Job), watched map[int]bool,
) (int, bool, lifeops.StopResult, error) {
	sent := false

	for c.alive(pid) {
		switch err := c.provesDrainRequest(label, pid); {
		case err != nil && !sent:
			return 0, false, lifeops.StopResult{}, nil

		case err == nil:
			if err := c.signal(pid); err != nil && c.alive(pid) {
				return 0, true, lifeops.StopResult{
					Gone:  lifeops.Unknown,
					How:   fmt.Sprintf("could not be asked to drain (pid %d)", pid),
					Asked: sent,
				}, fmt.Errorf("launchd: send %s (pid %d) %s: %w", label, pid, drainRequest, err)
			} else if err == nil {
				sent = true
			}
		}

		if !c.sleep(ctx, stopPoll) {
			result := lifeops.StopResult{
				Gone:  lifeops.Unknown,
				How:   "was asked to drain and " + c.stillThere(label, watched, c.anyAlive(watched)),
				Asked: sent,
			}

			if err := ctx.Err(); err != nil {
				return 0, true, result, fmt.Errorf("launchd: stopped waiting for %s to finish its drain "+
					"(the request is idempotent, so a retry asks again): %w", label, err)
			}

			return 0, true, result, fmt.Errorf("launchd: stopped waiting for %s to finish its drain; it %s",
				label, result.How)
		}
	}

	now, loaded, err := c.job(ctx, label)
	if err != nil {
		return 0, true, lifeops.StopResult{
			Gone:  lifeops.Unknown,
			How:   "could not be asked about after its process exited",
			Asked: sent,
		}, err
	}

	watch(now)

	if loaded && now.PIDKnown && now.PID > 0 && now.PID != pid {
		return now.PID, true, lifeops.StopResult{Asked: sent}, nil
	}

	return 0, true, lifeops.StopResult{Asked: sent}, nil
}

// signalDrain sends pid DrainSignal.
func signalDrain(pid int) error {
	if pid <= 0 {
		return errors.New("launchd: no process to ask")
	}

	return syscall.Kill(pid, DrainSignal)
}
