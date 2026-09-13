-- Proxmox uses two independent identities: an API token for the HTTPS control
-- plane and a restricted SSH key for the vzdump data stream. Reusing username
-- for both would turn a token id such as user@pve!backup into an invalid SSH
-- login and would encourage operators to use root@pam for the API as well.
ALTER TABLE servers
    ADD COLUMN IF NOT EXISTS ssh_username TEXT NOT NULL DEFAULT '';
