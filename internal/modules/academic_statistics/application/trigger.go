package application

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/LDouble/campus-academic/internal/core/apperror"
	"github.com/LDouble/campus-academic/internal/core/idempotency"
)

const (
	// ManualRunConfirmation is the exact text required for an expensive
	// administrator-triggered recomputation.
	ManualRunConfirmation = "立即重算"
	maxTriggerNoteRunes   = 200

	// ManualRunTaskType is the versioned Asynq task contract.
	ManualRunTaskType = "academic_statistics:manual_run:v1"
	// ManualRunQueue isolates the expensive source aggregation from notices.
	ManualRunQueue = "academic_statistics"
	// ManualRunRequestedEventType is the durable outbox contract relayed to Asynq.
	ManualRunRequestedEventType = "academic_statistics.manual_run_requested"
	// ManualRunAggregateType groups manual-run intents in the shared outbox.
	ManualRunAggregateType = "academic_statistics"
)

// ManualRunCommand is the validated durable event payload for a manual run.
type ManualRunCommand struct {
	ActorID        uint64 `json:"actor_id"`
	Note           string `json:"note,omitempty"`
	TaskID         string `json:"task_id,omitempty"`
	IdempotencyKey string `json:"-"`
}

// ManualRunTaskPayload contains only a reference to the authoritative durable
// event. Queue writers cannot supply the actor, note, or task identity.
type ManualRunTaskPayload struct {
	EventID uint64 `json:"event_id"`
}

// ManualRunInput contains the authenticated administrator request.
type ManualRunInput struct {
	ActorID        uint64
	Confirmation   string
	Note           string
	IdempotencyKey string
}

// RunReceipt identifies an accepted asynchronous aggregation task.
type RunReceipt struct {
	TaskID   string
	QueuedAt time.Time
}

// RunPublisher enqueues administrator-triggered aggregation work.
type RunPublisher interface {
	Enqueue(context.Context, ManualRunCommand) (RunReceipt, error)
}

// TriggerManual validates and enqueues a manual aggregation request.
func (manager *Manager) TriggerManual(
	ctx context.Context,
	input ManualRunInput,
) (RunReceipt, error) {
	input.Confirmation = strings.TrimSpace(input.Confirmation)
	input.Note = strings.TrimSpace(input.Note)
	input.IdempotencyKey = strings.TrimSpace(input.IdempotencyKey)
	if input.Confirmation != ManualRunConfirmation {
		return RunReceipt{}, apperror.New(
			http.StatusBadRequest,
			"academic_statistics_confirmation_invalid",
			"请输入“立即重算”进行确认",
		)
	}
	if input.ActorID == 0 {
		return RunReceipt{}, fmt.Errorf(
			"academic statistics manual run actor is required",
		)
	}
	if utf8.RuneCountInString(input.Note) > maxTriggerNoteRunes {
		return RunReceipt{}, apperror.New(
			http.StatusBadRequest,
			"academic_statistics_note_too_long",
			"操作备注不能超过 200 个字符",
		)
	}
	if input.IdempotencyKey == "" ||
		len(input.IdempotencyKey) > idempotency.MaxKeyLength {
		return RunReceipt{}, apperror.New(
			http.StatusBadRequest,
			"invalid_idempotency_key",
			"Idempotency-Key 缺失或超过 128 字符",
		)
	}
	if manager.queue == nil {
		return RunReceipt{}, apperror.New(
			http.StatusServiceUnavailable,
			"academic_statistics_runner_unavailable",
			"学业统计任务暂不可用，请确认 Worker 已启用",
		)
	}
	receipt, err := manager.queue.Enqueue(ctx, ManualRunCommand{
		ActorID:        input.ActorID,
		Note:           input.Note,
		IdempotencyKey: input.IdempotencyKey,
	})
	if err != nil {
		return RunReceipt{}, apperror.Wrap(
			http.StatusServiceUnavailable,
			"academic_statistics_queue_unavailable",
			"学业统计任务入队失败，请稍后重试",
			err,
		)
	}
	return receipt, nil
}
