-- Пороги заполнения доменов хранения виртуализации. Раньше они были зашиты в
-- код (предупреждение при 10 % свободного, критично при 5 %). NULL — порог
-- берётся из конфигурации запуска, как и у остальных настроек качества.
ALTER TABLE runtime_settings ADD COLUMN quality_domain_warning_free_percent INTEGER;
ALTER TABLE runtime_settings ADD COLUMN quality_domain_critical_free_percent INTEGER;
