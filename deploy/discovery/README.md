# Ограниченный поиск приложений внутри гостя

Уровень необязателен: HTTP/TLS и каталог `BACKUPDATA` ищутся без доступа в
гости. Helper добавляет точные данные о контейнерах, systemd, сетевых
монтированиях и заданиях резервного копирования.

На каждой Linux-ВМ:

```sh
install -o root -g root -m 0755 jhvirt-discovery /usr/local/sbin/jhvirt-discovery
useradd --system --create-home --shell /bin/sh jhvirt-discovery
install -d -o jhvirt-discovery -g jhvirt-discovery -m 0700 /home/jhvirt-discovery/.ssh
```

Добавьте публичный ключ в `authorized_keys` одной строкой:

```text
restrict,command="/usr/local/sbin/jhvirt-discovery" ssh-ed25519 AAAA... jhvirt-discovery
```

`restrict` запрещает PTY, проброс портов/агента/X11. Helper принимает только
операцию `probe`, не выполняет аргументы клиента и не изменяет гостя. Для Docker
пользователю потребуется доступ к сокету Docker; это фактически root-доступ,
поэтому безопаснее оставить контейнеры невидимыми либо поставить rootless
Podman. Чтение systemd и `/proc/self/mountinfo` работает без повышения прав.

Соберите ключи хостов заранее и проверьте отпечатки независимо:

```sh
ssh-keyscan -H vm-address >> /etc/jhvirt/discovery_known_hosts
chmod 0600 /etc/jhvirt/discovery_known_hosts /etc/jhvirt/discovery_ed25519
```

Затем заполните `discovery.guest` в конфигурации службы.
