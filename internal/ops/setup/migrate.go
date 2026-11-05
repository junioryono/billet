package setup

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/junioryono/billet/internal/cli"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/durablefile"
	"github.com/junioryono/billet/internal/regularfile"
)

// workflowsDir is where GitHub reads a repository's workflows from, the one
// directory it reads them from and without recursing.
const workflowsDir = ".github/workflows"

// Migrate is `billet migrate`: moving what already runs elsewhere onto billet.
func Migrate(ctx context.Context, env cli.Env, args []string) error {
	if len(args) > 0 && args[0] == "workflows" {
		return cmdMigrateWorkflows(ctx, env, args[1:])
	}

	return errors.New("usage: billet migrate workflows [--config PATH] [--path DIR] [--repo OWNER/NAME] " +
		"[--map LABEL=TIER]... [--rightsize FILE] [--public] [--write] [--open-pr]")
}

// labelMaps is --map, in the order given.
type labelMaps map[string]string

func (m labelMaps) String() string { return "" }

func (m labelMaps) Set(s string) error {
	label, tier, ok := strings.Cut(s, "=")
	if !ok || strings.TrimSpace(label) == "" || strings.TrimSpace(tier) == "" {
		return fmt.Errorf("%q is not LABEL=TIER", s)
	}
	m[strings.ToLower(label)] = tier

	return nil
}

// cmdMigrateWorkflows rewrites a checkout's `runs-on` lines to the deployment's
// tiers. It reads the checkout and the configuration and asks GitHub nothing;
// opening the pull request is the operator's own git and gh, because billet's App
// has no contents permission and this command does not ask for one (ADR-011).
func cmdMigrateWorkflows(ctx context.Context, env cli.Env, args []string) error {
	fs := cli.NewFlagSet("billet migrate workflows", env.Stdout)
	cfgPath := cli.AddConfigFlag(fs)
	path := fs.String("path", ".", "the repository checkout to read")
	repo := fs.String("repo", "", "the repository as owner/name (default: read from the checkout's origin remote)")
	maps := labelMaps{}
	fs.Var(maps, "map", "use this tier for this label, as LABEL=TIER with the tier's label; may be repeated")
	rightsize := fs.String("rightsize", "", "also apply the recommendations in this file, which is the output of "+
		"billet jobs rightsize --json")
	public := fs.Bool("public", false, "choose for the larger shape GitHub gives a public repository")
	write := fs.Bool("write", false, "edit the files in place rather than only printing the diff")
	openPR := fs.Bool("open-pr", false, "edit the files on a new branch, commit, push and open a pull request with "+
		"your own git and gh")
	if err := cli.Parse(fs, args); err != nil {
		return err
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}

	repository := *repo
	if repository == "" {
		repository, err = originRepository(ctx, *path)
		if err != nil {
			return err
		}
	} else if _, _, ok := config.SplitRepository(repository); !ok {
		return fmt.Errorf("billet migrate workflows: --repo takes owner/name, not %q", repository)
	}

	chooser, err := newTierChooser(cfg, repository, maps, *public)
	if err != nil {
		return err
	}

	var entries *rightsizeEntries
	if *rightsize != "" {
		if entries, err = readRightsize(*rightsize, repository); err != nil {
			return err
		}
	}

	if *openPR {
		if err := refuseUnlessClean(ctx, *path); err != nil {
			return err
		}
	}

	plan, err := planMigration(*path, repository, chooser, entries)
	if err != nil {
		return err
	}

	for i := range plan.files {
		if _, err := io.WriteString(env.Stdout, unifiedDiff(plan.files[i].name, plan.files[i].before, plan.files[i].after)); err != nil {
			return fmt.Errorf("write the diff: %w", err)
		}
	}
	report := plan.reportText()
	if _, err := io.WriteString(env.Stderr, report); err != nil {
		return fmt.Errorf("write the report: %w", err)
	}

	switch {
	case !plan.changes():
		if *write || *openPR {
			fmt.Fprintln(env.Stderr, "nothing to change")
		}

		return nil
	case *openPR:
		return openMigrationPR(ctx, env, *path, repository, plan, report)
	case *write:
		return plan.install(*path)
	}

	return nil
}

