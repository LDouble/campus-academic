-- module: academic_statistics
ALTER TABLE academic_statistics_batches DROP INDEX uk_academic_statistics_batch_trigger_task;
ALTER TABLE academic_statistics_batches DROP COLUMN trigger_task_id;
