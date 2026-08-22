-- module: academic_statistics
-- v5 can contain the same legacy term_code/course_code in multiple education
-- levels. Remove those batches before restoring the narrower legacy keys.
DELETE FROM academic_statistics_batches WHERE rule_version = 'v5';

ALTER TABLE academic_instructor_course_term_statistics
  DROP INDEX uk_academic_instructor_course_term_stat,
  ADD UNIQUE INDEX uk_academic_instructor_course_term_stat (batch_id, term_code, course_code, teacher_key),
  DROP INDEX idx_academic_instructor_stat_lookup,
  ADD INDEX idx_academic_instructor_stat_lookup (batch_id, course_code, teacher_key, term_code),
  DROP COLUMN term_label,
  DROP COLUMN period_id,
  DROP COLUMN education_level;

ALTER TABLE academic_course_term_statistics
  DROP INDEX uk_academic_course_term_stat,
  ADD UNIQUE INDEX uk_academic_course_term_stat (batch_id, term_code, course_code),
  DROP INDEX idx_academic_course_stat_lookup,
  ADD INDEX idx_academic_course_stat_lookup (batch_id, course_code, term_code),
  DROP COLUMN term_label,
  DROP COLUMN period_id,
  DROP COLUMN education_level;
