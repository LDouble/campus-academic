package infrastructure

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/LDouble/campus-academic/internal/modules/academic_statistics/application"
	"github.com/hibiken/asynq"
)

const (
	manualRunTimeoutBuffer = 5 * time.Minute
	manualRunTaskRetention = 24 * time.Hour
)

// RunPublisher is the Analytics-owned asynchronous command publisher. The
// payload contains the already validated command; no platform outbox is
// needed because the task and publication audit live in the Analytics DB.
type RunPublisher struct {
	client  *asynq.Client
	queue   string
	timeout time.Duration
}

// NewRunPublisher creates a publisher for one Analytics Redis queue.
func NewRunPublisher(client *asynq.Client, queue string, queryTimeout time.Duration) *RunPublisher {
	if queue == "" {
		queue = application.ManualRunQueue
	}
	return &RunPublisher{
		client:  client,
		queue:   queue,
		timeout: queryTimeout + manualRunTimeoutBuffer,
	}
}

// Enqueue implements application.RunPublisher with deterministic task IDs.
func (publisher *RunPublisher) Enqueue(
	ctx context.Context,
	command application.ManualRunCommand,
) (application.RunReceipt, error) {
	if publisher == nil || publisher.client == nil {
		return application.RunReceipt{}, errors.New("academic analytics task client is unavailable")
	}
	if command.ActorID == 0 || command.IdempotencyKey == "" {
		return application.RunReceipt{}, errors.New("academic analytics command identity is invalid")
	}
	taskID := manualRunTaskID(command.ActorID, command.IdempotencyKey)
	command.TaskID = taskID
	payload, err := json.Marshal(command)
	if err != nil {
		return application.RunReceipt{}, fmt.Errorf("encode academic analytics task: %w", err)
	}
	task := asynq.NewTask(
		application.ManualRunTaskType,
		payload,
		asynq.TaskID(taskID),
		asynq.Queue(publisher.queue),
		asynq.MaxRetry(5),
		asynq.Timeout(publisher.timeout),
		asynq.Retention(manualRunTaskRetention),
	)
	_, err = publisher.client.EnqueueContext(ctx, task)
	if err != nil && !errors.Is(err, asynq.ErrTaskIDConflict) {
		return application.RunReceipt{}, fmt.Errorf("enqueue academic analytics task: %w", err)
	}
	return application.RunReceipt{TaskID: taskID, QueuedAt: time.Now().UTC()}, nil
}

func manualRunTaskID(actorID uint64, idempotencyKey string) string {
	digest := sha256.Sum256([]byte(strconv.FormatUint(actorID, 10) + ":" + idempotencyKey))
	return "academic-analytics-" + hex.EncodeToString(digest[:])
}
