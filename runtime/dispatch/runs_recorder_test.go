package dispatch

import (
	"context"
	"testing"
	"time"
)

type fakeRecorder struct {
	started []startCall
	done    []doneCall
}

type startCall struct{ runID, ticket, agent, thread, repo, command, parent string }

type doneCall struct {
	runID, status, pr string
	dur               int
}

func (f *fakeRecorder) RecordRunStart(_ context.Context, runID, ticket, agent, thread, repo, command, parentRunID string, dispatchMsgID int64) {
	f.started = append(f.started, startCall{runID, ticket, agent, thread, repo, command, parentRunID})
}

func (f *fakeRecorder) RecordRunDone(_ context.Context, runID, status string, completedAt time.Time, prURL string, durationSecs int) {
	f.done = append(f.done, doneCall{runID, status, prURL, durationSecs})
}

func TestReserveRecordsRunStart(t *testing.T) {
	rec := &fakeRecorder{}
	r := &Runner{Recorder: rec, NewID: func() string { return "run-x" }}
	if err := r.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	run := r.reserve(Brief{Agent: "anvil", Ticket: "NEX-1", Thread: "NEX-1", Repo: "o/r", Task: "brief text"})
	if run.ID != "run-x" {
		t.Fatalf("run id = %q", run.ID)
	}
	if len(rec.started) != 1 || rec.started[0].agent != "anvil" || rec.started[0].command != "brief text" {
		t.Fatalf("RecordRunStart not called correctly: %+v", rec.started)
	}
}

func TestJobDoneRecordsRunDone(t *testing.T) {
	rec := &fakeRecorder{}
	r := &Runner{Recorder: rec, NewID: func() string { return "run-y" }}
	if err := r.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.reserve(Brief{Agent: "anvil", Ticket: "NEX-2", Thread: "NEX-2"})
	r.OnJobDone(JobDone{Ticket: "NEX-2", OK: true, CompletedAt: time.UnixMilli(9000)})
	if len(rec.done) != 1 || rec.done[0].runID != "run-y" || rec.done[0].status != "complete" {
		t.Fatalf("RecordRunDone not called correctly: %+v", rec.done)
	}
}

func TestJobDoneRecordsPRURL(t *testing.T) {
	rec := &fakeRecorder{}
	r := &Runner{Recorder: rec, NewID: func() string { return "run-pr" }}
	if err := r.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(SetLookupPRURLForTest(func(repo, branch string) (string, error) {
		if repo != "org/repo" || branch != "feature/x" {
			t.Fatalf("lookup args repo=%q branch=%q", repo, branch)
		}
		return "https://github.com/org/repo/pull/1", nil
	}))
	r.reserve(Brief{Agent: "anvil", Ticket: "NEX-3", Thread: "NEX-3", Repo: "org/repo", Branch: "feature/x"})
	r.OnJobDone(JobDone{Ticket: "NEX-3", OK: true, CompletedAt: time.UnixMilli(9000)})
	if len(rec.done) != 1 || rec.done[0].pr != "https://github.com/org/repo/pull/1" {
		t.Fatalf("RecordRunDone PR URL: %+v", rec.done)
	}
}
