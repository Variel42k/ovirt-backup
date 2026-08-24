-- Итог проверки неизменяемости хранилища.
--
-- Вопрос, на который отвечает проверка: может ли служба стереть уже записанную
-- копию. Если может — то может и тот, кто захватит службу, а именно с удаления
-- копий начинается атака шифровальщиком.
--
-- Хранится итог, а не намерение. Признак object_lock_enabled рядом говорит,
-- чего хотел оператор; эти колонки — что получилось на самом деле, проверенное
-- попыткой записать и удалить.
ALTER TABLE storage_targets
    ADD COLUMN IF NOT EXISTS immutability_state TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS immutability_detail TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS immutability_checked_at TIMESTAMPTZ;
