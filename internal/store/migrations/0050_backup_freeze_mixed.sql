-- Смешанный режим заморозки: замораживает служба, движок подстраховывает.
--
-- 0049 разрешала только service и engine; ограничение пересоздаётся с mixed.
ALTER TABLE backup_jobs DROP CONSTRAINT IF EXISTS backup_jobs_freeze_by_check;
ALTER TABLE backup_jobs ADD CONSTRAINT backup_jobs_freeze_by_check
    CHECK (freeze_by IN ('', 'service', 'engine', 'mixed'));
