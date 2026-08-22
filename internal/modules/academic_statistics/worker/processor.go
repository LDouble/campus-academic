package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/LDouble/campus-academic/internal/modules/academic_statistics/application"
	"github.com/hibiken/asynq"
)

// ManualRunner executes one Analytics-owned aggregation.
type ManualRunner interface {
	RunManual(context.Context, uint64, string, string) error
}

// Processor handles the direct, self-contained manual-run task payload.
type Processor struct{ runner ManualRunner }

func NewProcessor(runner ManualRunner) *Processor { return &Processor{runner: runner} }

func (processor *Processor) Register(mux *asynq.ServeMux) {
	mux.HandleFunc(application.ManualRunTaskType, processor.HandleManualRun)
}

func (processor *Processor) HandleManualRun(ctx context.Context, task *asynq.Task) error {
	if processor == nil || processor.runner == nil {
		return skipRetry("academic analytics runner is unavailable")
	}
	command := application.ManualRunCommand{}
	if err := json.Unmarshal(task.Payload(), &command); err != nil {
		return skipRetry("decode academic analytics task: " + err.Error())
	}
	command.Note = strings.TrimSpace(command.Note)
	if command.ActorID == 0 || command.TaskID == "" || len(command.TaskID) > 96 || utf8.RuneCountInString(command.Note) > 200 {
		return skipRetry("invalid academic analytics task payload")
	}
	if err := processor.runner.RunManual(ctx, command.ActorID, command.Note, command.TaskID); err != nil {
		return fmt.Errorf("run manual academic analytics: %w", err)
	}
	return nil
}

func skipRetry(message string) error { return fmt.Errorf("%s: %w", message, asynq.SkipRetry) }

// TaskRetryDelay keeps lock contention sparse while allowing genuine failures
// to use Asynq's normal exponential backoff.
func TaskRetryDelay(retryCount int, err error, task *asynq.Task) time.Duration {
	if errors.Is(err, application.ErrRunAlreadyLocked) {
		return 15 * time.Minute
	}
	return asynq.DefaultRetryDelayFunc(retryCount, err, task)
}
