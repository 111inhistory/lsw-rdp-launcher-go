package controller

import (
	"context"
	"errors"
	"testing"
)

func TestQueryFailureCompletesQueuedCallersRegardlessOfSourceMessageOrder(t *testing.T) {
	for _, tc := range []struct {
		name  string
		steps []string
	}{
		{"failure before loss", []string{"done", "lost", "ready"}},
		{"loss before failure", []string{"lost", "done", "ready"}},
		{"replacement acknowledged before old failure", []string{"lost", "ready", "done"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := testMachine()
			m.vmSource(false)
			m.vmSource(true)
			a := backgroundJob(m, "A")
			b := backgroundJob(m, "B")
			query := expectOp(t, m, epoch, QueryVM)
			for _, step := range tc.steps {
				switch step {
				case "lost":
					m.vmSource(false)
				case "ready":
					m.vmSource(true)
				case "done":
					m.done(query.ID, Outcome{Err: context.DeadlineExceeded}, epoch)
					if len(m.queue) != 0 || len(m.completed) != 2 {
						t.Fatal("query failure did not immediately complete both callers", m.snapshot(), m.completed)
					}
					for i, caller := range []*job{a, b} {
						if m.completed[i].job != caller || !errors.Is(m.completed[i].result.Err, context.DeadlineExceeded) {
							t.Fatal("caller received a different result", m.completed[i])
						}
					}
				}
			}
			if !m.needQuery || m.vmSubscription != subscriptionNeedsSnapshot {
				t.Fatal("old query error erased replacement snapshot requirement", m.snapshot())
			}
			refresh := expectOp(t, m, epoch, QueryVM)
			m.done(refresh.ID, Outcome{VM: VMRunning}, epoch)
			m.backend(true, SSHZero)
			expectIdle(t, m, epoch)
			if len(m.completed) != 2 || m.state.VM != VMRunning {
				t.Fatal("failed callers were replayed or observation did not recover", m.snapshot())
			}
		})
	}
}
