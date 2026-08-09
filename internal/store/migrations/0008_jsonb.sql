-- JSON перестаёт быть просто текстом: TEXT -> JSONB.
--
-- Хранить JSON в TEXT было следствием поддержки SQLite, где типа для него нет.
-- Цена этого решения — не место на диске, а тишина при порче: код читает такие
-- поля через json.Unmarshal с проигнорированной ошибкой, потому что упасть на
-- разборе списка ВМ хуже, чем показать пустой список. В итоге испорченное
-- значение не отличается от пустого: задание, потерявшее список машин, выглядит
-- как задание, у которого его и не было.
--
-- JSONB это закрывает на входе: PostgreSQL отвергает некорректный JSON при
-- записи, и до чтения битое значение просто не доходит.
--
-- Две группы колонок и разное обращение с ними:
--
--   1. Списки и объекты с DEFAULT '[]' или '{}' — уже корректный JSON,
--      преобразуются прямым приведением.
--
--   2. Поля отчётов с DEFAULT '' — пустая строка JSON-ом не является, и
--      приведение на ней падает. Пустое значение переводится в JSON null:
--      так сохраняется NOT NULL, а читающий код уже умеет обращаться с "null"
--      как с отсутствующим значением, и править его не нужно.

-- Группа 1: списки и объекты.
ALTER TABLE servers      ALTER COLUMN tags               DROP DEFAULT;
ALTER TABLE servers      ALTER COLUMN tags               TYPE JSONB USING tags::jsonb;
ALTER TABLE servers      ALTER COLUMN tags               SET DEFAULT '[]'::jsonb;

ALTER TABLE vms          ALTER COLUMN ip_addresses       DROP DEFAULT;
ALTER TABLE vms          ALTER COLUMN ip_addresses       TYPE JSONB USING ip_addresses::jsonb;
ALTER TABLE vms          ALTER COLUMN ip_addresses       SET DEFAULT '[]'::jsonb;

ALTER TABLE disks        ALTER COLUMN vm_ids             DROP DEFAULT;
ALTER TABLE disks        ALTER COLUMN vm_ids             TYPE JSONB USING vm_ids::jsonb;
ALTER TABLE disks        ALTER COLUMN vm_ids             SET DEFAULT '[]'::jsonb;

ALTER TABLE backup_jobs  ALTER COLUMN vm_ids             DROP DEFAULT;
ALTER TABLE backup_jobs  ALTER COLUMN vm_ids             TYPE JSONB USING vm_ids::jsonb;
ALTER TABLE backup_jobs  ALTER COLUMN vm_ids             SET DEFAULT '[]'::jsonb;

ALTER TABLE backup_jobs  ALTER COLUMN cluster_ids        DROP DEFAULT;
ALTER TABLE backup_jobs  ALTER COLUMN cluster_ids        TYPE JSONB USING cluster_ids::jsonb;
ALTER TABLE backup_jobs  ALTER COLUMN cluster_ids        SET DEFAULT '[]'::jsonb;

ALTER TABLE backup_jobs  ALTER COLUMN tags               DROP DEFAULT;
ALTER TABLE backup_jobs  ALTER COLUMN tags               TYPE JSONB USING tags::jsonb;
ALTER TABLE backup_jobs  ALTER COLUMN tags               SET DEFAULT '[]'::jsonb;

ALTER TABLE backup_jobs  ALTER COLUMN exclude_vm_ids     DROP DEFAULT;
ALTER TABLE backup_jobs  ALTER COLUMN exclude_vm_ids     TYPE JSONB USING exclude_vm_ids::jsonb;
ALTER TABLE backup_jobs  ALTER COLUMN exclude_vm_ids     SET DEFAULT '[]'::jsonb;

ALTER TABLE backup_jobs  ALTER COLUMN exclude_disk_ids   DROP DEFAULT;
ALTER TABLE backup_jobs  ALTER COLUMN exclude_disk_ids   TYPE JSONB USING exclude_disk_ids::jsonb;
ALTER TABLE backup_jobs  ALTER COLUMN exclude_disk_ids   SET DEFAULT '[]'::jsonb;

ALTER TABLE backup_jobs  ALTER COLUMN storage_target_ids DROP DEFAULT;
ALTER TABLE backup_jobs  ALTER COLUMN storage_target_ids TYPE JSONB USING storage_target_ids::jsonb;
ALTER TABLE backup_jobs  ALTER COLUMN storage_target_ids SET DEFAULT '[]'::jsonb;

ALTER TABLE backup_jobs  ALTER COLUMN retention          DROP DEFAULT;
ALTER TABLE backup_jobs  ALTER COLUMN retention          TYPE JSONB USING retention::jsonb;
ALTER TABLE backup_jobs  ALTER COLUMN retention          SET DEFAULT '{}'::jsonb;

ALTER TABLE restore_runs ALTER COLUMN disk_ids           DROP DEFAULT;
ALTER TABLE restore_runs ALTER COLUMN disk_ids           TYPE JSONB USING disk_ids::jsonb;
ALTER TABLE restore_runs ALTER COLUMN disk_ids           SET DEFAULT '[]'::jsonb;

-- Группа 2: отчёты, где пустая строка означала «ничего нет».
--
-- alerts.details сюда намеренно не входит: несмотря на похожее имя, там лежит
-- свободный текст («состояние: paused, причина паузы: eio»), а не JSON.
-- Перевод его в JSONB отвергал бы каждое оповещение.
ALTER TABLE verify_runs          ALTER COLUMN details       DROP DEFAULT;
ALTER TABLE verify_runs          ALTER COLUMN details       TYPE JSONB
    USING COALESCE(NULLIF(details, '')::jsonb, 'null'::jsonb);
ALTER TABLE verify_runs          ALTER COLUMN details       SET DEFAULT 'null'::jsonb;

ALTER TABLE remediation_periods  ALTER COLUMN summary       DROP DEFAULT;
ALTER TABLE remediation_periods  ALTER COLUMN summary       TYPE JSONB
    USING COALESCE(NULLIF(summary, '')::jsonb, 'null'::jsonb);
ALTER TABLE remediation_periods  ALTER COLUMN summary       SET DEFAULT 'null'::jsonb;

ALTER TABLE backup_runs          ALTER COLUMN skipped_disks DROP DEFAULT;
ALTER TABLE backup_runs          ALTER COLUMN skipped_disks TYPE JSONB
    USING COALESCE(NULLIF(skipped_disks, '')::jsonb, 'null'::jsonb);
ALTER TABLE backup_runs          ALTER COLUMN skipped_disks SET DEFAULT 'null'::jsonb;
