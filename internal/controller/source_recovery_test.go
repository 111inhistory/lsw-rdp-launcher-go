package controller

import (
	"errors"
	"testing"
)

func TestVMLossWaitsForSubscriptionThenQueriesMissingState(t *testing.T) {
	m := testMachine()
	m.vmSource(false)
	addJob(m, "ping", BackendOnly)
	expectIdle(t, m, epoch)
	if m.state.VM != VMUnknown {
		t.Fatal(m.snapshot())
	}
	m.vmSource(true)
	// Even a new event must not erase the required post-subscription snapshot.
	m.vm(VMPaused)
	query := expectOp(t, m, epoch, QueryVM)
	m.done(query.ID, Outcome{VM: VMPaused}, epoch)
	expectOp(t, m, epoch, WakeVM)
}

func TestFailedReconnectSnapshotWaitsForExplicitRetry(t *testing.T) {
	m := testMachine()
	m.vmSource(false)
	m.vmSource(true)
	query := expectOp(t, m, epoch, QueryVM)
	m.done(query.ID, Outcome{Err: errors.New("snapshot RPC failed")}, epoch)
	expectIdle(t, m, epoch)
	expectIdle(t, m, epoch)
	addJob(m, "ping", BackendOnly)
	expectOp(t, m, epoch, QueryVM)
}

func TestOldQueryErrorCannotCancelNewSubscriptionRefresh(t *testing.T) {
	m := testMachine()
	m.needQuery = true
	old := expectOp(t, m, epoch, QueryVM)
	m.vmSource(false)
	m.vmSource(true)
	m.done(old.ID, Outcome{Err: errors.New("old connection failed")}, epoch)
	expectOp(t, m, epoch, QueryVM)
}

func TestVMLossRejectsAnOldQueryReply(t *testing.T) {
	m := testMachine()
	m.needQuery = true
	old := expectOp(t, m, epoch, QueryVM)
	m.vmSource(false)
	m.vmSource(true)
	m.done(old.ID, Outcome{VM: VMRunning}, epoch)
	if m.state.VM != VMUnknown {
		t.Fatal("old reply overwrote disconnected state")
	}
	expectOp(t, m, epoch, QueryVM)
}

func TestMemorySourceRecoveryQueriesBeforeWritesAndRejectsOldReply(t *testing.T) {
	m := memoryMachine(t)
	m.memory.DeviceKnown = false
	old := expectOp(t, m, epoch, QueryMemory)
	snapshot := m.memory.Device
	m.memorySource(false)
	if m.memory.DeviceKnown || !errors.Is(m.memory.LastError, ErrMemorySourceLost) {
		t.Fatal(m.memory)
	}
	m.memoryGuest(&GuestMemory{TotalBytes: 4 * GiB, AvailableBytes: GiB / 32})
	m.memorySource(true)
	m.done(old.ID, Outcome{Memory: &snapshot}, epoch)
	if m.memory.DeviceKnown || m.memory.LastError != nil {
		t.Fatal("old query made new subscription current", m.memory)
	}
	query := expectOp(t, m, epoch, QueryMemory)
	m.done(query.ID, Outcome{Memory: &snapshot}, epoch)
	expectOp(t, m, epoch, SetMemory)
}

func TestMemorySourceLossSurvivesColdBootStateReset(t *testing.T) {
	m := memoryMachine(t)
	m.memorySource(false)
	m.vm(VMOff)
	addJob(m, "ping", BackendOnly)
	expectIdle(t, m, epoch)
	if !m.memorySourceLost || !errors.Is(m.memory.LastError, ErrMemorySourceLost) {
		t.Fatal(m.snapshot())
	}
	m.memorySource(true)
	expectOp(t, m, epoch, WakeVM)
}

func TestMemoryReconnectDoesNotClearWriteOrSafetyError(t *testing.T) {
	for _, reason := range []string{"memory write failed", "safe resume target unknown"} {
		m := memoryMachine(t)
		failure := errors.New(reason)
		m.memory.LastError = failure
		m.memorySource(false)
		m.memorySource(true)
		if !errors.Is(m.memory.LastError, failure) || m.memory.DeviceKnown {
			t.Fatal(m.memory)
		}
		query := expectOp(t, m, epoch, QueryMemory)
		device := m.memory.Device
		m.done(query.ID, Outcome{Memory: &device}, epoch)
		if !errors.Is(m.memory.LastError, failure) {
			t.Fatal("read-only refresh cleared fault")
		}
		expectIdle(t, m, epoch)
	}
}

func TestVMLossDuringMemoryPreparationCannotResumeFromOldResult(t *testing.T) {
	m := memoryMachine(t)
	m.memory.WindowPrepared = false
	m.memory.phase = memoryWindow
	m.state.VM = VMPaused
	prepare := expectOp(t, m, epoch, PrepareMemory)
	m.vmSource(false)
	m.done(prepare.ID, Outcome{ResumeSafe: true, MemoryPrepared: true}, epoch)
	if m.memory.phase != memoryIdle || m.memory.WindowPrepared || m.memory.LastError == nil || m.state.VM != VMUnknown {
		t.Fatal(m.snapshot())
	}
	expectIdle(t, m, epoch)
	m.vmSource(true)
	query := expectOp(t, m, epoch, QueryVM)
	m.done(query.ID, Outcome{VM: VMPaused}, epoch)
	expectIdle(t, m, epoch)
	if m.state.VM != VMPaused {
		t.Fatal("unsafe automatic resume", m.snapshot())
	}
}
