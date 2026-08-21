# DNS для oVirt и KVM

Приложение должно разрешать DNS-имена движка oVirt, гипервизоров и KVM-хостов
с той машины, где запущен процесс. Для Docker имя должно разрешаться и на
Docker-host, и внутри контейнера.

Типичная ошибка:

```text
dial tcp: lookup dengine.example.local on 127.0.0.11:53: server misbehaving
```

`127.0.0.11` — нормальный встроенный DNS Docker, а не адрес корпоративного
DNS. Docker принимает запрос контейнера и пересылает его резолверам host.

## 1. Определите уровень ошибки

Проверка на host:

```bash
getent ahostsv4 dengine.example.local
resolvectl query dengine.example.local
```

Проверка внутри контейнера:

```bash
cd <каталог-compose>
docker compose exec -T ovirt-backup \
  getent ahostsv4 dengine.example.local
```

Интерпретация:

| Host | Контейнер | Причина |
|---|---|---|
| не разрешает | не разрешает | DNS или route-domain host-системы |
| разрешает | не разрешает | Docker DNS; проверьте `/etc/resolv.conf` контейнера или задайте Compose override |
| разрешает | разрешает | DNS исправен; проверяйте маршрут, порт, TLS и учётные данные |

Посмотреть фактические DNS-серверы:

```bash
resolvectl status
cat /run/systemd/resolve/resolv.conf
```

Проверить корпоративный DNS напрямую, минуя системный resolver:

```bash
dig +time=2 +tries=1 +short @10.0.0.53 \
  dengine.example.local A
```

Если прямой запрос возвращает IP, а `resolvectl query` нет, проблема находится
в маршрутизации DNS-зоны на host, а не в записи DNS.

## 2. Почему ломается корпоративный `.local`

Зона `.local` зарезервирована для multicast DNS. `systemd-resolved` может не
отправлять такое имя обычному unicast DNS, даже если DHCP выдал правильные
DNS-серверы. Типичный результат:

```text
resolve call failed: No appropriate name servers or networks for name found
```

Для корпоративной зоны задайте DNS route-domain. Запись
`~pish.example.local` означает: все запросы этой зоны направлять DNS-серверам
конкретного сетевого интерфейса. Символ `~` важен: это маршрут, а не суффикс,
который будет дописываться к коротким именам.

Используйте наиболее узкую фактическую зону. Не направляйте весь `~local` в
корпоративный DNS без явного решения сетевого администратора.

## 3. Временная проверка через `resolvectl`

Определите интерфейс маршрута по умолчанию:

```bash
IFACE="$(ip -4 route show default | awk '{print $5; exit}')"
printf '%s\n' "$IFACE"
```

Добавьте route-domain до перезагрузки или переподнятия интерфейса:

```bash
sudo resolvectl domain "$IFACE" '~pish.example.local'
resolvectl domain "$IFACE"
resolvectl query dengine.pish.example.local
```

Если после этого имя разрешается на host и в контейнере, закрепите настройку
через сетевой backend ОС.

## 4. Постоянная настройка Ubuntu и Netplan

Не редактируйте `/etc/resolv.conf`: на Ubuntu это сгенерированный файл или
ссылка на stub `systemd-resolved`, и изменение пропадёт.

Посмотрите имя интерфейса и выданные DNS:

```bash
ip -4 route show default
resolvectl status
```

Создайте отдельный override, чтобы не менять файл установщика ОС:

```bash
sudoedit /etc/netplan/90-corporate-dns.yaml
```

Пример:

```yaml
network:
  version: 2
  ethernets:
    enp6s0:
      nameservers:
        addresses:
          - 10.0.0.53
          - 10.0.0.54
        search:
          - "~pish.example.local"
```

Имя `enp6s0`, адреса DNS и зону замените фактическими значениями. Ограничьте
права и проверьте объединённую конфигурацию:

```bash
sudo chmod 600 /etc/netplan/90-corporate-dns.yaml
sudo netplan generate
sudo netplan get ethernets.enp6s0.nameservers
```

`netplan generate` только проверяет и генерирует backend-конфигурацию. Для
применения выполните из консоли либо в окно, когда кратковременная потеря SSH
допустима:

```bash
sudo netplan try
# после подтверждения либо вместо try:
sudo netplan apply
```

Проверка постоянного результата:

```bash
resolvectl domain enp6s0
resolvectl query dengine.pish.example.local
sudo systemctl restart systemd-resolved
resolvectl query dengine.pish.example.local
```

После плановой перезагрузки повторите последние две DNS-проверки и проверку
из контейнера.

