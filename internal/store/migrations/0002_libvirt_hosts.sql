-- Подключения к голым хостам libvirt/KVM.
--
-- У libvirt нет управляющего движка с REST API: единственный практичный
-- транспорт — SSH до его unix-сокета. Поэтому подключению нужны поля,
-- которых нет у oVirt: адрес хоста, порт, приватный ключ, ключ хоста и
-- каталог под scratch-файлы бэкапа на самом гипервизоре.
--
-- Столбцы добавляются с DEFAULT, поэтому существующие строки oVirt-подключений
-- остаются валидными и ничего переносить не нужно.

ALTER TABLE servers ADD COLUMN ssh_host TEXT NOT NULL DEFAULT '';
ALTER TABLE servers ADD COLUMN ssh_port INTEGER NOT NULL DEFAULT 22;
ALTER TABLE servers ADD COLUMN ssh_private_key_enc TEXT NOT NULL DEFAULT '';
ALTER TABLE servers ADD COLUMN ssh_host_key TEXT NOT NULL DEFAULT '';
ALTER TABLE servers ADD COLUMN scratch_dir TEXT NOT NULL DEFAULT '';