// originRepository reads owner/name from the checkout's origin remote.
//
// THE URL IS NEVER QUOTED IN AN ERROR: a remote can carry a token in its userinfo,
// and this command's output lands in logs and pull requests.
func originRepository(ctx context.Context, path string) (string, error) {
	output, err := runGit(ctx, path, "remote", "get-url", "origin")
	if err != nil {
		return "", errors.New("billet migrate workflows: could not read the checkout's origin remote; " +
			"pass --repo owner/name")
	}

	repository, ok := repositoryOfRemote(strings.TrimSpace(output))
	if !ok {
		return "", errors.New("billet migrate workflows: could not read owner/name from the checkout's origin " +
			"remote; pass --repo owner/name")
	}

	return repository, nil
}

// pushOwner is the owner of the repository origin pushes to, which the pull request's
// head names. ORIGIN'S CONFIGURED PUSH URL, falling back to its URL, and not the one
// git resolves: a fork pushes somewhere its fetch URL does not say, and a rewrite rule
// is how a host is reached, not which repository it is. More than one push URL is a
// push to several, which one head cannot name.
func pushOwner(ctx context.Context, path string) (string, error) {
	urls, err := runGit(ctx, path, "config", "--get-all", "remote.origin.pushurl")
	if err != nil || strings.TrimSpace(urls) == "" {
		urls, err = runGit(ctx, path, "config", "--get-all", "remote.origin.url")
	}
	lines := strings.Fields(urls)
	if err != nil || len(lines) != 1 {
		return "", errors.New("billet migrate workflows: --open-pr pushes to origin, and origin has no one URL to push " +
			"to; nothing was changed")
	}
	repository, ok := repositoryOfRemote(lines[0])
	if !ok {
		return "", errors.New("billet migrate workflows: --open-pr pushes to origin, and its push URL names no " +
			"owner/name; nothing was changed")
	}
	owner, _, _ := config.SplitRepository(repository)

	return owner, nil
}

// repositoryOfRemote reads owner/name from a remote in any form git takes: a URL, or
// the scp form git@host:owner/name.
func repositoryOfRemote(remote string) (string, bool) {
	path := ""
	if u, err := url.Parse(remote); err == nil && u.Scheme != "" && u.Host != "" {
		path = u.Path
	} else if _, rest, ok := strings.Cut(remote, ":"); ok && !strings.Contains(remote, "://") {
		path = rest
	}

	path = strings.TrimSuffix(strings.Trim(path, "/"), ".git")
	if _, _, ok := config.SplitRepository(path); !ok {
		return "", false
	}

	return path, true
}

// tierChooser chooses a tier for a label from a deployment's configuration.
type tierChooser struct {
	repository string
	public     bool
	// tiers are the tiers whose target serves the repository.
	tiers []config.Tier
	// mapped is --map, label to tier.
	mapped map[string]*config.Tier
	// names is every runs-on any of the deployment's tiers answers to.
	names map[string]bool
}

// newTierChooser reads what the configuration offers this repository, and refuses a
// --map naming a tier it does not have or one that cannot serve the repository.
func newTierChooser(cfg *config.Config, repository string, maps labelMaps, public bool) (*tierChooser, error) {
	owner, _, _ := config.SplitRepository(repository)

	serving := map[string]bool{}
	for _, target := range cfg.GitHubTargets() {
		if strings.EqualFold(target.Repository, repository) || (target.Repository == "" && strings.EqualFold(target.Org, owner)) {
			serving[target.Name] = true
		}
	}
	if len(serving) == 0 {
		return nil, fmt.Errorf("billet migrate workflows: no target of this configuration serves %s, so no tier of "+
			"it can run its jobs", strconv.Quote(repository))
	}

	c := &tierChooser{repository: repository, public: public, mapped: map[string]*config.Tier{}, names: map[string]bool{}}
	for i := range cfg.Tiers {
		if t := &cfg.Tiers[i]; serving[config.ShareTarget(*t)] {
			c.tiers = append(c.tiers, *t)
			c.names[strings.ToLower(t.ScaleSetName())] = true
		}
	}

	labels := make([]string, 0, len(maps))
	for label := range maps {
		labels = append(labels, label)
	}
	slices.Sort(labels)
	for _, label := range labels {
		i := slices.IndexFunc(c.tiers, func(t config.Tier) bool { return t.Label == maps[label] })
		if i < 0 {
			return nil, fmt.Errorf("billet migrate workflows: --map %s=%s names a tier this configuration does not "+
				"give %s", label, maps[label], strconv.Quote(repository))
		}
		c.mapped[label] = &c.tiers[i]
	}

	return c, nil
}

