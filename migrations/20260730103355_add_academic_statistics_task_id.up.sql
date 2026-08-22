-- module: academic_statistics
ALTER TABLE academic_statistics_batches ADD COLUMN trigger_task_id VARCHAR(96) NULL;
ALTER TABLE academic_statistics_batches ADD UNIQUE INDEX uk_academic_statistics_batch_trigger_task (trigger_task_id);
