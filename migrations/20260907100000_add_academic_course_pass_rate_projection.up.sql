-- module: academic_statistics
ALTER TABLE academic_statistics_batches
  ADD COLUMN course_pass_rate_count BIGINT NOT NULL DEFAULT 0 AFTER course_stat_count,
  ADD COLUMN minimum_sample_size BIGINT NOT NULL DEFAULT 0 AFTER course_pass_rate_count;

CREATE TABLE academic_course_pass_rate_statistics (
  id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  batch_id BIGINT UNSIGNED NOT NULL,
  education_level VARCHAR(32) NOT NULL,
  course_code VARCHAR(50) NOT NULL,
  course_name VARCHAR(255) NOT NULL,
  term_count BIGINT NOT NULL,
  valid_count BIGINT NOT NULL,
  pass_count BIGINT NOT NULL,
  fail_count BIGINT NOT NULL,
  numeric_score_count BIGINT NOT NULL,
  numeric_score_sum_x100 BIGINT NOT NULL,
  numeric_fail_count BIGINT NOT NULL,
  score_60_69_count BIGINT NOT NULL,
  score_70_79_count BIGINT NOT NULL,
  score_80_89_count BIGINT NOT NULL,
  score_90_100_count BIGINT NOT NULL,
  level_excellent_count BIGINT NOT NULL,
  level_good_count BIGINT NOT NULL,
  level_medium_count BIGINT NOT NULL,
  level_pass_count BIGINT NOT NULL,
  level_fail_count BIGINT NOT NULL,
  created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
  updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3),
  PRIMARY KEY (id),
  UNIQUE KEY uk_academic_course_pass_rate_stat (batch_id, education_level, course_code),
  KEY idx_academic_course_pass_rate_list (batch_id, valid_count DESC, course_code ASC),
  KEY idx_academic_course_pass_rate_code (batch_id, course_code),
  CONSTRAINT fk_academic_course_pass_rate_statistics_batch_id
    FOREIGN KEY (batch_id) REFERENCES academic_statistics_batches(id) ON DELETE CASCADE
);

-- Keep published history queryable immediately after deployment. The old
-- batches did not retain the publication-time sample threshold, so their
-- cached total intentionally remains zero and is calculated cheaply from this
-- projection until the next publication records it.
INSERT INTO academic_course_pass_rate_statistics (
  batch_id, education_level, course_code, course_name, term_count,
  valid_count, pass_count, fail_count, numeric_score_count,
  numeric_score_sum_x100, numeric_fail_count, score_60_69_count,
  score_70_79_count, score_80_89_count, score_90_100_count,
  level_excellent_count, level_good_count, level_medium_count,
  level_pass_count, level_fail_count
)
SELECT
  statistics.batch_id,
  statistics.education_level,
  statistics.course_code,
  MAX(statistics.course_name),
  COUNT(*),
  SUM(statistics.valid_count),
  SUM(statistics.pass_count),
  SUM(statistics.fail_count),
  SUM(statistics.numeric_score_count),
  SUM(statistics.numeric_score_sum_x100),
  SUM(statistics.numeric_fail_count),
  SUM(statistics.score_60_69_count),
  SUM(statistics.score_70_79_count),
  SUM(statistics.score_80_89_count),
  SUM(statistics.score_90_100_count),
  SUM(statistics.level_excellent_count),
  SUM(statistics.level_good_count),
  SUM(statistics.level_medium_count),
  SUM(statistics.level_pass_count),
  SUM(statistics.level_fail_count)
FROM academic_course_term_statistics AS statistics
INNER JOIN academic_statistics_batches AS batches
  ON batches.id = statistics.batch_id
WHERE batches.status = 'published'
GROUP BY statistics.batch_id, statistics.education_level, statistics.course_code;
