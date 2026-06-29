package shadowrunner

import "testing"

func TestWorkqueue_IdleTriggerRuns(t *testing.T) {
	q := NewWorkqueue()
	if !q.Trigger() { // returns true = caller should start a drain now
		t.Fatal("idle+trigger should signal a drain")
	}
}

func TestWorkqueue_TriggerWhileRunningSetsPending(t *testing.T) {
	q := NewWorkqueue()
	q.Trigger()      // idle -> running (drain starts)
	if q.Trigger() { // running+trigger -> pending, NOT a new drain
		t.Fatal("trigger while running must not start a 2nd drain")
	}
	if !q.Done() { // finish: pending was set -> should re-drain
		t.Fatal("Done with pending should signal re-drain")
	}
	if q.Done() { // finish again: no pending -> idle, no re-drain
		t.Fatal("Done with no pending should not re-drain")
	}
}

func TestWorkqueue_BurstCollapsesToOne(t *testing.T) {
	q := NewWorkqueue()
	q.Trigger()                  // -> running
	for i := 0; i < 5; i++ {     // 5 mid-drain triggers
		q.Trigger()
	}
	if !q.Done() { // one follow-up drain
		t.Fatal("burst should collapse to exactly one follow-up")
	}
	if q.Done() { // and no more
		t.Fatal("no second follow-up")
	}
}
