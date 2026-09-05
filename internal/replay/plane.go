package replay

import (
	"container/heap"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/fakeactions"
)

// owner is the session owner every replayed control plane names itself as.
const owner = "billet-replay"

// eventKind orders the three things the harness does to GitHub's queue.
type eventKind int

const (
	eventArrival eventKind = iota
	eventStarted
	eventCompleted
)

// event is one change to the scripted service at one simulated instant.
type event struct {
	at     time.Time
	kind   eventKind
	seq    int64
	runner *runner
}

// eventHeap orders events by time, then kind, then job, so two runs of one
// trace deliver identical sequences.
type eventHeap []event

func (h *eventHeap) Len() int { return len(*h) }

func (h *eventHeap) Less(i, j int) bool {
	a, b := &(*h)[i], &(*h)[j]

	if !a.at.Equal(b.at) {
		return a.at.Before(b.at)
	}

	if a.kind != b.kind {
		return a.kind < b.kind
	}

	return a.seq < b.seq
}

func (h *eventHeap) Swap(i, j int) { (*h)[i], (*h)[j] = (*h)[j], (*h)[i] }

// Push takes an event; anything else is a programming error in this package and
// is dropped rather than panicked on, since a control plane is running.
func (h *eventHeap) Push(x any) {
	if ev, ok := x.(event); ok {
		*h = append(*h, ev)
	}
}

func (h *eventHeap) Pop() any {
	old := *h
	n := len(old)
	ev := old[n-1]
	*h = old[:n-1]

	return ev
}

// runner is one registration GitHub minted, and the job it was handed.
type runner struct {
	id   int64
	name string
	set  *scaleSet
	job  int64
}

// message is one envelope on a set's queue, with the assignments it carries
// noted so a mint can be paired with the job whose launch caused it, and the
// offers it carries so an acquisition can be checked against what was offered.
type message struct {
	id       int
	envelope map[string]any
	assigned []int64
	offered  []int64
}

// scaleSet is one tier's scale set as the service sees it.
type scaleSet struct {
	id     int
	name   string
	labels []string

	// opened records that the tier's session was answered; parked that its
	// listener is blocked in a poll with nothing to do. Together with an empty
	// queue they are what idle means.
	opened bool
	parked bool
	// lastCap is the capacity the most recent poll advertised, or -1 before any.
	lastCap int

	queue   []message
	nextMsg int
	// wake is closed and replaced when a message is queued, so a parked poll
	// returns at once.
	wake chan struct{}
	// served marks the head as handed to the listener and not yet acknowledged.
	served bool
	// nudged asks the parked poll to return empty once, so the listener runs one
	// iteration of its loop and re-advertises; offered records that the
	// outstanding offers were served since the last nudge, so a listener that
	// could not take them is not offered them in a loop. ONLY A NUDGE CLEARS IT:
	// the harness offers waiting jobs in the fleet's tier order, one tier at a
	// time, and a tier that re-offered itself on its own poll after its own
	// completion would take freed room ahead of that order.
	nudged  bool
	offered bool
	// pollers counts poll handlers inside poll for this set; one listener means
	// at most one.
	pollers int

	// available are offers not yet acquired; assigned are acquisitions not yet
	// completed. Their sizes are the statistics every message carries.
	available map[int64]struct{}
	assigned  map[int64]struct{}
	// inflight are the assignments of the served message that no mint has
	// paired with yet; unpaired are older assignments a launch never minted for.
	inflight []int64
	unpaired []int64
}

// plane is the scripted Actions service the replay drives billet with.
//
// It serves what the trace tells it to and models nothing else: an
// unacknowledged head is redelivered, acknowledging anything but the head is a
// test failure, 202 is the ordinary poll answer. What it adds over the
// end-to-end suite's scripted service is several scale sets, several runners,
// and three rules that make a replay both fast and deterministic: an acquired
// offer is assigned at once, a mint schedules the runner's start and the job's
// completion, and an idle poll is parked rather than answered.
type plane struct {
	*fakeactions.Server

	t     *testing.T
	clock *Clock
	boot  time.Duration
	jobs  map[int64]Arrival
	// order is the tier labels in the order their sessions may open.
	order []string

	mu   sync.Mutex
	cond *sync.Cond

	sets    map[int]*scaleSet
	byName  map[string]*scaleSet
	nextSet int

	runners     map[string]*runner
	runnersByID map[int64]*runner
	nextRunner  int64

	events eventHeap
}

