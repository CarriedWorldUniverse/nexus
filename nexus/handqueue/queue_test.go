package handqueue

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nexus-cw/nexus/nexus/frames"
)

func TestSubmitRunsExecutor(t *testing.T) {
	var calls atomic.Int32
	q, err := New(Config{
		MaxConcurrent: 1,
		Executor: ExecutorFunc(func(ctx context.Context, req frames.HandDispatchPayload) (frames.HandResultPayload, error) {
			calls.Add(1)
			return frames.HandResultPayload{
				TargetAspect: req.TargetAspect,
				HandName:     req.HandName,
				Output:       map[string]any{"echo": req.HandName},
			}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer q.Shutdown(context.Background())

	ctx := context.Background()
	resp, err := q.Submit(ctx, frames.HandDispatchPayload{
		TargetAspect: "wren",
		HandName:     "verify-canon",
		Input:        map[string]any{"text": "hi"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.HandName != "verify-canon" {
		t.Errorf("HandName = %q", resp.HandName)
	}
	if calls.Load() != 1 {
		t.Errorf("executor calls = %d, want 1", calls.Load())
	}
}

func TestConcurrencyCap(t *testing.T) {
	var inFlight atomic.Int32
	var maxSeen atomic.Int32
	start := make(chan struct{})
	q, err := New(Config{
		MaxConcurrent: 3,
		Executor: ExecutorFunc(func(ctx context.Context, _ frames.HandDispatchPayload) (frames.HandResultPayload, error) {
			n := inFlight.Add(1)
			for {
				m := maxSeen.Load()
				if n <= m {
					break
				}
				if maxSeen.CompareAndSwap(m, n) {
					break
				}
			}
			<-start
			time.Sleep(50 * time.Millisecond)
			inFlight.Add(-1)
			return frames.HandResultPayload{}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer q.Shutdown(context.Background())

	// Submit 10 jobs in parallel.
	results := make(chan error, 10)
	for i := 0; i < 10; i++ {
		go func() {
			_, err := q.Submit(context.Background(), frames.HandDispatchPayload{
				TargetAspect: "x", HandName: "noop",
			})
			results <- err
		}()
	}

	// Let jobs progress.
	time.Sleep(50 * time.Millisecond)
	close(start)

	for i := 0; i < 10; i++ {
		if err := <-results; err != nil {
			t.Errorf("Submit err: %v", err)
		}
	}

	if maxSeen.Load() > 3 {
		t.Errorf("in-flight max = %d, want ≤ 3 (concurrency cap)", maxSeen.Load())
	}
}

func TestSubmitSurfacesExecutorError(t *testing.T) {
	q, _ := New(Config{
		MaxConcurrent: 1,
		Executor: ExecutorFunc(func(context.Context, frames.HandDispatchPayload) (frames.HandResultPayload, error) {
			return frames.HandResultPayload{}, errors.New("simulated failure")
		}),
	})
	defer q.Shutdown(context.Background())

	_, err := q.Submit(context.Background(), frames.HandDispatchPayload{TargetAspect: "x", HandName: "n"})
	if err == nil || err.Error() != "simulated failure" {
		t.Errorf("err = %v, want simulated failure", err)
	}
}

func TestSubmitRespectsContext(t *testing.T) {
	// Queue capacity 1 + 1 worker; block the worker so Submit's
	// channel send backs up and ctx can time it out.
	block := make(chan struct{})
	q, _ := New(Config{
		MaxConcurrent: 1,
		Executor: ExecutorFunc(func(ctx context.Context, _ frames.HandDispatchPayload) (frames.HandResultPayload, error) {
			<-block
			return frames.HandResultPayload{}, nil
		}),
	})
	defer func() { close(block); q.Shutdown(context.Background()) }()

	// Fill the first worker slot.
	go q.Submit(context.Background(), frames.HandDispatchPayload{TargetAspect: "x", HandName: "first"})
	time.Sleep(20 * time.Millisecond)

	// Second submit with a tight ctx should time out waiting for result.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := q.Submit(ctx, frames.HandDispatchPayload{TargetAspect: "x", HandName: "second"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want DeadlineExceeded", err)
	}
}
