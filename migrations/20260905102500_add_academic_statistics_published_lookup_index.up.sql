-- module: academic_statistics
ALTER TABLE academic_statistics_batches
  ADD INDEX idx_academic_statistics_batch_published_rule (status, rule_version, published_at, id);