// onBillet says whether a label is one of the deployment's runs-on. GitHub compares
// labels without regard to case, and only an ASCII label is folded, so a look-alike
// such as the Kelvin sign is never one of them.
func (c *tierChooser) onBillet(label string) bool {
	return printableASCII(label) && c.names[strings.ToLower(label)]
}

// tierAnswering is the one tier serving the repository that answers to runs-on, nil
// for none or for several, which cannot be told apart.
func (c *tierChooser) tierAnswering(runsOn string) *config.Tier {
	var found *config.Tier
	for i := range c.tiers {
		if sameLabel(c.tiers[i].ScaleSetName(), runsOn) {
			if found != nil {
				return nil
			}
			found = &c.tiers[i]
		}
	}

	return found
}

// sameLabel compares two labels as GitHub does, without regard to case, and folds
// ASCII alone.
func sameLabel(a, b string) bool {
	return printableASCII(a) && printableASCII(b) && strings.EqualFold(a, b)
}

// choose picks the tier for a label: --map's, or the smallest whose platform is the
// label's and whose shape holds what the label gives a job.
func (c *tierChooser) choose(label, workflowFile string) (string, string, string) {
	if t, ok := c.mapped[strings.ToLower(label)]; ok {
		return t.ScaleSetName(), fmt.Sprintf("tier %s by --map", t.Label), ""
	}

	hosted, ok := lookupHostedLabel(label, c.public)
	if !ok {
		return "", "it is neither a label GitHub hosts nor a runs-on of this deployment; a larger runner's or a " +
			"self-hosted runner's name is yours, so pass --map for it", findUnknown
	}
	if hosted.NoMapping != "" {
		return "", hosted.NoMapping, findNoTier
	}

	best, why := c.smallestFor(hosted, workflowFile)
	if best == nil {
		return "", why, findNoTier
	}

	platform := best.Platform()
	shapeOf := "a private repository's"
	if c.public {
		shapeOf = "a public repository's"
	}

	return best.ScaleSetName(), fmt.Sprintf("tier %s (%d vCPU, %s, %s), the smallest holding %s %d vCPU and %d GB on %s",
		best.Label, best.VCPU, cli.HumanBytes(int64(best.Memory)), platform.Arch, shapeOf, hosted.VCPU, hosted.MemoryGB,
		label), ""
}

// smallestFor is the smallest tier for a hosted label, or why there is none, naming
// the closest thing that was passed over: "report, never guess" is only useful to an
// operator who can see what they would have to change.
func (c *tierChooser) smallestFor(hosted hostedLabel, workflowFile string) (*config.Tier, string) {
	var best *config.Tier
	unknownArch, wrongVersion, tooSmall, disallowed := 0, 0, 0, 0
	sameOS := 0

	for i := range c.tiers {
		t := &c.tiers[i]
		if t.GuestOS != hosted.OS {
			continue
		}
		sameOS++
		platform := t.Platform()
		switch {
		case platform.Arch == "":
			unknownArch++

			continue
		case platform.Arch != hosted.Arch:
			continue
		case hosted.Version != "" && platform.OSVersion != hosted.Version:
			wrongVersion++

			continue
		case int64(t.Memory) < hosted.MemoryGB*1_000_000_000 || t.VCPU < hosted.VCPU:
			tooSmall++

			continue
		case !allowsWorkflow(t, c.repository, workflowFile):
			disallowed++

			continue
		}
		if best == nil || cmp.Or(cmp.Compare(t.VCPU, best.VCPU), cmp.Compare(t.Memory, best.Memory),
			cmp.Compare(t.Label, best.Label)) < 0 {
			best = t
		}
	}
	if best != nil {
		return best, ""
	}

	switch {
	case sameOS == 0:
		return nil, fmt.Sprintf("no tier serving this repository has guest OS %s", hosted.OS)
	case unknownArch > 0:
		return nil, fmt.Sprintf("%d tiers of the OS say nothing of their architecture, so name `arch` on them or "+
			"pass --map", unknownArch)
	case disallowed > 0:
		return nil, fmt.Sprintf("%d trusted tiers would hold it and do not list this workflow, so add it to their "+
			"`workflows` or pass --map", disallowed)
	case wrongVersion > 0:
		return nil, fmt.Sprintf("%d tiers of its architecture are on another version of the OS, or do not say which, "+
			"and moving a job to one may break it, so pass --map if it will not", wrongVersion)
	case tooSmall > 0:
		return nil, fmt.Sprintf("the %d tiers of its platform are smaller than %d vCPU and %d GB, so pass --map if it "+
			"fits", tooSmall, hosted.VCPU, hosted.MemoryGB)
	default:
		return nil, fmt.Sprintf("no tier of guest OS %s runs on %s", hosted.OS, hosted.Arch)
	}
}