var (
	setRoute        = regexp.MustCompile(`runnerscalesets/(\d+)(?:/(sessions|acquirejobs|generatejitconfig)(?:/([^/]+))?)?/?$`)
	setCollection   = regexp.MustCompile(`runnerscalesets/?$`)
	queueRoute      = regexp.MustCompile(`^/queue/(\d+)(?:/(\d+))?$`)
	agentRoute      = regexp.MustCompile(`agents/(\d+)$`)
	agentCollection = regexp.MustCompile(`agents/?$`)
)

// newPlane starts the scripted service for one trace over one fleet.
func newPlane(t *testing.T, clock *Clock, fleet Fleet, trace Trace) *plane {
	t.Helper()

	p := &plane{
		t:           t,
		clock:       clock,
		boot:        fleet.boot(),
		jobs:        make(map[int64]Arrival, len(trace.Arrivals)),
		sets:        map[int]*scaleSet{},
		byName:      map[string]*scaleSet{},
		nextSet:     100,
		runners:     map[string]*runner{},
		runnersByID: map[int64]*runner{},
		nextRunner:  1000,
	}
	p.cond = sync.NewCond(&p.mu)

	for _, shape := range fleet.Tiers {
		p.order = append(p.order, shape.Label)
	}

	for i := range trace.Arrivals {
		p.jobs[trace.Arrivals[i].Seq] = trace.Arrivals[i]
	}

	p.Server = fakeactions.New(t, p.route)

	// A waiter blocked on the condition variable has no way to notice a context
	// ending, so the clock below wakes every waiter often enough to look.
	stopTick := make(chan struct{})

	go func() {
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()

		for {
			select {
			case <-stopTick:
				return
			case <-ticker.C:
				p.mu.Lock()
				p.cond.Broadcast()
				p.mu.Unlock()
			}
		}
	}()

	t.Cleanup(func() { close(stopTick) })

	return p
}

// route answers the scale-set API. The App handshake is answered upstream by
// the shared fake; everything here is protocol proper.
func (p *plane) route(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	switch {
	case r.Method == http.MethodGet && strings.Contains(path, "/runnergroups"):
		if r.URL.Query().Get("groupName") == RunnerGroup {
			fakeactions.WriteJSON(p.t, w, fakeactions.ListJSON(
				map[string]any{"id": 1, "name": RunnerGroup, "isDefaultGroup": false}))

			return
		}

		fakeactions.WriteJSON(p.t, w, fakeactions.ListJSON())

	case r.Method == http.MethodGet && strings.Contains(path, "/actions/runner-groups/"):
		fakeactions.WriteJSON(p.t, w, map[string]any{
			"restricted_to_workflows": true,
			"selected_workflows":      []string{Workflow},
		})

	case strings.Contains(path, "/pools/0/agents"):
		p.agents(w, r)

	case strings.HasPrefix(path, "/queue/"):
		p.queue(w, r)

	case strings.Contains(path, "/runnerscalesets"):
		p.scaleSets(w, r)

	default:
		p.t.Errorf("replay: unexpected call %s %s", r.Method, path)
		w.WriteHeader(http.StatusNotFound)
	}
}

// scaleSetJSON is what the service reports for one set: exactly the labels
// billet asked for, because reconciliation refuses a set carrying others.
func (p *plane) scaleSetJSON(s *scaleSet) map[string]any {
	return fakeactions.ScaleSetJSON(s.id, s.name, RunnerGroup, s.labels...)
}

