package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// ---------- Scheduler runningExecutions ----------

func TestScheduler_GetRunningExecutions_Empty(t *testing.T) {
	s := NewScheduler()
	result := s.GetRunningExecutions(1)
	assert.Empty(t, result, "should return empty for no executions")
}

func TestScheduler_GetRunningExecutions_ByTaskID(t *testing.T) {
	s := NewScheduler()

	// Add executions for two different tasks
	s.runningExecutions.Store(TestExecID1, &RunningExecution{
		ID:          TestExecID1,
		TaskID:      1,
		CancelFunc:  func() {},
		StartedAt:   time.Now(),
		TriggerType: TriggerAuto,
	})
	s.runningExecutions.Store(TestExecID2, &RunningExecution{
		ID:          TestExecID2,
		TaskID:      2,
		CancelFunc:  func() {},
		StartedAt:   time.Now(),
		TriggerType: TriggerManual,
	})
	s.runningExecutions.Store(TestExecID3, &RunningExecution{
		ID:          TestExecID3,
		TaskID:      1,
		CancelFunc:  func() {},
		StartedAt:   time.Now(),
		TriggerType: TriggerAuto,
	})

	// Get executions for task 1
	result := s.GetRunningExecutions(1)
	assert.Len(t, result, 2, "task 1 should have 2 executions")

	// Get executions for task 2
	result = s.GetRunningExecutions(2)
	assert.Len(t, result, 1, "task 2 should have 1 execution")

	// Get executions for non-existent task
	result = s.GetRunningExecutions(999)
	assert.Empty(t, result)

	// Cleanup
	s.runningExecutions.Delete(TestExecID1)
	s.runningExecutions.Delete(TestExecID2)
	s.runningExecutions.Delete(TestExecID3)
}

func TestScheduler_GetRunningCounts(t *testing.T) {
	s := NewScheduler()

	s.runningExecutions.Store(TestExecID1, &RunningExecution{
		ID: TestExecID1, TaskID: 1, CancelFunc: func() {}, StartedAt: time.Now(), TriggerType: TriggerAuto,
	})
	s.runningExecutions.Store(TestExecID2, &RunningExecution{
		ID: TestExecID2, TaskID: 1, CancelFunc: func() {}, StartedAt: time.Now(), TriggerType: TriggerManual,
	})
	s.runningExecutions.Store(TestExecID3, &RunningExecution{
		ID: TestExecID3, TaskID: 2, CancelFunc: func() {}, StartedAt: time.Now(), TriggerType: TriggerAuto,
	})

	counts := s.GetRunningCounts()
	assert.Equal(t, 2, counts[1])
	assert.Equal(t, 1, counts[2])

	// Cleanup
	s.runningExecutions.Delete(TestExecID1)
	s.runningExecutions.Delete(TestExecID2)
	s.runningExecutions.Delete(TestExecID3)
}

func TestScheduler_HasRunningExecutions(t *testing.T) {
	s := NewScheduler()

	assert.False(t, s.HasRunningExecutions(1), "should be false with no executions")

	s.runningExecutions.Store(TestExecID1, &RunningExecution{
		ID: TestExecID1, TaskID: 1, CancelFunc: func() {}, StartedAt: time.Now(), TriggerType: TriggerAuto,
	})

	assert.True(t, s.HasRunningExecutions(1), "should be true when execution exists")
	assert.False(t, s.HasRunningExecutions(2), "should be false for different task")

	s.runningExecutions.Delete(TestExecID1)
}

func TestScheduler_CancelExecution_Found(t *testing.T) {
	s := NewScheduler()
	cancelled := false
	ctx, cancel := context.WithCancel(context.Background())

	s.runningExecutions.Store(TestExecID1, &RunningExecution{
		ID:          TestExecID1,
		TaskID:      1,
		CancelFunc:  cancel,
		StartedAt:   time.Now(),
		TriggerType: TriggerAuto,
	})

	// Replace cancel with our own to detect invocation
	s.runningExecutions.Store(TestExecID1, &RunningExecution{
		ID: TestExecID1, TaskID: 1,
		CancelFunc:  func() { cancelled = true; cancel() },
		StartedAt:   time.Now(),
		TriggerType: TriggerAuto,
	})

	err := s.CancelExecution(TestExecID1)
	assert.NoError(t, err)
	assert.True(t, cancelled, "cancel function should have been called")
	assert.Error(t, ctx.Err(), "context should be cancelled")

	s.runningExecutions.Delete(TestExecID1)
}

func TestScheduler_CancelExecution_NotFound(t *testing.T) {
	s := NewScheduler()
	err := s.CancelExecution("nonexistent")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "execution not found")
}