// allowsWorkflow says a tier will take a job of this workflow. An untrusted tier takes
// any. A trusted one lists the workflows its runner group admits, and a job moved
// there without being listed would queue forever; the ref in the list is GitHub's to
// judge, so only the file is compared.
func allowsWorkflow(t *config.Tier, repository, workflowFile string) bool {
	if t.Trust.Effective() != config.WorkloadTrusted {
		return true
	}

	// THE REPOSITORY IS COMPARED WITHOUT CASE, AS GITHUB NAMES IT, AND THE FILE WITH:
	// ci.yml and CI.yml are two files a repository can hold.
	return slices.ContainsFunc(t.Workflows, func(allowed string) bool {
		path, _, _ := strings.Cut(allowed, "@")
		owner, rest, _ := strings.Cut(path, "/")
		name, file, _ := strings.Cut(rest, "/")

		return strings.EqualFold(owner+"/"+name, repository) && file == workflowFile
	})
}

// rightsizeEntry is one job of `billet jobs rightsize --json`, as far as this command
// reads it.
type rightsizeEntry struct {
	Workflow          string `json:"workflow"`
	Job               string `json:"job"`
	CurrentRunsOn     string `json:"current_runs_on"`
	Recommendation    string `json:"recommendation"`
	Reason            string `json:"reason"`
	Confidence        string `json:"confidence"`
	RecommendedTier   string `json:"recommended_tier"`
	RecommendedRunsOn string `json:"recommended_runs_on"`
	Repository        string `json:"repository"`
	MemorySource      string `json:"memory_source"`
}

// rightsizeEntries is a report's recommendations for one repository that name a tier
// to move to, by workflow file.
type rightsizeEntries struct {
	byFile map[string][]rightsizeEntry
}

// rightsizeSchemaRead is the report's schema this binary reads.
const rightsizeSchemaRead = 1

// maxRightsizeFile bounds the read of a report, which a week of a large fleet's jobs
// stays well under.
const maxRightsizeFile = 256 << 20

// readRightsize reads a report and keeps what it recommends for repository.
func readRightsize(path, repository string) (*rightsizeEntries, error) {
	raw, err := regularfile.ReadFile(path, maxRightsizeFile, regularfile.Options{})
	if err != nil {
		return nil, fmt.Errorf("billet migrate workflows: read --rightsize: %w", err)
	}

	var report struct {
		Schema *int             `json:"schema"`
		Jobs   []rightsizeEntry `json:"jobs"`
	}
	if err := json.Unmarshal(raw, &report); err != nil {
		return nil, fmt.Errorf("billet migrate workflows: --rightsize is not the output of `billet jobs rightsize "+
			"--json`: %w", err)
	}
	if report.Schema == nil || *report.Schema != rightsizeSchemaRead {
		return nil, fmt.Errorf("billet migrate workflows: --rightsize is not schema %d of `billet jobs rightsize "+
			"--json`; read it with the billet that wrote it", rightsizeSchemaRead)
	}

	out := &rightsizeEntries{byFile: map[string][]rightsizeEntry{}}
	for i := range report.Jobs {
		entry := report.Jobs[i]
		if !strings.EqualFold(entry.Repository, repository) || entry.RecommendedRunsOn == "" {
			continue
		}
		file, ok := cutRepository(entry.Workflow, repository)
		if !ok {
			continue
		}
		out.byFile[file] = append(out.byFile[file], entry)
	}

	return out, nil
}