// scaleSets serves the collection, one set, its sessions, its acquisitions and
// its mints.
func (p *plane) scaleSets(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	if setCollection.MatchString(path) {
		switch r.Method {
		case http.MethodGet:
			p.listSets(w, r.URL.Query().Get("name"))
		case http.MethodPost:
			p.createSet(w, r)
		default:
			p.t.Errorf("replay: unexpected call %s %s", r.Method, path)
			w.WriteHeader(http.StatusMethodNotAllowed)
		}

		return
	}

	m := setRoute.FindStringSubmatch(path)
	if m == nil {
		p.t.Errorf("replay: unexpected scale-set call %s %s", r.Method, path)
		w.WriteHeader(http.StatusNotFound)

		return
	}

	id, err := strconv.Atoi(m[1])
	if err != nil {
		p.t.Errorf("replay: call %s %s names a scale set that is not a number: %v", r.Method, path, err)
		w.WriteHeader(http.StatusNotFound)

		return
	}

	p.mu.Lock()
	set := p.sets[id]
	p.mu.Unlock()

	if set == nil {
		p.t.Errorf("replay: call %s %s names scale set %d, which does not exist", r.Method, path, id)
		w.WriteHeader(http.StatusNotFound)

		return
	}

	switch {
	case m[2] == "" && r.Method == http.MethodGet:
		fakeactions.WriteJSON(p.t, w, p.scaleSetJSON(set))
	case m[2] == "sessions" && m[3] == "" && r.Method == http.MethodPost:
		p.openSession(w, set)
	case m[2] == "sessions" && m[3] != "" && r.Method == http.MethodDelete:
		w.WriteHeader(http.StatusNoContent)
	case m[2] == "sessions" && m[3] != "" && r.Method == http.MethodPatch:
		fakeactions.WriteJSON(p.t, w, p.sessionJSON(set))
	case m[2] == "acquirejobs" && r.Method == http.MethodPost:
		p.acquire(w, r, set)
	case m[2] == "generatejitconfig" && r.Method == http.MethodPost:
		p.mint(w, r, set)
	default:
		p.t.Errorf("replay: unexpected scale-set call %s %s", r.Method, path)
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (p *plane) listSets(w http.ResponseWriter, name string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	var values []map[string]any

	for _, label := range p.order {
		s := p.byName[label]
		if s != nil && (name == "" || s.name == name) {
			values = append(values, p.scaleSetJSON(s))
		}
	}

	fakeactions.WriteJSON(p.t, w, fakeactions.ListJSON(values...))
}

func (p *plane) createSet(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name   string `json:"name"`
		Labels []struct {
			Name string `json:"name"`
		} `json:"labels"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		p.t.Errorf("replay: decode scale-set create: %v", err)
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if _, exists := p.byName[req.Name]; exists {
		p.t.Errorf("replay: scale set %q was created twice", req.Name)
	}

	s := &scaleSet{
		id:        p.nextSet,
		name:      req.Name,
		lastCap:   -1,
		wake:      make(chan struct{}),
		available: map[int64]struct{}{},
		assigned:  map[int64]struct{}{},
	}
	p.nextSet++

	for _, l := range req.Labels {
		s.labels = append(s.labels, l.Name)
	}

	p.sets[s.id] = s
	p.byName[s.name] = s
	p.cond.Broadcast()

	fakeactions.WriteJSON(p.t, w, p.scaleSetJSON(s))
}

func (p *plane) sessionJSON(s *scaleSet) map[string]any {
	return fakeactions.SessionJSON(fmt.Sprintf("3f8a1c22-0000-4000-8000-%012d", s.id), owner,
		p.scaleSetJSON(s), p.URL+"/queue/"+strconv.Itoa(s.id), "queue-token")
}

// openSession answers a tier's session. The ORDER sessions open in is decided
// before the request is made, by awaitTurn, not here: the scale-set client
// serialises its calls under one mutex, so a session request held open in this
// handler would hold every other tier's request behind it.
func (p *plane) openSession(w http.ResponseWriter, s *scaleSet) {
	p.mu.Lock()
	s.opened = true
	p.cond.Broadcast()
	p.mu.Unlock()

	fakeactions.WriteJSON(p.t, w, p.sessionJSON(s))
}

// awaitTurn returns once every tier before this set in the fleet's order is
// parked, so its session may be asked for.
//
// SESSIONS OPEN IN TIER ORDER, because the first thing a listener does with one
// is escrow its discovery slot, and escrow is where placement happens: opened
// together, which tier's slot lands on which host depends on which goroutine
// ran first, and the first job of every tier inherits that accident.
func (p *plane) awaitTurn(ctx context.Context, setID int) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	s := p.sets[setID]
	if s == nil {
		return fmt.Errorf("replay: a session was asked for scale set %d, which does not exist", setID)
	}

	for !p.predecessorsParkedLocked(s) {
		if err := ctx.Err(); err != nil {
			return err
		}

		p.cond.Wait()
	}

	return nil
}

func (p *plane) predecessorsParkedLocked(s *scaleSet) bool {
	for _, label := range p.order {
		if label == s.name {
			return true
		}

		prev := p.byName[label]
		if prev == nil || !prev.parked {
			return false
		}
	}

	return true
}

// acquire grants every id billet bids for out of the offer it is holding, and
// assigns it at once.
//
// ASSIGNED ON ACQUISITION is the minimum of GitHub's contract, and it is what
// every end-to-end scenario does by hand: an acquired offer becomes an
// assignment, carried on the next message with statistics that count it.
//
// ONLY WHAT THE SERVED OFFER CARRIED. A bid for a job that is waiting but was
// not in the message the listener is holding is a bid for something GitHub
// did not offer, which the worst defect in this project's history was made of;
// it is reported and not granted.
func (p *plane) acquire(w http.ResponseWriter, r *http.Request, s *scaleSet) {
	var ids []int64
	if err := json.NewDecoder(r.Body).Decode(&ids); err != nil {
		p.t.Errorf("replay: acquire body is not a list of ids: %v", err)
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	inOffer := map[int64]bool{}

	if len(s.queue) > 0 && s.served {
		for _, id := range s.queue[0].offered {
			inOffer[id] = true
		}
	}

	var (
		granted []int64
		jobs    []map[string]any
	)

	for _, id := range ids {
		if !inOffer[id] {
			p.t.Errorf("replay: %s bid for request %d, which the offer it holds does not carry", s.name, id)

			continue
		}

		if _, offered := s.available[id]; !offered {
			continue
		}

		delete(s.available, id)
		s.assigned[id] = struct{}{}
		granted = append(granted, id)
		jobs = append(jobs, p.jobJSON("JobAssigned", p.jobs[id], fakeactions.JobFields{}))
	}

	if len(granted) > 0 {
		p.pushLocked(s, granted, nil, jobs...)
	}

	if granted == nil {
		granted = []int64{}
	}

	fakeactions.WriteJSON(p.t, w, map[string]any{"count": len(granted), "value": granted})
}

// mint registers a runner and pairs it with the job whose launch caused it.
//
// The mint arrives while the assignment's message is served and unacknowledged,
// so the job is the head of that message's assignments. A launch that never
// minted leaves its job in `unpaired` for the next runner on the set, which is
// GitHub's pool semantics: a registered runner takes whatever is waiting.
func (p *plane) mint(w http.ResponseWriter, r *http.Request, s *scaleSet) {
	var req struct {
		Name string `json:"name"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		p.t.Errorf("replay: decode jit request: %v", err)
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if _, exists := p.runners[req.Name]; exists {
		p.t.Errorf("replay: runner %q was minted twice", req.Name)
	}

	rn := &runner{id: p.nextRunner, name: req.Name, set: s}
	p.nextRunner++
	p.runners[rn.name] = rn
	p.runnersByID[rn.id] = rn

	switch {
	case len(s.inflight) > 0:
		rn.job, s.inflight = s.inflight[0], s.inflight[1:]
	case len(s.unpaired) > 0:
		rn.job, s.unpaired = s.unpaired[0], s.unpaired[1:]
	default:
		p.t.Logf("replay: runner %q was minted with no assigned job on %s to take", rn.name, s.name)
	}

	if rn.job != 0 {
		started := p.clock.Now().Add(p.boot)
		heap.Push(&p.events, event{at: started, kind: eventStarted, seq: rn.job, runner: rn})
		heap.Push(&p.events, event{
			at: started.Add(time.Duration(p.jobs[rn.job].Duration)), kind: eventCompleted,
			seq: rn.job, runner: rn,
		})
	}

	// The registration names the scale set it was minted against, not the
	// shared helper's placeholder, so a reader of the response cannot be handed
	// an identity that contradicts the request.
	resp := fakeactions.JitConfigJSON(int(rn.id), rn.name, "encoded-jit-config-"+rn.name)
	if runner, ok := resp["runner"].(map[string]any); ok {
		runner["runnerScaleSetId"] = s.id
	}

	fakeactions.WriteJSON(p.t, w, resp)
}

// agents answers the runner lookup by name and the removal by id.
func (p *plane) agents(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	switch {
	case r.Method == http.MethodGet && agentCollection.MatchString(path):
		name := r.URL.Query().Get("agentName")

		p.mu.Lock()
		rn := p.runners[name]
		p.mu.Unlock()

		if rn == nil {
			fakeactions.WriteJSON(p.t, w, map[string]any{"count": 0, "value": []any{}})

			return
		}

		fakeactions.WriteJSON(p.t, w, map[string]any{"count": 1, "value": []map[string]any{
			{"id": rn.id, "name": rn.name, "runnerScaleSetId": rn.set.id},
		}})

	case r.Method == http.MethodDelete && agentRoute.MatchString(path):
		id, err := strconv.ParseInt(agentRoute.FindStringSubmatch(path)[1], 10, 64)
		if err != nil {
			p.t.Errorf("replay: runner removal %s names an id that is not a number: %v", path, err)
			w.WriteHeader(http.StatusNotFound)

			return
		}

		p.mu.Lock()
		if rn := p.runnersByID[id]; rn != nil {
			delete(p.runnersByID, id)
			delete(p.runners, rn.name)
		}
		p.mu.Unlock()

		w.WriteHeader(http.StatusNoContent)

	default:
		p.t.Errorf("replay: unexpected runner call %s %s", r.Method, path)
		w.WriteHeader(http.StatusNotFound)
	}
}

// queue serves a set's long poll and its acknowledgements.
func (p *plane) queue(w http.ResponseWriter, r *http.Request) {
	m := queueRoute.FindStringSubmatch(r.URL.Path)
	if m == nil {
		p.t.Errorf("replay: unexpected queue call %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)

		return
	}

	id, err := strconv.Atoi(m[1])
	if err != nil {
		p.t.Errorf("replay: queue call %s names a scale set that is not a number: %v", r.URL.Path, err)
		w.WriteHeader(http.StatusNotFound)

		return
	}

	p.mu.Lock()
	set := p.sets[id]
	p.mu.Unlock()

	if set == nil {
		p.t.Errorf("replay: queue call for scale set %d, which does not exist", id)
		w.WriteHeader(http.StatusNotFound)

		return
	}

	switch {
	case r.Method == http.MethodGet && m[2] == "":
		p.poll(w, r, set)
	case r.Method == http.MethodDelete && m[2] != "":
		msgID, err := strconv.Atoi(m[2])
		if err != nil {
			p.t.Errorf("replay: acknowledgement %s names a message that is not a number: %v", r.URL.Path, err)
			w.WriteHeader(http.StatusNotFound)

			return
		}

		p.ack(w, set, msgID)
	default:
		p.t.Errorf("replay: unexpected queue call %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// poll serves the head of the queue, lands a changed advertisement, offers what
// is waiting, or parks.
//
// THE HEAD IS NOT REMOVED HERE; it goes when its id is acknowledged, so a
// missing acknowledgement redelivers rather than passes.
//
// A CHANGED CAPACITY IS ANSWERED AT ONCE. The listener hands surplus escrow back
// only after a poll that carried the smaller number has RETURNED, so a poll
// advertising something new is answered 202 immediately and the next one, with
// the same number, is what parks. Idle therefore means the escrow has settled
// to the listener's target, not merely that the listener is waiting.
//
// AN OFFER NOT ACQUIRED IS OFFERED AGAIN once the tier advertises capacity, which
// is what a queued job is: the listener acknowledges an offer it had no escrow
// for, and GitHub keeps the job for a scale set that later has room. Offered
// once per nudge, because a listener can advertise running work it cannot take
// an offer against, and offering into that forever would be a loop with no
// exit; and only on a nudge, because the harness offers waiting jobs tier by
// tier in the fleet's order after every event, and a tier that re-offered
// itself on the poll after its own completion would contest the freed room
// ahead of that order. Whether GitHub re-offers on this cadence is not
// claimed; that a job it holds is offered to a set with capacity is the whole
// of what is modelled.
//
// A PARKED POLL RETURNS ONLY WHEN THE HARNESS SAYS SO, by queueing a message or
// nudging the set. A wall-clock timeout here would let a listener run an escrow
// at a moment the harness did not choose, which is a placement decided by a
// timer rather than by the trace.
func (p *plane) poll(w http.ResponseWriter, r *http.Request, s *scaleSet) {
	// The client sends X-ScaleSetMaxCapacity; Go canonicalises the key on receipt,
	// so it is read under its canonical spelling.
	capacity, err := strconv.Atoi(r.Header.Get("X-Scalesetmaxcapacity"))
	if err != nil {
		p.t.Errorf("replay: a poll on %s carried no capacity header: %v", s.name, err)
	}

	p.mu.Lock()

	s.pollers++
	if s.pollers > 1 {
		p.t.Errorf("replay: %d polls are inside poll for %s at once", s.pollers, s.name)
	}

	defer func() {
		p.mu.Lock()
		s.pollers--
		p.mu.Unlock()
	}()

	for {
		if len(s.queue) > 0 {
			head := s.queue[0]

			if !s.served {
				s.served = true
				s.inflight = append(s.inflight, head.assigned...)
			}

			p.mu.Unlock()
			fakeactions.WriteJSON(p.t, w, head.envelope)

			return
		}

		if capacity != s.lastCap {
			s.lastCap = capacity
			p.cond.Broadcast()
			p.mu.Unlock()
			w.WriteHeader(http.StatusAccepted)

			return
		}

		if capacity > 0 && len(s.available) > 0 && !s.offered {
			s.offered = true
			p.reofferLocked(s, capacity)

			continue
		}

		if s.nudged {
			// THE NUDGE'S OFFER IS SPENT WITH IT. A nudge consumed while the tier
			// advertised nothing must not leave an offer pending for a later poll
			// of the tier's own, or a completion on that tier would re-offer its
			// backlog ahead of the fleet's order after all.
			s.nudged = false
			s.offered = true
			p.cond.Broadcast()
			p.mu.Unlock()
			w.WriteHeader(http.StatusAccepted)

			return
		}

		s.parked = true
		p.cond.Broadcast()
		wake := s.wake
		p.mu.Unlock()

		cancelled := false

		select {
		case <-wake:
		case <-r.Context().Done():
			cancelled = true
		}

		p.mu.Lock()
		s.parked = false
		p.cond.Broadcast()

		if cancelled {
			p.mu.Unlock()
			w.WriteHeader(http.StatusAccepted)

			return
		}
	}
}

// reofferLocked queues the oldest outstanding offers, up to what the tier
// advertises, as one JobAvailable message.
func (p *plane) reofferLocked(s *scaleSet, capacity int) {
	ids := make([]int64, 0, len(s.available))
	for id := range s.available {
		ids = append(ids, id)
	}

	slices.Sort(ids)

	if len(ids) > capacity {
		ids = ids[:capacity]
	}

	jobs := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		jobs = append(jobs, p.jobJSON("JobAvailable", p.jobs[id], fakeactions.JobFields{}))
	}

	p.pushLocked(s, nil, ids, jobs...)
}

// nudge makes a parked listener run one iteration of its loop: it re-escrows
// against whatever room the fleet now has and re-advertises, and the poll that
// follows offers it whatever is waiting.
//
// HOW FREED ROOM IS CONTESTED. When a job finishes, every tier with jobs waiting
// would in production notice the room on its next poll, in an order the clocks
// decide. The harness nudges the starved tiers one at a time in the fleet's
// order after every event, so the contest has one outcome per trace.
func (p *plane) nudge(label string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	s := p.byName[label]
	if s == nil {
		return
	}

	s.nudged = true
	s.offered = false

	p.wakeLocked(s)
}

// starved reports whether a tier has offers it has not been able to take, or
// advertises nothing at all and so could not take one.
func (p *plane) starved(label string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	s := p.byName[label]

	return s != nil && (len(s.available) > 0 || s.lastCap == 0)
}

// standing is what a tier advertises and holds, so a caller can tell whether
// a poll it caused changed anything.
type standing struct {
	capacity  int
	available int
	assigned  int
}

// standingOf reports a tier's standing.
func (p *plane) standingOf(label string) standing {
	p.mu.Lock()
	defer p.mu.Unlock()

	s := p.byName[label]
	if s == nil {
		return standing{}
	}

	return standing{capacity: s.lastCap, available: len(s.available), assigned: len(s.assigned)}
}

// wakeLocked returns a parked poll.
//
// THE WAKER UNPARKS THE SET, not the woken handler. The handler clears the flag
// only once its goroutine runs, and a settle that begins in between would read
// the set as still parked and let the next event overlap the work this wake
// starts, which is exactly the goroutine ordering the harness exists to keep
// out of placement.
func (p *plane) wakeLocked(s *scaleSet) {
	s.parked = false

	close(s.wake)
	s.wake = make(chan struct{})
	p.cond.Broadcast()
}

// ack drops the head, and only the head. Acking an id that is not the head
// means the cursor has run ahead of the work, which is the failure that loses a
// job silently.
func (p *plane) ack(w http.ResponseWriter, s *scaleSet, msgID int) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if len(s.queue) == 0 || s.queue[0].id != msgID {
		p.t.Errorf("replay: %s acknowledged message %d, which is not the head of its queue", s.name, msgID)
		w.WriteHeader(http.StatusNoContent)

		return
	}

	// An assignment the message carried and nothing minted for waits for the
	// next runner on the set.
	s.unpaired = append(s.unpaired, s.inflight...)
	s.inflight = nil
	s.queue = s.queue[1:]
	s.served = false
	p.cond.Broadcast()

	w.WriteHeader(http.StatusNoContent)
}

// pushLocked queues one envelope on a set and wakes its parked poll.
func (p *plane) pushLocked(s *scaleSet, assigned, offered []int64, jobs ...map[string]any) {
	s.nextMsg++

	env := fakeactions.MessageJSON(p.t, s.nextMsg,
		fakeactions.StatisticsJSON(len(s.available), len(s.assigned)), jobs...)

	s.queue = append(s.queue, message{id: s.nextMsg, envelope: env, assigned: assigned, offered: offered})

	p.wakeLocked(s)
}

// jobJSON is one job entry with the trace's own facts on it.
func (p *plane) jobJSON(kind string, a Arrival, fields fakeactions.JobFields) map[string]any {
	fields.Owner = a.Owner
	fields.Repository = a.Repository
	fields.WorkflowRef = a.WorkflowRef
	fields.RunID = a.RunID

	return fields.Apply(fakeactions.JobJSON(kind, a.Seq, "push", a.Tier))
}

// schedule adds an event the run loop will deliver.
func (p *plane) schedule(ev event) {
	p.mu.Lock()
	defer p.mu.Unlock()

	heap.Push(&p.events, ev)
}

// next pops the earliest event, or reports that none remain.
func (p *plane) next() (event, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if len(p.events) == 0 {
		return event{}, false
	}

	ev, ok := heap.Pop(&p.events).(event)

	return ev, ok
}

// deliver puts one event on GitHub's queue as the message it is.
func (p *plane) deliver(ev event) {
	p.mu.Lock()
	defer p.mu.Unlock()

	a := p.jobs[ev.seq]

	// A DELIVERY OFFERS NOTHING BEYOND ITSELF. What the set could not take before
	// is offered again only when the run loop nudges it, in the fleet's order,
	// after this event has settled.
	switch ev.kind {
	case eventArrival:
		s := p.byName[a.Tier]
		if s == nil {
			p.t.Fatalf("replay: job %d arrives on tier %q, which has no scale set", a.Seq, a.Tier)
		}

		s.available[a.Seq] = struct{}{}
		p.pushLocked(s, nil, []int64{a.Seq}, p.jobJSON("JobAvailable", a, fakeactions.JobFields{}))

	case eventStarted:
		p.pushLocked(ev.runner.set, nil, nil, p.jobJSON("JobStarted", a, fakeactions.JobFields{
			RunnerID: ev.runner.id, RunnerName: ev.runner.name,
		}))

	case eventCompleted:
		delete(ev.runner.set.assigned, a.Seq)
		p.pushLocked(ev.runner.set, nil, nil, p.jobJSON("JobCompleted", a, fakeactions.JobFields{
			RunnerID: ev.runner.id, RunnerName: ev.runner.name, Result: a.Result,
		}))
	}
}

// AwaitIdle returns once every tier's session is open, its queue is empty and
// its listener is parked with an unchanged advertisement: billet has nothing
// left to do about what it has been told so far.
func (p *plane) AwaitIdle(deadline <-chan struct{}) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	for !p.idleLocked() {
		select {
		case <-deadline:
			return fmt.Errorf("replay: billet did not settle: %s", p.stateLocked())
		default:
		}

		p.cond.Wait()
	}

	return nil
}

func (p *plane) idleLocked() bool {
	if len(p.byName) != len(p.order) {
		return false
	}

	for _, label := range p.order {
		s := p.byName[label]
		if s == nil || !s.opened || !s.parked || len(s.queue) > 0 {
			return false
		}
	}

	return true
}

// stateLocked describes every set, for the failure that names why a replay
// stalled.
func (p *plane) stateLocked() string {
	var b strings.Builder

	for _, label := range p.order {
		s := p.byName[label]
		if s == nil {
			fmt.Fprintf(&b, "%s: no scale set; ", label)

			continue
		}

		fmt.Fprintf(&b, "%s: opened=%v parked=%v queued=%d capacity=%d available=%d assigned=%d; ",
			label, s.opened, s.parked, len(s.queue), s.lastCap, len(s.available), len(s.assigned))
	}

	return b.String()
}
