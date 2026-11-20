package nodeclient

import (
	"sync"

	"github.com/junioryono/billet/internal/nodeapi"
)

// destroyQueue runs destroy commands beside the command loop, one at a time, in
// the order they arrived.
//
// A DESTROY MUST NOT HOLD THE LOOP. A completion's destroy settles the job's Docker
// image store first, and that waits up to dockerSettleWait (two minutes) for a
// guest that never says it quiesced. Run on the loop, a few of those in a row kept
// the node from taking a launch for longer than the command's ten-minute window:
// on 2026-11-20 a consumer's lint job was assigned at 07:45:45, the launch went
// unanswered while ubuntu-01 worked through teardowns, GitHub cancelled the
// assignment twice and never offered the job again, and it sat queued for ten
// hours. Beside the loop, the loop keeps polling and a launch starts while a
// teardown settles.
//
// IN ORDER, AND ONE AT A TIME, so destroys see the node as they did on the loop:
// two never overlap, and each runs after every destroy before it. AND BEHIND THE
// LAUNCHES IT ARRIVED BEHIND, never behind later ones: a destroy waits for the
// launches in flight when it was queued, so it can never overtake its own
// lease's launch, and a node that is always launching still tears down. A LAUNCH
// TAKEN AFTER A DESTROY OF ITS REQUEST waits for that destroy (pending): a destroy
// is addressed by request id, and one that ran after the launch would tear down
// what the launch started. The plane keeps such a launch back while the destroy is
// out; this is for one the plane gave up on after its command timeout, which the
// node still holds.
//
// THE PLANE IS TOLD WHICH IT HOLDS. A plane gives back a destroy a node polled past
// without answering, as lost, because a node used to answer one before its next
// poll; every poll names what this queue holds (held), so none is handed over twice
// and the next command comes instead (VersionDestroysBeside). The plane gives a node
// at most nodeapi.MaxDestroysRunning, the most a poll names, counting those this
// node named; one past it, which only a plane that did not keep to that would give,
// waits for a place (waitRoom), one destroy's settling at most, so a held destroy is
// never left out of a poll.
//
// A MAINTENANCE TURN (turn) runs in the same line: the node's sweep and custody
// tending must not run beside a destroy, and waiting for the queue to be empty could
// wait for ever on a node that is always tearing down.
//
// THE OPERATOR'S SECOND SIGNAL ENDS THE LINE (stop): what is running finishes, and
// nothing queued behind it starts. Each would run under a fresh bounded context of
// its own, so a long line would hold a forced stop for as long as it took; what is
// left is the plane's to give again, as it gives any destroy it was not answered.
type destroyQueue struct {
	mu      sync.Mutex
	queue   []*queuedDestroy
	holding map[string]*queuedDestroy
	running bool
	err     error
	idle    *sync.Cond
	stop    <-chan struct{}
}

type queuedDestroy struct {
	id      string
	request int64
	work    func() error
	done    chan struct{}
}

// newDestroyQueue is a queue whose line ends when stop closes; nil never does.
func newDestroyQueue(stop <-chan struct{}) *destroyQueue {
	q := &destroyQueue{holding: map[string]*queuedDestroy{}, stop: stop}
	q.idle = sync.NewCond(&q.mu)

	return q
}

// waitRoom returns once the queue holds fewer destroys than a poll can name: one
// destroy's settling at most, never the whole line's.
func (q *destroyQueue) waitRoom() {
	q.mu.Lock()
	defer q.mu.Unlock()

	for len(q.holding) >= nodeapi.MaxDestroysRunning {
		q.idle.Wait()
	}
}

// pending is a channel for each destroy of request queued or running now, closed
// once it has run. Only those: a destroy of the same request queued later waits for
// the launches in flight when it is queued, and a launch that waited for it in turn
// would wait for itself.
func (q *destroyQueue) pending(request int64) []<-chan struct{} {
	q.mu.Lock()
	defer q.mu.Unlock()

	var out []<-chan struct{}
	for _, d := range q.holding {
		if d.request == request {
			out = append(out, d.done)
		}
	}

	return out
}