// cutRepository is a workflow's path inside repository, false for a workflow another
// repository owns, as a reusable one can be.
func cutRepository(workflow, repository string) (string, bool) {
	prefix := repository + "/"
	if len(workflow) <= len(prefix) || !strings.EqualFold(workflow[:len(prefix)], prefix) {
		return "", false
	}

	return workflow[len(prefix):], true
}

// applyRightsize moves the jobs a report recommends moving. Only a job the report's
// name proves it means, on the tier the report judged, moves; anything else is said.
func (r *workflowRewrite) applyRightsize(file string, jobs []jobRunsOn, lines lineIndex, chooser *tierChooser,
	entries *rightsizeEntries,
) {
	list := entries.byFile[file]
	for i := range list {
		entry := &list[i]
		say := func(line int, detail string) {
			r.findings = append(r.findings, finding{Kind: findRightsize, File: file, Line: line, Job: entry.Job,
				Text: entry.Job, Detail: detail})
		}

		// A NAME ONLY GITHUB CAN WORK OUT IS NO NAME: it is empty here, and an entry
		// naming no job is refused below before any match is taken.
		var matches []*jobRunsOn
		for j := range jobs {
			if jobs[j].display == entry.Job {
				matches = append(matches, &jobs[j])
			}
		}
		// THE FILE IS NOT TRUSTED TO SAY WHERE A JOB GOES: both tiers are this
		// configuration's own, the move must keep what the job runs on and who may run
		// it, and what is written is the configuration's name for the tier.
		current, recommended := chooser.tierAnswering(entry.CurrentRunsOn), chooser.tierAnswering(entry.RecommendedRunsOn)
		switch {
		case entry.Job == "":
			say(0, "the report names no job")

			continue
		case recommended == nil:
			say(0, fmt.Sprintf("it recommends %s, which is no one tier this configuration gives the repository",
				strconv.Quote(entry.RecommendedRunsOn)))

			continue
		case current == nil:
			say(0, fmt.Sprintf("it was judged on %s, which is no one tier this configuration gives the repository",
				strconv.Quote(entry.CurrentRunsOn)))

			continue
		case !compatibleMove(current, recommended, chooser.repository, file):
			say(0, fmt.Sprintf("tier %s is not %s with another size here: some other setting of it differs, it has "+
				"less disk or shared memory, or it does not allow this workflow", strconv.Quote(recommended.Label),
				strconv.Quote(current.Label)))

			continue
		case len(matches) == 0:
			say(0, "no job here on one of this deployment's tiers is named that: a matrix leg's name and a job whose "+
				"name is an expression come to this")

			continue
		case len(matches) > 1:
			say(matches[0].node.Line, fmt.Sprintf("%d jobs here are named that", len(matches)))

			continue
		case matches[0].matrix:
			say(matches[0].node.Line, "the job has a matrix, whose legs were judged one by one")

			continue
		case !sameLabel(matches[0].node.Value, current.ScaleSetName()):
			say(matches[0].node.Line, fmt.Sprintf("it runs on %s here, and the recommendation was for %s",
				strconv.Quote(matches[0].node.Value), strconv.Quote(entry.CurrentRunsOn)))

			continue
		case r.edited[matches[0].node]:
			say(matches[0].node.Line, "another recommendation in the report already moved it")

			continue
		}

		node, to := matches[0].node, recommended.ScaleSetName()
		placed, reason := lines.place(node, to)
		if reason != "" {
			say(node.Line, reason)

			continue
		}
		r.edits = append(r.edits, placed)
		r.edited[node] = true
		memory := ""
		if entry.MemorySource == "guest" {
			memory = ", its memory as the job's own VM reported it"
		}
		r.findings = append(r.findings, finding{Kind: findRewritten, File: file, Line: node.Line, Job: matches[0].key,
			Text: node.Value, Detail: fmt.Sprintf("to %s, tier %s by the rightsize report%s: %s; confidence %s", to,
				recommended.Label, memory, strconv.Quote(entry.Reason), strconv.Quote(entry.Confidence))})
	}
}

// compatibleMove says moving a job from current to recommended changes its size and
// nothing else, judged by the tiers as they are now: the rule `billet jobs rightsize`
// recommends by (config.Tier.SameFamily), and a tier that allows the workflow.
func compatibleMove(current, recommended *config.Tier, repository, workflowFile string) bool {
	return current.SameFamily(*recommended) && allowsWorkflow(recommended, repository, workflowFile)
}

