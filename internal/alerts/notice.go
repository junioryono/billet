package alerts

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/junioryono/billet/internal/config"
)

// NoticeVersion is what a webhook's receiver reads the rest of a notice by.
const NoticeVersion = 1

// Notice is what one rule firing says about one job.
//
// THE JSON IS A CONTRACT with whatever a deployment points its webhook at: a field is
// added, never renamed, and NoticeVersion moves if one must change. An empty string
// is not recorded, and a time is RFC 3339 on billet's clock.
type Notice struct {
	Version    int       `json:"billet_alert"`
	Rule       string    `json:"rule"`
	Condition  Condition `json:"condition"`
	Repository string    `json:"repository"`
	Workflow   string    `json:"workflow"`
	Job        string    `json:"job"`
	Branch     string    `json:"branch"`
	Tier       string    `json:"tier"`
	Node       string    `json:"node"`
	Lease      string    `json:"lease"`
	RunID      int64     `json:"run_id"`
	RunURL     string    `json:"run_url"`
	Result     string    `json:"result"`
	Disruption string    `json:"disruption"`
	Rerun      string    `json:"rerun"`
	// Seconds is how long the job had run, or had waited, for a condition about a
	// duration, and zero for any other.
	Seconds int64     `json:"seconds"`
	FiredAt time.Time `json:"fired_at"`
}

// NewNotice is what rule says about job.
func NewNotice(rule *Rule, condition Condition, j *Job, branch string, now time.Time) Notice {
	n := Notice{
		Version: NoticeVersion, Rule: rule.Name, Condition: condition, Repository: j.Repository,
		Workflow: j.Workflow(), Job: j.JobName, Branch: branch, Tier: j.Tier, Node: j.Node, Lease: j.LeaseID,
		RunID: j.RunID, Result: j.Result, Disruption: j.Disruption, Rerun: j.Rerun, FiredAt: now.UTC(),
	}
	if j.Repository != "" && j.RunID > 0 {
		n.RunURL = "https://github.com/" + j.Repository + "/actions/runs/" + strconv.FormatInt(j.RunID, 10)
	}

	switch condition {
	case RanLong:
		end := now
		if !j.ResultAt.IsZero() {
			end = j.ResultAt
		}
		n.Seconds = int64(end.Sub(j.StartedAt).Seconds())
	case WaitedLong:
		n.Seconds = int64(j.StartedAt.Sub(j.QueuedAt).Seconds())
	}

	return n
}

// Render is the body of the request a destination of this kind is sent.
func (n *Notice) Render(kind config.AlertDestinationKind) ([]byte, error) {
	body := any(n)
	if kind == config.AlertSlack {
		body = map[string]string{"text": n.slackText()}
	}

	out, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("alerts: render a notice: %w", err)
	}

	return out, nil
}

// slackText is the notice as a person reads it in a channel.
func (n *Notice) slackText() string {
	var b strings.Builder

	fmt.Fprintf(&b, "billet: %s: job %s %s", slackEscape(n.Rule), slackEscape(n.Job), n.Condition.sentence(n.Seconds))

	if n.Repository != "" {
		fmt.Fprintf(&b, " in %s", slackEscape(n.Repository))
	}
	if n.Workflow != "" {
		fmt.Fprintf(&b, ", workflow %s", slackEscape(n.Workflow))
	}
	if n.Branch != "" {
		fmt.Fprintf(&b, ", branch %s", slackEscape(n.Branch))
	}
	fmt.Fprintf(&b, ", tier %s", slackEscape(n.Tier))
	if n.Disruption != "" {
		fmt.Fprintf(&b, " (billet recorded %s)", slackEscape(n.Disruption))
	}
	if n.RunURL != "" {
		// THE URL IS ESCAPED TOO. A repository's name comes from a message, and only
		// escaping stops it closing a link and opening another.
		fmt.Fprintf(&b, ". Run: %s", slackEscape(n.RunURL))
	}

	return b.String()
}

// sentence is what happened, in the words a channel reads.
func (c Condition) sentence(seconds int64) string {
	long := (time.Duration(seconds) * time.Second).String()

	switch c {
	case Failed:
		return "failed"
	case Cancelled:
		return "was cancelled"
	case Disrupted:
		return "was disrupted by billet's infrastructure"
	case RanLong:
		return "has run for " + long
	case WaitedLong:
		return "waited " + long + " for a runner"
	}

	return string(c)
}

// slackEscape replaces the three characters Slack's documentation says control its
// formatting, so a job name a workflow chose cannot mention `<!channel>` or a person,
// or make a link of itself.
func slackEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}