## 5. Постоянная настройка RHEL, AlmaLinux и Rocky Linux

На системах с NetworkManager определите интерфейс и имя connection profile:

```bash
IFACE="$(ip -4 route show default | awk '{print $5; exit}')"
CONNECTION="$(nmcli -g GENERAL.CONNECTION device show "$IFACE")"
printf 'interface=%s connection=%s\n' "$IFACE" "$CONNECTION"
```

Добавьте DNS и route-domain, не удаляя существующие значения:

```bash
sudo nmcli connection modify "$CONNECTION" \
  +ipv4.dns "10.0.0.53,10.0.0.54" \
  +ipv4.dns-search "~pish.example.local"
sudo nmcli connection up "$CONNECTION"
```

Проверка:

```bash
nmcli -f IP4.DNS,IP4.DOMAIN device show "$IFACE"
resolvectl query dengine.pish.example.local
```

Если DNS уже корректно приходит по DHCP, достаточно добавить только
`+ipv4.dns-search`.

## 6. Docker-only override

Этот вариант используйте, только если host разрешает имя либо его сетевую
конфигурацию нельзя менять. Создайте `docker-compose.override.yml` рядом с
основным Compose-файлом:

```yaml
services:
  ovirt-backup:
    dns:
      - 10.0.0.53
      - 10.0.0.54
```

Compose v1 и v2 автоматически читают этот файл при запуске из того же
каталога. Пересоздайте приложение:

```bash
docker compose up -d --force-recreate ovirt-backup
docker compose exec -T ovirt-backup \
  getent ahostsv4 dengine.example.local
```

Для Compose v1 используйте `docker-compose`.

## 7. Проверка сети после DNS

DNS-проверка подтверждает только получение IP. Проверьте нужный порт из того
же окружения, где работает приложение:

```bash
docker compose exec -T ovirt-backup \
  wget -S --spider --no-check-certificate --timeout=10 \
  https://dengine.example.local/ovirt-engine/sso/oauth/token
```

Ответ HTTP `400`, `401` или `405` без корректных параметров SSO подтверждает,
что DNS, маршрут и TCP 443 работают. Это ещё не подтверждает логин и доверие
сертификату.

Не заменяйте hostname движка IP-адресом как постоянное исправление:

- сертификат TLS обычно выпущен на DNS-имя;
- SSO может возвращать абсолютные URL с hostname;
- смена IP снова потребует изменения подключения.

## 8. Пример постоянной настройки на сервере

Адреса здесь — из диапазона, отведённого стандартом под документацию
(RFC 5737). Подставьте свои.

Для постоянной настройки используется файл:

```text
/etc/netplan/90-jhvirt-corporate-dns.yaml
```

Его актуальное содержимое:

```yaml
network:
  version: 2
  ethernets:
    enp6s0:
      nameservers:
        addresses:
          - 203.0.113.12
          - 203.0.113.2
          - 203.0.113.22
        search:
          - "~pish.example.local"
```

Контрольные команды:

```bash
sudo netplan get ethernets.enp6s0.nameservers
resolvectl domain enp6s0
resolvectl query dengine.pish.example.local

cd /home/user/ovirt-backup/deploy
sudo docker compose exec -T ovirt-backup \
  getent ahostsv4 dengine.pish.example.local
```

Обе команды обязаны вернуть один и тот же адрес движка. Разошлись — значит
контейнер разрешает имя не через те DNS-серверы, что хост: смотрите раздел 2.

## 9. DNS при переносе приложения

Installer переносит конфигурацию и может изменить `--url`, но DNS/NAT он не
меняет. Для cutover заранее уменьшите TTL, выпустите TLS с SAN итогового имени
и добавьте новый callback во внешний OIDC provider. После импорта проверьте имя
из браузера и со стороны сервера, затем переключите запись и не запускайте
старый экземпляр против общей PostgreSQL.

Если имя остаётся прежним, сохранённый сертификат можно использовать только
после успешной SAN-проверки установщика. Если меняется — выберите новый
self-signed или PEM через installer. Полная последовательность:
[DEPLOY.md](DEPLOY.md#перенос-приложения-на-другой-сервер).

## 10. Официальные справочники

- [Netplan: YAML configuration](https://netplan.readthedocs.io/en/stable/netplan-yaml/)
- [systemd-resolved: `Domains=` и route-only domains](https://www.freedesktop.org/software/systemd/man/latest/resolved.conf.html)
- [NetworkManager: `ipv4.dns-search`](https://www.networkmanager.dev/docs/api/latest/nm-settings-nmcli.html)