// migrationFile is one file the plan would change.
type migrationFile struct {
	name          string
	before, after []byte
	mode          os.FileMode
}

// migrationPlan is what the command would do: nothing has been written. dirs is what
// .github and its workflows directory were when they were read.
type migrationPlan struct {
	dirs     []os.FileInfo
	files    []migrationFile
	findings []finding
	entries  *rightsizeEntries
}

func (p *migrationPlan) changes() bool {
	return slices.ContainsFunc(p.files, func(f migrationFile) bool { return !bytes.Equal(f.before, f.after) })
}

// planMigration reads every workflow in the checkout and decides what happens to it.
func planMigration(path, repository string, chooser *tierChooser, entries *rightsizeEntries) (*migrationPlan, error) {
	// NEITHER DIRECTORY MAY BE A LINK: a checkout is somebody's repository, and a
	// .github that points elsewhere would have this read, and --write change, another
	// tree's workflows.
	dirs, err := workflowDirs(path)
	if err != nil {
		return nil, fmt.Errorf("billet migrate workflows: read the workflows of %s: %w", strconv.Quote(repository), err)
	}

	dir := filepath.Join(path, filepath.FromSlash(workflowsDir))
	listing, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("billet migrate workflows: read the workflows of %s: %w", strconv.Quote(repository), err)
	}

	plan := &migrationPlan{entries: entries, dirs: dirs}
	seen := map[string]bool{}
	for _, entry := range listing {
		ext := filepath.Ext(entry.Name())
		if ext != ".yml" && ext != ".yaml" {
			continue
		}
		name := workflowsDir + "/" + entry.Name()
		seen[name] = true

		// NOFOLLOW AND REGULAR ONLY: a workflow is a file in somebody's checkout, and
		// a link out of it or a FIFO in it is no workflow GitHub would read.
		raw, err := regularfile.ReadFile(filepath.Join(dir, entry.Name()), maxWorkflowSize,
			regularfile.Options{NoFollow: true})
		if err != nil {
			plan.findings = append(plan.findings, finding{Kind: findUnreadable, File: name,
				Detail: "it could not be read as a regular file of at most " + cli.HumanBytes(maxWorkflowSize) + ": " +
					strconv.Quote(err.Error())})

			continue
		}
		info, err := entry.Info()
		if err != nil {
			return nil, fmt.Errorf("billet migrate workflows: read %s: %w", strconv.Quote(name), err)
		}

		rewrite := rewriteWorkflow(name, raw, chooser, entries)
		plan.findings = append(plan.findings, rewrite.findings...)
		plan.files = append(plan.files, migrationFile{name: name, before: rewrite.before, after: rewrite.after,
			mode: info.Mode().Perm()})
	}

	if entries != nil {
		files := make([]string, 0, len(entries.byFile))
		for file := range entries.byFile {
			files = append(files, file)
		}
		slices.Sort(files)
		for _, file := range files {
			if seen[file] {
				continue
			}
			for i := range entries.byFile[file] {
				job := entries.byFile[file][i].Job
				plan.findings = append(plan.findings, finding{Kind: findRightsize, File: file, Job: job, Text: job,
					Detail: "the report names this workflow, and the checkout has no such file"})
			}
		}
	}

	return plan, nil
}

// reportOrder is the order the report says things in: what changed, then what is left
// for a person.
var reportOrder = []string{findRewritten, findOnBillet, findExpression, findMatrix, findGroup, findList, findCall,
	findCaller, findNoTier, findUnknown, findCouldNot, findRightsize, findUnreadable}