// submit queues the destroy command id of request to run once every destroy before
// it has, after waiting for each channel in behind to close, and reports false,
// queuing nothing, when it already holds id: a destroy given twice runs once, and
// its one report answers both.
func (q *destroyQueue) submit(id string, request int64, behind []<-chan struct{}, destroy func() error) bool {
	q.mu.Lock()
	defer q.mu.Unlock()

	if _, held := q.holding[id]; held {
		return false
	}

	d := &queuedDestroy{id: id, request: request, done: make(chan struct{}), work: q.behind(behind, destroy)}
	q.holding[id] = d
	q.enqueueLocked(d)

	return true
}

// after queues work behind every destroy queued now and the launches in behind, held
// by no command: a command of the control plane's that must not run beside a
// teardown, and need not hold the loop for one.
func (q *destroyQueue) after(behind []<-chan struct{}, work func() error) {
	q.mu.Lock()
	defer q.mu.Unlock()

	q.enqueueLocked(&queuedDestroy{done: make(chan struct{}), work: q.behind(behind, work)})
}

// behind is work run once every channel in launches has closed, unless the second
// signal came while it waited: then it is not started, and its command is left for the
// plane to give again.
func (q *destroyQueue) behind(launches []<-chan struct{}, work func() error) func() error {
	return func() error {
		for _, done := range launches {
			<-done
		}

		if q.stopped() {
			return nil
		}

		return work()
	}
}

// turn queues maintain behind every destroy queued now, held by no command, so the
// destroys queued after it wait for it.
func (q *destroyQueue) turn(maintain func()) {
	q.mu.Lock()
	defer q.mu.Unlock()

	q.enqueueLocked(&queuedDestroy{done: make(chan struct{}), work: func() error {
		maintain()

		return nil
	}})
}

func (q *destroyQueue) enqueueLocked(d *queuedDestroy) {
	q.queue = append(q.queue, d)
	if !q.running {
		q.running = true

		go q.drain()
	}
}

// held is the command ids of the destroys queued or running, for the next poll.
func (q *destroyQueue) held() []string {
	q.mu.Lock()
	defer q.mu.Unlock()

	out := make([]string, 0, len(q.holding))
	for id := range q.holding {
		out = append(out, id)
	}

	return out
}

// drain runs queued work until none is left.
func (q *destroyQueue) drain() {
	for {
		q.mu.Lock()
		if q.stopped() {
			for _, d := range q.queue {
				if d.id != "" {
					delete(q.holding, d.id)
				}
				close(d.done)
			}
			q.queue = nil
		}

		if len(q.queue) == 0 {
			q.running = false
			q.idle.Broadcast()
			q.mu.Unlock()

			return
		}

		next := q.queue[0]
		q.queue = q.queue[1:]
		q.mu.Unlock()

		// HELD UNTIL ITS REPORT IS SENT, which work includes: a poll between the two
		// would otherwise not name it, and the plane would give it back.
		err := next.work()

		q.mu.Lock()
		if next.id != "" {
			delete(q.holding, next.id)
		}
		close(next.done)
		q.idle.Broadcast()
		if err != nil && q.err == nil {
			q.err = err
		}
		q.mu.Unlock()
	}
}

// stopped reports whether stop has closed.
func (q *destroyQueue) stopped() bool {
	select {
	case <-q.stop:
		return true
	default:
		return false
	}
}

// busy reports whether a destroy or a maintenance turn is queued or running.
func (q *destroyQueue) busy() bool {
	q.mu.Lock()
	defer q.mu.Unlock()

	return q.running
}

// wait returns once everything queued has run.
func (q *destroyQueue) wait() {
	q.mu.Lock()
	defer q.mu.Unlock()

	for q.running {
		q.idle.Wait()
	}
}

// failed is the first error a destroy's report ended the loop with, if any.
func (q *destroyQueue) failed() error {
	q.mu.Lock()
	defer q.mu.Unlock()

	return q.err
}