func TestScheduler_CancelAllExecutions(t *testing.T) {
	s := NewScheduler()
	cancelledCount := 0

	s.runningExecutions.Store(TestExecID1, &RunningExecution{
		ID: TestExecID1, TaskID: 1,
		CancelFunc: func() { cancelledCount++ },
		StartedAt:  time.Now(), TriggerType: TriggerAuto,
	})
	s.runningExecutions.Store(TestExecID2, &RunningExecution{
		ID: TestExecID2, TaskID: 1,
		CancelFunc: func() { cancelledCount++ },
		StartedAt:  time.Now(), TriggerType: TriggerManual,
	})
	s.runningExecutions.Store(TestExecID3, &RunningExecution{
		ID: TestExecID3, TaskID: 2,
		CancelFunc: func() { cancelledCount++ },
		StartedAt:  time.Now(), TriggerType: TriggerAuto,
	})

	// Cancel all for task 1 only
	s.CancelAllExecutions(1)
	assert.Equal(t, 2, cancelledCount, "should cancel 2 executions for task 1")

	s.runningExecutions.Delete(TestExecID1)
	s.runningExecutions.Delete(TestExecID2)
	s.runningExecutions.Delete(TestExecID3)
}

// ── Completion transition (runningCount drops to 0) ──

func TestScheduler_CompletionTransition(t *testing.T) {
	s := NewScheduler()

	// Task 1 starts running
	s.runningExecutions.Store(TestExecID1, &RunningExecution{
		ID: TestExecID1, TaskID: 1, CancelFunc: func() {}, StartedAt: time.Now(), TriggerType: TriggerAuto,
	})

	// Before completion: runningCount > 0
	counts := s.GetRunningCounts()
	assert.Equal(t, 1, counts[1], "task 1 should have running count 1")
	assert.True(t, s.HasRunningExecutions(1))

	// Task 1 completes (backend deletes from sync.Map)
	s.runningExecutions.Delete(TestExecID1)

	// After completion: runningCount = 0
	counts = s.GetRunningCounts()
	_, exists := counts[1]
	assert.False(t, exists, "task 1 should have no running count after completion")
	assert.False(t, s.HasRunningExecutions(1), "HasRunningExecutions should be false")
}

func TestScheduler_MultipleTasksPartialCompletion(t *testing.T) {
	s := NewScheduler()

	// Two tasks running
	s.runningExecutions.Store(TestExecID1, &RunningExecution{
		ID: TestExecID1, TaskID: 1, CancelFunc: func() {}, StartedAt: time.Now(), TriggerType: TriggerAuto,
	})
	s.runningExecutions.Store(TestExecID2, &RunningExecution{
		ID: TestExecID2, TaskID: 2, CancelFunc: func() {}, StartedAt: time.Now(), TriggerType: TriggerAuto,
	})

	counts := s.GetRunningCounts()
	assert.Equal(t, 1, counts[1])
	assert.Equal(t, 1, counts[2])

	// Only task 1 completes
	s.runningExecutions.Delete(TestExecID1)

	counts = s.GetRunningCounts()
	_, exists1 := counts[1]
	assert.False(t, exists1, "completed task should not appear in counts")
	assert.Equal(t, 1, counts[2], "still-running task should keep its count")
}

func TestScheduler_SameTaskMultipleExecutions(t *testing.T) {
	s := NewScheduler()

	// Task 1 has two concurrent executions
	s.runningExecutions.Store("exec-1a", &RunningExecution{
		ID: "exec-1a", TaskID: 1, CancelFunc: func() {}, StartedAt: time.Now(), TriggerType: TriggerAuto,
	})
	s.runningExecutions.Store("exec-1b", &RunningExecution{
		ID: "exec-1b", TaskID: 1, CancelFunc: func() {}, StartedAt: time.Now(), TriggerType: TriggerManual,
	})

	counts := s.GetRunningCounts()
	assert.Equal(t, 2, counts[1], "should count both executions")

	// One completes
	s.runningExecutions.Delete("exec-1a")
	counts = s.GetRunningCounts()
	assert.Equal(t, 1, counts[1], "should still have 1 running after one completes")

	// Both complete
	s.runningExecutions.Delete("exec-1b")
	counts = s.GetRunningCounts()
	_, exists := counts[1]
	assert.False(t, exists, "should have no running after both complete")
}

// ── Cron skip-if-running guard ──

func TestScheduler_CronSkipIfRunning(t *testing.T) {
	s := NewScheduler()

	// Task 1 has a running execution
	s.runningExecutions.Store("exec-running", &RunningExecution{
		ID: "exec-running", TaskID: 1, CancelFunc: func() {}, StartedAt: time.Now(), TriggerType: TriggerAuto,
	})

	// HasRunningExecutions should return true — cron callback would skip
	assert.True(t, s.HasRunningExecutions(1), "should skip when execution is running")

	// After execution completes, HasRunningExecutions should return false — cron would proceed
	s.runningExecutions.Delete("exec-running")
	assert.False(t, s.HasRunningExecutions(1), "should proceed after execution completes")
}

func TestScheduler_CronSkip_DifferentTaskIndependent(t *testing.T) {
	s := NewScheduler()

	// Task 1 is running, task 2 is not
	s.runningExecutions.Store(TestExecID1, &RunningExecution{
		ID: TestExecID1, TaskID: 1, CancelFunc: func() {}, StartedAt: time.Now(), TriggerType: TriggerAuto,
	})

	// Task 1 should be skipped, task 2 should proceed
	assert.True(t, s.HasRunningExecutions(1), "task 1 should be skipped")
	assert.False(t, s.HasRunningExecutions(2), "task 2 should not be affected")

	s.runningExecutions.Delete(TestExecID1)
	assert.False(t, s.HasRunningExecutions(1), "task 1 can proceed after completion")
}
