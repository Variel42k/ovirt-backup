# Сценарии заморозки для гостевых ВМ

Комплект для уровня согласованности «приложения» (`application`). Ставится
внутрь каждой ВМ с СУБД, а не на сервер копий: qemu-guest-agent вызывает
сценарии перед заморозкой файловых систем. Служба копий команд в госте не
выполняет и о сценариях не знает — она видит только, удалась ли заморозка.

| Файл | Назначение |
|---|---|
| `linux/fsfreeze-hook` | строгий диспетчер вместо штатного: ошибка сценария отменяет заморозку |
| `linux/fsfreeze-hook.d/50-jhvirt-postgresql` | `CHECKPOINT` перед заморозкой |
| `linux/fsfreeze-hook.d/50-jhvirt-mysql` | `FLUSH TABLES WITH READ LOCK` на время заморозки, для MySQL и MariaDB |
| `linux/guest-hooks.conf.example` | порты, тайм-ауты, учётная запись MySQL, цели в подах Kubernetes |
| `linux/install.sh` | установка, проверка без заморозки, удаление |

```bash
sudo sh ./linux/install.sh --postgresql
sudo sh ./linux/install.sh --check
```

Узел Kubernetes: сценарии ставятся так же, а СУБД в подах задаётся в
`/etc/jhvirt/guest-hooks.conf` (`JHVIRT_PG_K8S_TARGETS`,
`JHVIRT_MYSQL_K8S_TARGETS` — `пространство_имён/контейнер`). Контейнер
находится через `crictl` только на этом узле, учётные данные кластера не нужны.
Держите у задания узлов короткий предел заморозки (10–15 с).

Windows: сценарии не нужны — qemu-guest-agent с VSS-провайдером вызывает
VSS-писателей (SQL Server и другие) при каждой заморозке.

Подробно — раздел «Согласованность копий СУБД» в
[docs/OPERATIONS.md](../../docs/OPERATIONS.md).
