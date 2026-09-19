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
| `linux/guest-hooks.conf.example` | порты, тайм-ауты, учётная запись MySQL |
| `linux/install.sh` | установка, проверка без заморозки, удаление |

```bash
sudo sh ./linux/install.sh --postgresql
sudo sh ./linux/install.sh --check
```

Windows: сценарии не нужны — qemu-guest-agent с VSS-провайдером вызывает
VSS-писателей (SQL Server и другие) при каждой заморозке.

Подробно — раздел «Согласованность копий СУБД» в
[docs/OPERATIONS.md](../../docs/OPERATIONS.md).