// reportText is the whole account of every `runs-on`. EVERY STRING A WORKFLOW CHOSE
// IS QUOTED, so a job name cannot forge a line of it.
func (p *migrationPlan) reportText() string {
	var b strings.Builder
	for _, kind := range reportOrder {
		var of []finding
		for i := range p.findings {
			if p.findings[i].Kind == kind {
				of = append(of, p.findings[i])
			}
		}
		if len(of) == 0 {
			continue
		}
		slices.SortStableFunc(of, func(x, y finding) int {
			return cmp.Or(cmp.Compare(x.File, y.File), cmp.Compare(x.Line, y.Line))
		})
		fmt.Fprintf(&b, "%s (%d):\n", kind, len(of))
		for i := range of {
			f := &of[i]
			place := strconv.Quote(f.File)
			if f.Line > 0 {
				place += ":" + strconv.Itoa(f.Line)
			}
			b.WriteString("  " + place)
			if f.Job != "" {
				b.WriteString(" job " + strconv.Quote(f.Job))
			}
			if f.Text != "" && f.Text != f.Job {
				b.WriteString(": " + strconv.Quote(f.Text))
			}
			if f.Detail != "" {
				b.WriteString(": " + f.Detail)
			}
			b.WriteString("\n")
		}
	}
	if b.Len() == 0 {
		return "no workflow here names a runs-on\n"
	}

	return b.String()
}

// install writes the changed files in place, each whole or not at all, and keeps
// its mode.
//
// EVERY FILE IS READ AGAIN FIRST, and nothing is written if one has changed since it
// was planned: writing the plan over an edit made meanwhile would lose the edit.
func (p *migrationPlan) install(path string) error {
	// THE DIRECTORIES TOO: the files read again below are named through them, and a
	// directory replaced since by a link elsewhere would put the writes there.
	dirs, err := workflowDirs(path)
	if err != nil {
		return fmt.Errorf("billet migrate workflows: %w, so nothing was written", err)
	}
	for i := range dirs {
		if !os.SameFile(dirs[i], p.dirs[i]) {
			return errors.New("billet migrate workflows: the workflows directory changed since it was read, so " +
				"nothing was written; run again")
		}
	}
	for i := range p.files {
		f := &p.files[i]
		if bytes.Equal(f.before, f.after) {
			continue
		}
		now, err := regularfile.ReadFile(filepath.Join(path, filepath.FromSlash(f.name)), maxWorkflowSize,
			regularfile.Options{NoFollow: true})
		if err != nil || !bytes.Equal(now, f.before) {
			return fmt.Errorf("billet migrate workflows: %s changed since it was read, so nothing was written; run "+
				"again", strconv.Quote(f.name))
		}
	}
	for i := range p.files {
		f := &p.files[i]
		if bytes.Equal(f.before, f.after) {
			continue
		}
		target := filepath.Join(path, filepath.FromSlash(f.name))
		if _, err := (durablefile.Installer{}).Install(filepath.Dir(target), filepath.Base(target), f.mode,
			func(w io.Writer) error {
				_, err := w.Write(f.after)

				return err
			}); err != nil {
			return fmt.Errorf("billet migrate workflows: write %s: %w", strconv.Quote(f.name), err)
		}
	}

	return nil
}

// workflowDirs is .github and its workflows directory, each refused unless it is a
// directory: a checkout is somebody's repository, and a link there would have this
// read, and --write change, another tree's workflows.
func workflowDirs(path string) ([]os.FileInfo, error) {
	out := make([]os.FileInfo, 0, 2)
	for _, d := range []string{".github", workflowsDir} {
		info, err := os.Lstat(filepath.Join(path, filepath.FromSlash(d)))
		switch {
		case err != nil:
			return nil, err
		case !info.IsDir():
			return nil, fmt.Errorf("%s in the checkout is a link or a file, not a directory, so its workflows are not "+
				"this checkout's", d)
		}
		out = append(out, info)
	}

	return out, nil
}

// refuseUnlessClean refuses a checkout --open-pr could not commit cleanly: anything
// uncommitted would go on the branch beside the migration or be lost to it, and a
// detached HEAD has no branch for a pull request to merge into.
func refuseUnlessClean(ctx context.Context, path string) error {
	status, err := runGit(ctx, path, "status", "--porcelain")
	if err != nil {
		return err
	}
	if strings.TrimSpace(status) != "" {
		return errors.New("billet migrate workflows: --open-pr needs a clean checkout, and this one has uncommitted " +
			"changes; commit or set them aside first")
	}
	if _, err := runGit(ctx, path, "symbolic-ref", "--quiet", "HEAD"); err != nil {
		return errors.New("billet migrate workflows: --open-pr needs a branch to start from, and this checkout's " +
			"HEAD is detached")
	}

	return nil
}

