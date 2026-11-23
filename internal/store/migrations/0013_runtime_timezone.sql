-- Часовой пояс IANA, выбранный администратором в веб-интерфейсе.
-- При NULL действует scheduler.timezone из YAML или переменной окружения.
ALTER TABLE runtime_settings
    ADD COLUMN scheduler_timezone TEXT
    CHECK (scheduler_timezone <> '' AND length(scheduler_timezone) <= 255);
