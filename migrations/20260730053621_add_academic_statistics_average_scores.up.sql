-- module: academic_statistics
ALTER TABLE academic_course_term_statistics ADD COLUMN numeric_score_count BIGINT NOT NULL;
ALTER TABLE academic_course_term_statistics ADD COLUMN numeric_score_sum_x100 BIGINT NOT NULL;
ALTER TABLE academic_instructor_course_term_statistics ADD COLUMN numeric_score_count BIGINT NOT NULL;
ALTER TABLE academic_instructor_course_term_statistics ADD COLUMN numeric_score_sum_x100 BIGINT NOT NULL;
