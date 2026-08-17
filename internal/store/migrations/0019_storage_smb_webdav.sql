-- Хранилища по SMB/CIFS и WebDAV.
--
-- До этого сетевую папку Windows или NAS можно было использовать только через
-- монтирование на хосте: администратор правил /etc/fstab, добавлял туда
-- учётные данные и заводил локальное хранилище на точке монтирования. Работает,
-- но настройка живёт вне системы: в интерфейсе видно каталог, а не то, что за
-- ним стоит, и упавшая шара выглядит как ошибка прав на локальный путь.
--
-- Три новых поля. share и domain нужны только SMB: имя сетевой папки и домен
-- Active Directory. Остальное берётся из существующих колонок — host, port,
-- username, password_enc, base_path для SMB и endpoint, base_path, username,
-- password_enc для WebDAV.

ALTER TABLE storage_targets ADD COLUMN IF NOT EXISTS share TEXT NOT NULL DEFAULT '';
ALTER TABLE storage_targets ADD COLUMN IF NOT EXISTS domain TEXT NOT NULL DEFAULT '';

-- Проверку сертификата отключают осознанно и только для WebDAV: у NAS он почти
-- всегда самоподписанный. Значение по умолчанию — проверять.
ALTER TABLE storage_targets ADD COLUMN IF NOT EXISTS insecure_tls BOOLEAN NOT NULL DEFAULT false;

COMMENT ON COLUMN storage_targets.share IS 'SMB: имя сетевой папки, без имени сервера и разделителей';
COMMENT ON COLUMN storage_targets.domain IS 'SMB: домен AD или рабочая группа; пусто — локальная учётная запись сервера';
COMMENT ON COLUMN storage_targets.insecure_tls IS 'WebDAV: не проверять сертификат сервера';
