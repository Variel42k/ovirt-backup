-- Для управления Proxmox по HTTPS используется токен API, а для потока vzdump
-- используется отдельный ключ SSH с ограничениями. Идентификатор токена
-- вроде user@pve!backup не подходит как имя пользователя SSH. Общий username
-- побуждал бы использовать root@pam также и для API.
ALTER TABLE servers
    ADD COLUMN IF NOT EXISTS ssh_username TEXT NOT NULL DEFAULT '';
