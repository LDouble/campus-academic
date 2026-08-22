-- module: academic_statistics
ALTER TABLE academic_statistics_batches ADD COLUMN trigger_type VARCHAR(16) NOT NULL;
ALTER TABLE academic_statistics_batches ADD COLUMN triggered_by BIGINT UNSIGNED NULL;
ALTER TABLE academic_statistics_batches ADD COLUMN trigger_note VARCHAR(200) NULL;
