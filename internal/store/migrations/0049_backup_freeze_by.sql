-- Кто замораживает гостя: служба или движок.
--
-- Служба замораживает гостя до запроса бэкапа и держит заморозку всю фазу
-- initializing — на oVirt это десятки секунд, которых узел Kubernetes не
-- переживает. Движок oVirt 4.4+ умеет замораживать сам, на доли секунды
-- вокруг фиксации точки (require_consistency).
--
-- Пусто — служба: у существующих заданий поведение не меняется.
ALTER TABLE backup_jobs
    ADD COLUMN IF NOT EXISTS freeze_by TEXT NOT NULL DEFAULT ''
        CHECK (freeze_by IN ('', 'service', 'engine'));