// runGit runs the operator's git in the checkout and returns what it printed.
func runGit(ctx context.Context, path string, args ...string) (string, error) {
	return runTool(ctx, path, "git", args...)
}

// runTool runs one of the operator's own tools in dir, with their environment and argv
// and no shell, and carries what it said into the error, since only it knows what went
// wrong. Only a subcommand is named there: an argument can be a whole pull request.
func runTool(ctx context.Context, dir, name string, args ...string) (string, error) {
	return runToolWith(ctx, dir, nil, name, args...)
}

// runToolWith is runTool with stdin.
func runToolWith(ctx context.Context, dir string, stdin io.Reader, name string, args ...string) (string, error) {
	proc := exec.CommandContext(ctx, name, args...)
	proc.Dir, proc.Stdin = dir, stdin
	var stderr bytes.Buffer
	proc.Stderr = &stderr
	output, err := proc.Output()
	if err != nil {
		return "", fmt.Errorf("%s %s: %w: %s", name, args[0], err, withoutCredentials(strings.TrimSpace(stderr.String())))
	}

	return string(output), nil
}

// urlCredentials is the userinfo of a URL, which a remote can carry a token in.
var urlCredentials = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://)[^/@\s]+@`)

// withoutCredentials is a tool's message with every URL's userinfo cut out, since
// this command's output lands in logs and pull requests.
func withoutCredentials(s string) string { return urlCredentials.ReplaceAllString(s, "$1") }

// openMigrationPR does what the plan says on a new branch and opens the pull request.
// It stops at the first failure and says what was already done, because nothing here
// can be undone for the operator: their branch and their commit are theirs to keep or
// remove.
//
// THE PULL REQUEST IS NAMED WHOLE, so gh guesses none of it: into repository, onto
// the branch the checkout was on, from the branch pushed to origin's owner.
func openMigrationPR(ctx context.Context, env cli.Env, path, repository string, plan *migrationPlan,
	report string,
) error {
	if _, err := exec.LookPath("gh"); err != nil {
		return errors.New("billet migrate workflows: --open-pr uses your own gh, and none is on PATH; nothing was changed")
	}
	base, err := runGit(ctx, path, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		return fmt.Errorf("billet migrate workflows: nothing was changed: %w", err)
	}
	base = strings.TrimSpace(base)
	pushedTo, err := pushOwner(ctx, path)
	if err != nil {
		return err
	}

	branch := "billet/migrate-workflows-" + time.Now().UTC().Format("20060102-150405")
	if _, err := runGit(ctx, path, "checkout", "-q", "-b", branch); err != nil {
		return fmt.Errorf("billet migrate workflows: nothing was changed: %w", err)
	}

	stopped := func(err error, done string) error {
		return fmt.Errorf("billet migrate workflows: stopped on branch %s after %s: %w", branch, done, err)
	}
	if err := plan.install(path); err != nil {
		return stopped(err, "creating it")
	}

	args := []string{"add", "--"}
	for i := range plan.files {
		if !bytes.Equal(plan.files[i].before, plan.files[i].after) {
			args = append(args, filepath.FromSlash(plan.files[i].name))
		}
	}
	if _, err := runGit(ctx, path, args...); err != nil {
		return stopped(err, "writing the files")
	}

	const title = "CI: Run workflows on billet's tiers"
	if _, err := runGit(ctx, path, "commit", "-q", "-m", title); err != nil {
		return stopped(err, "staging the files")
	}
	if _, err := runGit(ctx, path, "push", "-q", "-u", "origin", branch); err != nil {
		return stopped(err, "committing")
	}

	body := "Written by `billet migrate workflows`. It moved each `runs-on` it could and lists what it left alone.\n\n" +
		"```\n" + strings.ReplaceAll(report, "```", "'''") + "```\n"
	// THE BODY ON STDIN, since a report can be longer than one argument may be.
	output, err := runToolWith(ctx, path, strings.NewReader(body), "gh", "pr", "create", "--repo="+repository,
		"--base="+base, "--head="+pushedTo+":"+branch, "--title="+title, "--body-file=-")
	if err != nil {
		return stopped(err, "pushing")
	}
	if _, err := io.WriteString(env.Stdout, output); err != nil {
		return fmt.Errorf("write the pull request's address: %w", err)
	}

	return nil
}

var _ flag.Value = labelMaps{}
