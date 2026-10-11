package guestagent

import (
	"testing"
	"testing/synctest"
	"time"
)

// Even a configuration the agent refuses at once is refused inside the
// supervised work: a log nobody drains cannot hold SIGTERM there either.
func TestARefusedConfigurationDoesNotHoldSIGTERM(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})

		r := start(t, Config{Endpoint: "not a url", Token: testToken, Log: stuckWriter{release: release}})

		time.Sleep(time.Second)
		synctest.Wait()

		took, err := r.stop()
		if err != nil || took > 3*time.Second {
			t.Fatalf("SIGTERM with the refusal's line stuck returned %v after %v, want nil within 3s", err, took)
		}

		close(release)
	})
}

// A reading is stamped when it is read: a loop held from 9 s to 17 s does not date
// counters read at 17 s as read at 10 s.
func TestAReadingIsStampedWhenItIsRead(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		node := newFakeNode(t)
		began := time.Now()

		var w whenever

		var log syncBuffer

		cfg := testConfig(t, node, &log)
		cfg.Proc = heldFS{FS: cfg.Proc, hold: func(name string) {
			w.once(name == "stat" && time.Since(began) >= 8500*time.Millisecond, func() { time.Sleep(8 * time.Second) })
		}}

		r := start(t, cfg)

		time.Sleep(30 * time.Second)
		synctest.Wait()

		if _, err := r.stop(); err != nil {
			t.Fatal(err)
		}

		_, posts := node.snapshot()

		stamps := 0

		for _, p := range posts {
			for _, s := range p.batch.Samples {
				if at := time.UnixMilli(s.AtMillis).Sub(began); at > 9*time.Second && at < 17*time.Second {
					t.Errorf("a sample is stamped %v, inside the 8 s the loop was held", at)
				}

				stamps++
			}

			for _, ps := range p.batch.Processes {
				if at := time.UnixMilli(ps.AtMillis).Sub(began); at > 9*time.Second && at < 17*time.Second {
					t.Errorf("a process sample is stamped %v, inside the 8 s the loop was held", at)
				}
			}
		}

		if stamps < 15 {
			t.Fatalf("only %d samples were sent", stamps)
		}
	})
}
