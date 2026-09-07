-- module: academic_statistics
DROP TABLE academic_course_pass_rate_statistics;

ALTER TABLE academic_statistics_batches
  DROP COLUMN minimum_sample_size,
  DROP COLUMN course_pass_rate_count;
