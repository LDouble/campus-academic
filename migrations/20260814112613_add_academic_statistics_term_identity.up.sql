-- module: academic_statistics
ALTER TABLE academic_course_term_statistics
  ADD COLUMN education_level VARCHAR(24) NOT NULL DEFAULT '',
  ADD COLUMN period_id VARCHAR(64) NOT NULL DEFAULT '',
  ADD COLUMN term_label VARCHAR(100) NOT NULL DEFAULT '';

-- Existing v4 rows cannot be assigned an education level from term_code alone.
-- Give them a stable audit-only identity before replacing the legacy unique key;
-- current readers filter by rule_version and never publish these legacy values.
UPDATE academic_course_term_statistics
SET education_level = 'legacy',
    period_id = CONCAT('legacy:', term_code),
    term_label = term_code;

ALTER TABLE academic_course_term_statistics
  DROP INDEX uk_academic_course_term_stat,
  ADD UNIQUE INDEX uk_academic_course_term_stat (batch_id, education_level, period_id, course_code),
  DROP INDEX idx_academic_course_stat_lookup,
  ADD INDEX idx_academic_course_stat_lookup (batch_id, education_level, course_code, period_id),
  ALTER COLUMN education_level DROP DEFAULT,
  ALTER COLUMN period_id DROP DEFAULT,
  ALTER COLUMN term_label DROP DEFAULT;

ALTER TABLE academic_instructor_course_term_statistics
  ADD COLUMN education_level VARCHAR(24) NOT NULL DEFAULT '',
  ADD COLUMN period_id VARCHAR(64) NOT NULL DEFAULT '',
  ADD COLUMN term_label VARCHAR(100) NOT NULL DEFAULT '';

UPDATE academic_instructor_course_term_statistics
SET education_level = 'legacy',
    period_id = CONCAT('legacy:', term_code),
    term_label = term_code;

ALTER TABLE academic_instructor_course_term_statistics
  DROP INDEX uk_academic_instructor_course_term_stat,
  ADD UNIQUE INDEX uk_academic_instructor_course_term_stat (batch_id, education_level, period_id, course_code, teacher_key),
  DROP INDEX idx_academic_instructor_stat_lookup,
  ADD INDEX idx_academic_instructor_stat_lookup (batch_id, education_level, course_code, teacher_key, period_id),
  ALTER COLUMN education_level DROP DEFAULT,
  ALTER COLUMN period_id DROP DEFAULT,
  ALTER COLUMN term_label DROP DEFAULT;
