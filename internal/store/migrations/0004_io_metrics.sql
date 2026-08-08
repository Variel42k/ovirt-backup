-- Метрики ввода-вывода: нагрузка на диски ВМ и здоровье путей до хранилища.
--
-- Хранятся уже посчитанные скорости и задержки, а не сырые счётчики. Счётчик
-- сам по себе бесполезен для графика: он монотонно растёт, и чтобы получить из
-- него «сколько сейчас», всё равно нужна пара соседних замеров. Разность
-- считается один раз при сборе, а не каждый раз при отрисовке.
--
-- Таблицы отдельные, потому что вопросы разные: диск отвечает за то, быстро ли
-- работает гостю, монтирование — за то, доходит ли трафик до СХД. Смешивать их
-- в одну «метрику» значит потерять смысл обеих.

CREATE TABLE IF NOT EXISTS disk_samples (
    -- Ключ задаётся явно, как у health_samples: AUTOINCREMENT есть только в
    -- SQLite, SERIAL — только в PostgreSQL, а схема здесь одна на обе СУБД.
    id          BIGINT PRIMARY KEY,
    server_id   TEXT   NOT NULL,
    vm_id       TEXT   NOT NULL,
    vm_name     TEXT   NOT NULL DEFAULT '',
    disk        TEXT   NOT NULL,

    read_bps    BIGINT NOT NULL DEFAULT 0,
    write_bps   BIGINT NOT NULL DEFAULT 0,
    read_iops   BIGINT NOT NULL DEFAULT 0,
    write_iops  BIGINT NOT NULL DEFAULT 0,

    -- Задержка на операцию в микросекундах; -1 — гипервизор её не сообщает.
    read_lat_us  BIGINT NOT NULL DEFAULT -1,
    write_lat_us BIGINT NOT NULL DEFAULT -1,
    flush_lat_us BIGINT NOT NULL DEFAULT -1,

    errors       BIGINT NOT NULL DEFAULT 0,
    errors_delta BIGINT NOT NULL DEFAULT 0,

    at          BIGINT NOT NULL,

    FOREIGN KEY (server_id) REFERENCES servers (id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_disk_samples_lookup ON disk_samples (server_id, vm_id, disk, at DESC);
CREATE INDEX IF NOT EXISTS idx_disk_samples_at ON disk_samples (at);

CREATE TABLE IF NOT EXISTS mount_samples (
    id        BIGINT PRIMARY KEY,
    server_id TEXT NOT NULL,
    kind      TEXT NOT NULL,
    -- target — точка монтирования или IQN цели, source — экспорт или портал.
    target    TEXT NOT NULL,
    source    TEXT NOT NULL DEFAULT '',

    healthy   BOOLEAN NOT NULL DEFAULT TRUE,
    state     TEXT    NOT NULL DEFAULT '',

    operations     BIGINT NOT NULL DEFAULT 0,
    retransmits    BIGINT NOT NULL DEFAULT 0,
    major_timeouts BIGINT NOT NULL DEFAULT 0,
    bad_transfers  BIGINT NOT NULL DEFAULT 0,
    avg_rtt_ms     BIGINT NOT NULL DEFAULT 0,
    avg_exec_ms    BIGINT NOT NULL DEFAULT 0,
    queue_ms       BIGINT NOT NULL DEFAULT 0,
    read_bps       BIGINT NOT NULL DEFAULT 0,
    write_bps      BIGINT NOT NULL DEFAULT 0,

    detail    TEXT   NOT NULL DEFAULT '',
    at        BIGINT NOT NULL,

    FOREIGN KEY (server_id) REFERENCES servers (id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_mount_samples_lookup ON mount_samples (server_id, target, at DESC);
CREATE INDEX IF NOT EXISTS idx_mount_samples_at ON mount_samples (at);
