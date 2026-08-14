-- IANA timezone selected by an administrator in the web interface.
-- NULL keeps scheduler.timezone from YAML or the environment effective.
ALTER TABLE runtime_settings
    ADD COLUMN scheduler_timezone TEXT
    CHECK (scheduler_timezone <> '' AND length(scheduler_timezone) <= 255);
