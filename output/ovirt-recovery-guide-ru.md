# Инструкция по восстановлению запуска ВМ, vGPU, OVN и хранилищ oVirt

Практический разбор работ 5–9 ноября 2026 года. Инструкция предназначена для администратора, который подключается по SSH к engine и гипервизорам.

Применявшийся контур — oVirt 4.3, CentOS 7 / oVirt Node 4.3.9, Python 2.7, oVirt SDK 4. Команды приведены для этого контура. Перед работой в другой версии сверяйте локальные справки утилит, имена устройств, сервисов и объектов API.

Выполняйте блоки по соответствующему симптому и после проверки условий перед ними. Команды ремонта описывают аварийное состояние того инцидента. К 9 ноября этот контур уже восстановлен, поэтому повторное применение изменяющих блоков требует новой диагностики.

## 1. Схема контура и выбор сценария

| Объект | Значение в нашем случае |
|---|---|
| Engine | 10.249.254.127, ovirt.advengineering.ru |
| Гипервизор 03 | adv-hv-03.advengineering.ru, 10.249.254.129 |
| Гипервизор 04 | adv-hv-04.advengineering.ru, 10.249.254.108 |
| Интерфейс хранения хоста 04 | 10.249.249.8 |
| Data Center ID | fe21ed28-e381-11ea-923c-00163e7e996d |
| StdVolume, Storage Domain ID | b4fa552e-4a11-4017-8e54-75f518aaa075 |
| FastVolume, Storage Domain ID | 4c27b63c-6b1d-42b6-b186-9fcc293ae9c5 |
| Локальная ФС StdVolume | /data/storage/stdvolume |
| Устройство StdVolume | /dev/onn_adv-hv-04/StdVolume |
| Thin-pool StdVolume | onn_adv-hv-04/standart_stor |
| Локальная ФС FastVolume | /data/storage/fastvolume |
| Thin-pool FastVolume | onn_adv-hv-04-ssd/fast_stor |

Устройство dm-25 соответствовало StdVolume, dm-20 — FastVolume. Эти номера могут меняться после перезагрузки. Для операций выбирайте проверенное имя LV.

| Симптом | Что проверять | Раздел |
|---|---|---|
| No such file or directory: /sys/class/mdev_bus | Старое свойство mdev_type и наличие GPU на хосте | 5 |
| Failed to communicate with the external provider, NotAfter | Сертификаты OVN, TLS до провайдера и его БД | 6 |
| No free metadata blocks, pool read-only | Data% и Meta% LVM thin-pool | 7 |
| Paused/EIO, XFS forced shutdown, локальный stat возвращает EIO | Локальную ФС сервера NFS, LVM и RAID | 8–10 |
| После xfs_repair пропал файл диска | Снимок до ремонта и восстановление отдельного образа | 11 |
| WaitForLaunch | Подготовку диска VDSM, qemu-img, журнал конкретной ВМ | 12 |
| SSH timeout, имена хостов перестали разрешаться | VPN, маршруты, DNS | 2 |

В инциденте потребовались несколько независимых исправлений. После каждого исправления повторяйте проверку запуска и ищите следующую фактическую ошибку.

## 2. SSH, VPN и подготовка рабочих каталогов

На рабочем компьютере:

~~~powershell
ssh root@10.249.254.127
ssh root@adv-hv-03.advengineering.ru
ssh root@adv-hv-04.advengineering.ru
~~~

При первом подключении сверьте отпечаток SSH-ключа с доверенным источником. Для сохранённых ключей удобно выделить отдельный файл known_hosts.

Если доступ пропал, сначала проверьте Windows:

~~~powershell
rasdial
ipconfig
route print -4
Resolve-DnsName adv-hv-04.advengineering.ru
~~~

В нашем случае профиль adv2 отключился во время сверки образа. Попытка восстановления существующего подключения выглядела так:

~~~powershell
rasdial adv2
~~~

Windows вернула L2TP 789. До восстановления VPN команды SSH к engine и хостам завершались тайм-аутом. Длительная задача копирования была запущена через nohup на гипервизоре и продолжила работу.

После восстановления доступа сначала проверяйте журнал существующей задачи и её результат. Повторный запуск копирования может потребовать ещё 500 ГиБ и затереть контекст диагностики.

На каждом Linux-сервере создайте свой защищённый каталог текущего инцидента:

~~~bash
umask 077
export RECOVERY_DIR="/var/tmp/recovery-$(date +%Y%m%d-%H%M%S)"
install -d -m 700 "$RECOVERY_DIR"
printf '%s\n' "$RECOVERY_DIR"
date
hostname
df -h /var/tmp
~~~

Блоки Bash ниже выполняются в SSH-сеансе соответствующего сервера. Переменные живут в конкретном сеансе. При переподключении восстановите значения путей из журнала работ.

Удобно держать отдельные окна для Bash на engine, интерактивного Python на engine и Bash на каждом гипервизоре. В двух окнах engine задайте одинаковый RECOVERY_DIR, скопировав уже созданный путь. Для выхода из Python используйте exit(). При новом Python-сеансе заново задайте выбранные vm_id, ids и объект domain перед соответствующим разделом.

При передаче Bash-сценария из Windows использовали удаление CR, чтобы окончания строк CRLF не ломали команды. Пример для подготовленного файла step.sh на рабочем компьютере:

~~~powershell
Get-Content -Raw .\step.sh | ssh root@10.249.254.127 "tr -d '\r' | bash -s"
~~~

Для интерактивного ввода пароля SDK откройте обычный SSH-терминал и запустите Python-файл внутри него.

Для длительных задач сохраняйте журнал и код завершения. Пример оболочки для уже подготовленного сценария:

~~~bash
nohup bash -c '
    /root/prepared-recovery-step.sh
    result=$?
    printf "%s\n" "$result" >"$RECOVERY_DIR/step.exit"
    exit "$result"
' >"$RECOVERY_DIR/step.log" 2>&1 </dev/null &
printf '%s\n' "$!" >"$RECOVERY_DIR/step.pid"
~~~

Сначала создайте и проверьте сам prepared-recovery-step.sh. В нём должны быть явные проверки целевого устройства и состояния ФС.

## 3. Первичная диагностика

### 3.1. На engine

~~~bash
systemctl is-active ovirt-engine

grep -E 'VM_DOWN_ERROR|USER_FAILED_RUN_VM|USER_INITIATED_RUN_VM_FAILED|mdev_bus|NotAfter|external provider' \
    /var/log/ovirt-engine/engine.log | tail -n 80
~~~

Сохраните нужный интервал журнала и события конкретной ВМ. Для вывода больших строк с XML ограничивайте длину отображаемой строки:

~~~bash
grep 'ADV-SAM01' /var/log/ovirt-engine/engine.log |
    tail -n 30 | cut -c 1-700
~~~

### 3.2. На обоих гипервизорах

~~~bash
virsh -r list --all
systemctl is-active vdsmd libvirtd nvidia-vgpu-mgr
lspci -nn | grep -Ei '10de|nvidia|vga|3d'
command -v nvidia-smi
ls -ld /sys/class/mdev_bus /sys/bus/mdev

grep -E 'ERROR|No such file.*mdev_bus' /var/log/vdsm/vdsm.log |
    tail -n 30 | cut -c 1-700
~~~

В нашем случае на обоих хостах отсутствовали NVIDIA GPU и подсистема mdev. У ВМ сохранились профили nvidia-259 и nvidia-378.

### 3.3. Проверка диска и хранилища

~~~bash
virsh -r domstate ADV-SAM01 --reason
virsh -r domblkerror ADV-SAM01
virsh -r domblklist ADV-SAM01 --details

findmnt -rn -t nfs,nfs4 -o SOURCE,TARGET,OPTIONS
df -h
lsblk -o NAME,KNAME,TYPE,SIZE,FSTYPE,MOUNTPOINT
lvs -a -o lv_name,vg_name,lv_attr,lv_size,pool_lv,data_percent,metadata_percent,devices
vgs -o vg_name,vg_size,vg_free
dmsetup status

journalctl -k --since '30 minutes ago' --no-pager |
    grep -Ei 'XFS|nfs|thin|I/O|metadata|read-only|shutdown' |
    tail -n 100
~~~

Если NFS возвращает EIO, проверьте тот же файл локально на сервере экспорта. Локальный EIO указывает, что диагностика должна охватывать ФС и устройства сервера.

~~~bash
stat /data/storage/stdvolume
timeout 10 stat /rhev/data-center/mnt/adv-hv-04.advengineering.ru:_data_storage_stdvolume
~~~

Пространство проверяется на трёх уровнях — df для ФС, Data% для thin-pool и Meta% для его метаданных. В нашем FastVolume df показывал свободные 524 ГиБ, а Meta% thin-pool достигла 100%.

Для установленного MegaCLI мы проверяли RAID:

~~~bash
/opt/MegaRAID/MegaCli/MegaCli64 -LDInfo -Lall -aALL |
    grep -E 'Virtual Drive|RAID Level|State|Size|Bad Blocks'

/opt/MegaRAID/MegaCli/MegaCli64 -PDList -aALL |
    grep -E 'Slot Number|Firmware state|Media Error|Other Error|Predictive Failure'
~~~

В инциденте оба массива были Optimal, счётчики ошибок дисков — 0. Исправление выявленного повреждения XFS всё равно потребовалось.

## 4. Определение объектов engine и резервное копирование настроек

### 4.1. SQL только для диагностики

На engine:

~~~bash
/usr/share/ovirt-engine/dbscripts/engine-psql.sh <<'SQL'
SELECT s.vm_name,s.vm_guid,d.status,d.pause_status,
       h.vds_name,left(d.exit_message,250) AS exit_message,
       s.predefined_properties
FROM vm_static s
JOIN vm_dynamic d USING(vm_guid)
LEFT JOIN vds_static h ON h.vds_id=d.run_on_vds
WHERE s.vm_name IN ('ADV-SAM01','ADV-COS7-01','ADV-SAM06','ADV-TC-02-Clone');

SELECT s.vm_name,s.vm_guid,s.predefined_properties,s.userdefined_properties
FROM vm_static s
WHERE s.predefined_properties ILIKE '%mdev%'
   OR s.userdefined_properties ILIKE '%mdev%';

SELECT s.vm_name,e.disk_id,e.is_boot,dev.is_plugged,
       i.image_guid,i.volume_format,i.size,m.storage_domain_id
FROM vm_static s
JOIN disk_vm_element e ON e.vm_id=s.vm_guid
JOIN images i ON i.image_group_id=e.disk_id AND i.active=true
JOIN image_storage_domain_map m ON m.image_id=i.image_guid
LEFT JOIN vm_device dev ON dev.vm_id=e.vm_id AND dev.device_id=e.disk_id
WHERE s.vm_name='ADV-TC-02-Clone';
SQL
~~~

Для этой версии значения vm_dynamic.status 0, 1 и 4 соответствовали Down, Up и Paused. Для принятия решения проверяйте также имена статусов через API и фактическое состояние libvirt.

Чтобы получить все ВМ, использующие проблемный домен, включая дополнительные диски:

~~~bash
/usr/share/ovirt-engine/dbscripts/engine-psql.sh <<'SQL'
SELECT DISTINCT s.vm_name,s.vm_guid,d.status,h.vds_name,sd.storage_name
FROM vm_static s
JOIN vm_dynamic d ON d.vm_guid=s.vm_guid
JOIN disk_vm_element e ON e.vm_id=s.vm_guid
JOIN images i ON i.image_group_id=e.disk_id AND i.active=true
JOIN image_storage_domain_map m ON m.image_id=i.image_guid
JOIN storage_domain_static sd ON sd.id=m.storage_domain_id
LEFT JOIN vds_static h ON h.vds_id=d.run_on_vds
WHERE m.storage_domain_id='b4fa552e-4a11-4017-8e54-75f518aaa075'
ORDER BY s.vm_name;
SQL
~~~

Изменения ВМ и доменов далее выполняются через штатный API. Прямое исправление строк БД может рассогласовать состояние engine и VDSM.

### 4.2. Подготовить интерактивный сеанс SDK на engine

Установленный в нашем engine пакет назывался python-ovirt-engine-sdk4. Проверка:

~~~bash
rpm -q python-ovirt-engine-sdk4
python -c 'import ovirtsdk4; print(ovirtsdk4.__file__)'
~~~

Создайте файл сеанса. Пароль вводится скрыто через getpass:

~~~bash
cat >"$RECOVERY_DIR/api-session.py" <<'PY'
from __future__ import print_function
import atexit
import getpass
import json
import os
import ovirtsdk4
import ovirtsdk4.types as types

base = os.environ['RECOVERY_DIR']
username = raw_input('API login: ').strip()
conn = ovirtsdk4.Connection(
    url='https://ovirt.advengineering.ru/ovirt-engine/api',
    username=username,
    password=getpass.getpass('API password: '),
    ca_file='/etc/pki/ovirt-engine/ca.pem',
    insecure=False,
    debug=False,
    timeout=60,
)
atexit.register(conn.close)
system = conn.system_service()
vms = system.vms_service()

def save_json(filename, value):
    path = os.path.join(base, filename)
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(fd, 'w') as output:
        json.dump(value, output, indent=2)
    print('Saved', path)

def vm_state(vm_id):
    service = vms.vm_service(vm_id)
    vm = service.get()
    disks = service.disk_attachments_service().list()
    nics = service.nics_service().list()
    return {
        'id': vm.id,
        'name': vm.name,
        'status': str(vm.status),
        'host_id': vm.host.id if vm.host else None,
        'custom_properties': [
            {'name': p.name, 'value': p.value}
            for p in vm.custom_properties or []
        ],
        'disks': [
            {'id': a.id, 'disk_id': a.disk.id,
             'active': a.active, 'bootable': a.bootable}
            for a in disks
        ],
        'nics': [
            {'id': n.id, 'name': n.name,
             'linked': n.linked, 'plugged': n.plugged}
            for n in nics
        ],
    }

print('API session ready')
PY

python -i "$RECOVERY_DIR/api-session.py"
~~~

Для доменного входа использовался логин m.filippov@advengineering.ru. Далее блоки с пометкой Python выполняются в этом интерактивном сеансе. Если сеанс закрыт, повторно откройте его с тем же RECOVERY_DIR. Файлы резервных копий создаются с запретом перезаписи.

Идентификаторы ВМ из инцидента:

| ВМ | VM ID |
|---|---|
| ADV-SAM01 | b9fdf390-c0b5-4930-bbf6-032376451101 |
| ADV-COS7-01 | 1fadd1bc-c533-4152-96cf-61baadc1e495 |
| ADV-SAM06 | e7171027-6ef8-45f9-85d8-86897370766a |
| ADV-TC-02-Clone | 1f1c47d8-038f-4b65-8517-a80446348a20 |

Сохранение настроек выбранной ВМ, Python:

~~~python
vm_id = 'b9fdf390-c0b5-4930-bbf6-032376451101'
print(json.dumps(vm_state(vm_id), indent=2))
save_json('vm-' + vm_id + '-before.json', vm_state(vm_id))
~~~

При фактических работах дополнительно сохранены JSON-строки vm_static, vm_dynamic, vm_device и vm_interface для трёх ВМ. Для этого использовались SELECT row_to_json(...) и engine-psql.sh. SQL-копия служила материалом диагностики, изменения затем выполнялись через API.

## 5. Удаление старой привязки vGPU

Этот сценарий применялся к ВМ, перенесённым на хосты без NVIDIA GPU. После удаления профиля ВМ работает с виртуальным видеоадаптером, GPU-ускорение для приложений теряется. Если на целевом хосте есть требуемая GPU, сначала исследуйте драйвер и подсистему mdev.

В oVirt/RHV 4.3 профиль задавался свойством mdev_type. Для версий с отдельными объектами mediated devices процедура отличается. [RHV 4.3, настройка vGPU](https://docs.redhat.com/en/documentation/red_hat_virtualization/4.3/html/setting_up_an_nvidia_gpu_for_a_virtual_machine_in_red_hat_virtualization/assigning_virtual_gpus)

В сеансе Python после сохранения настроек выбранной ВМ:

~~~python
service = vms.vm_service(vm_id)
vm = service.get()
assert vm.status == types.VmStatus.DOWN
print([(p.name, p.value) for p in vm.custom_properties or []])

kept = [p for p in vm.custom_properties or [] if p.name != 'mdev_type']
service.update(types.Vm(custom_properties=kept))

updated = service.get()
assert all(p.name != 'mdev_type' for p in updated.custom_properties or [])
print([(p.name, p.value) for p in updated.custom_properties or []])
service.start()
~~~

Для трёх ВМ операция была выполнена отдельно. Фильтрация сохраняет остальные custom properties.

После запуска на хосте, который выбрал engine:

~~~bash
virsh -r domstate ADV-SAM01 --reason
virsh -r domblkerror ADV-SAM01
virsh -r dumpxml ADV-SAM01 | grep -E 'mdev|model type=.qxl'
~~~

Ожидаются running, отсутствие ошибок диска, виртуальный видеоадаптер и отсутствие hostdev типа mdev. При проверке использовалась фактическая конфигурация запущенного домена.

Если требуется возврат сохранённого свойства, остановите ВМ и в Python восстановите custom properties из резервной копии:

~~~python
service = vms.vm_service(vm_id)
assert service.get().status == types.VmStatus.DOWN
saved = json.load(open(os.path.join(base, 'vm-' + vm_id + '-before.json')))
properties = [
    types.CustomProperty(name=p['name'], value=p['value'])
    for p in saved['custom_properties']
]
service.update(types.Vm(custom_properties=properties))
~~~

Повторный запуск с vGPU требует хоста с соответствующим аппаратным обеспечением и драйвером.

## 6. Просроченные сертификаты OVN

### 6.1. Подтверждение причины

На engine:

~~~bash
date
systemctl is-active ovirt-provider-ovn ovn-northd

grep -E 'NotAfter|external provider' /var/log/ovirt-engine/engine.log |
    tail -n 30 | cut -c 1-700

for name in ovirt-provider-ovn ovn-ndb ovn-sdb; do
    printf '%s\n' "$name"
    openssl x509 -in "/etc/pki/ovirt-engine/certs/$name.cer" \
        -noout -dates -subject -issuer
done

openssl x509 -in /etc/pki/ovirt-engine/ca.pem -noout -dates
~~~

В инциденте три сертификата истекли 26 июля 2025 года, CA оставался действующим до 2030 года. Ошибка запуска ADV-SAM06 содержала NotAfter.

Проверка соответствия CSR, действовавшего сертификата и существующего закрытого ключа для каждого имени:

~~~bash
set -euo pipefail
name=ovirt-provider-ovn

cert_pub=$(openssl x509 -in "/etc/pki/ovirt-engine/certs/$name.cer" \
    -pubkey -noout | openssl pkey -pubin -outform DER | sha256sum | cut -d ' ' -f 1)

req_pub=$(openssl req -in "/etc/pki/ovirt-engine/requests/$name.req" \
    -pubkey -noout | openssl pkey -pubin -outform DER | sha256sum | cut -d ' ' -f 1)

key_pub=$(openssl pkey -in "/etc/pki/ovirt-engine/keys/$name.key.nopass" \
    -pubout -outform DER | sha256sum | cut -d ' ' -f 1)

test "$cert_pub" = "$req_pub"
test "$cert_pub" = "$key_pub"
~~~

Повторите для ovn-ndb и ovn-sdb. Если ключи не совпадают или CSR отсутствует, подготовьте корректный запрос для существующего ключа по процедуре вашей установки.

### 6.2. Резервная копия и перевыпуск

В защищённый каталог сохраните сертификаты, CSR и контейнеры PKCS12:

~~~bash
umask 077
tar -cf "$RECOVERY_DIR/ovn-pki-before.tar" -C /etc/pki/ovirt-engine \
    certs/ovirt-provider-ovn.cer certs/ovn-ndb.cer certs/ovn-sdb.cer \
    requests/ovirt-provider-ovn.req requests/ovn-ndb.req requests/ovn-sdb.req \
    keys/ovirt-provider-ovn.p12 keys/ovn-ndb.p12 keys/ovn-sdb.p12
~~~

Проверьте локальную справку установленного скрипта. В этом контуре он подписывает существующий CSR. [Исходный код oVirt pki-enroll-request.sh](https://github.com/oVirt/ovirt-engine/blob/master/packaging/bin/pki-enroll-request.sh)

~~~bash
/usr/share/ovirt-engine/bin/pki-enroll-request.sh --help
openssl x509 -in /etc/pki/ovirt-engine/certs/ovirt-provider-ovn.cer \
    -noout -subject -text
~~~

Использованный Subject был /C=US/O=advengineering.ru/CN=ovirt.advengineering.ru. В этом инциденте все три сертификата имели такой Subject и SAN DNS:ovirt.advengineering.ru. Для другого контура возьмите его фактические значения.

Важная особенность выполненных работ — первая попытка с ошибочно разобранной строкой Subject завершилась ошибкой и оставила выходной сертификат пустым. Файл сразу восстановлен из резервной копии, затем подпись повторена с явным Subject. Передача строки с префиксом subject= вместо DN, начинающегося с /, недопустима для этого вызова.

Ниже используется пароль существующего PKCS12. Контейнеры в нашем контуре открывались со штатным значением oVirt mypass. Пароль API и пароль PKCS12 относятся к разным операциям.

~~~bash
set -euo pipefail
read -rsp 'Existing PKCS12 password: ' PKCS12_PASS
printf '\n'
export PKCS12_PASS

days=1095
openssl x509 -in /etc/pki/ovirt-engine/ca.pem \
    -checkend "$((days * 86400))" -noout

for name in ovirt-provider-ovn ovn-ndb ovn-sdb; do
    openssl pkcs12 -in "/etc/pki/ovirt-engine/keys/$name.p12" \
        -passin env:PKCS12_PASS -noout

    cp -p "/etc/pki/ovirt-engine/certs/$name.cer" \
        "$RECOVERY_DIR/$name.cer.before"

    if ! /usr/share/ovirt-engine/bin/pki-enroll-request.sh \
        --name="$name" \
        --subject="/C=US/O=advengineering.ru/CN=ovirt.advengineering.ru" \
        --days="$days" \
        --san=DNS:ovirt.advengineering.ru \
        >"$RECOVERY_DIR/enroll-$name.log" 2>&1; then
        cp -p "$RECOVERY_DIR/$name.cer.before" \
            "/etc/pki/ovirt-engine/certs/$name.cer"
        restorecon "/etc/pki/ovirt-engine/certs/$name.cer"
        echo "Signing failed; previous certificate restored"
        exit 1
    fi

    openssl verify -CAfile /etc/pki/ovirt-engine/ca.pem \
        "/etc/pki/ovirt-engine/certs/$name.cer"

    openssl pkcs12 -export \
        -in "/etc/pki/ovirt-engine/certs/$name.cer" \
        -inkey "/etc/pki/ovirt-engine/keys/$name.key.nopass" \
        -out "/etc/pki/ovirt-engine/keys/$name.p12" \
        -passout env:PKCS12_PASS

    chmod 600 "/etc/pki/ovirt-engine/keys/$name.p12"
    restorecon "/etc/pki/ovirt-engine/certs/$name.cer" \
        "/etc/pki/ovirt-engine/keys/$name.p12"
    openssl x509 -in "/etc/pki/ovirt-engine/certs/$name.cer" -noout -dates
done

unset PKCS12_PASS
~~~

Срок 1095 дней выбрали с учётом оставшегося срока CA. Перед следующим перевыпуском пересчитайте его. В инциденте новые сертификаты были действительны до 4 ноября 2029 года.

Если ошибка возникла после изменения части файлов, завершите восстановление согласованного комплекта из защищённой копии либо исправьте конкретный шаг до перезапуска сервисов.

### 6.3. Подхват сертификатов и проверки

На установленной версии OVN управляющие сокеты находились в /var/run/openvswitch. Сначала проверьте их наличие:

~~~bash
ls /var/run/openvswitch/ovnnb_db.ctl /var/run/openvswitch/ovnsb_db.ctl

ovs-appctl -t /var/run/openvswitch/ovnnb_db.ctl ovsdb-server/reconnect
ovs-appctl -t /var/run/openvswitch/ovnsb_db.ctl ovsdb-server/reconnect

systemctl restart ovirt-provider-ovn
systemctl is-active ovirt-provider-ovn ovn-northd
~~~

При reconnect обновляются соединения управляющих БД, поэтому выполняйте его в согласованное окно работ.

~~~bash
ovn-nbctl --timeout=10 --db=ssl:127.0.0.1:6641 \
    -p /etc/pki/ovirt-engine/keys/ovn-ndb.key.nopass \
    -c /etc/pki/ovirt-engine/certs/ovn-ndb.cer \
    -C /etc/pki/ovirt-engine/ca.pem show

openssl s_client -connect ovirt.advengineering.ru:9696 \
    -servername ovirt.advengineering.ru \
    -CAfile /etc/pki/ovirt-engine/ca.pem </dev/null 2>&1 |
    grep -E 'subject=|issuer=|Verify return'

curl -sS --cacert /etc/pki/ovirt-engine/ca.pem \
    -o /dev/null -w '%{http_code}\n' \
    https://ovirt.advengineering.ru:9696/
~~~

В нашем случае проверка цепочки вернула 0 (ok), curl без авторизации получил HTTP 401, запрос OVN Northbound через SSL выполнился. Первый curl сразу после перезапуска ещё не подключился, следующий подтвердил готовность.

Проверьте также сроки сертификатов хостов, если журнал указывает на них. В нашем случае сертификаты VDSM обоих хостов были действующими:

~~~bash
openssl x509 -in /etc/pki/vdsm/certs/vdsmcert.pem -noout -dates
~~~

## 7. Thin-pool ушёл в read-only из-за заполнения метаданных

### 7.1. Подтверждение и копия конфигурации

На хосте 04:

~~~bash
lvs -a -o lv_name,vg_name,lv_attr,lv_size,pool_lv,data_percent,metadata_percent
vgs -o vg_name,vg_size,vg_free
dmsetup status onn_adv--hv--04--ssd-fast_stor-tpool

journalctl -k --no-pager |
    grep -Ei 'thin:.*metadata|No free metadata|switching pool to read-only' |
    tail -n 20

vgcfgbackup -f "$RECOVERY_DIR/ssd-vg-before.vg" onn_adv-hv-04-ssd
vgcfgbackup -f "$RECOVERY_DIR/std-vg-before.vg" onn_adv-hv-04

lvs -a -o lv_name,vg_name,lv_attr,lv_size,data_percent,metadata_percent \
    >"$RECOVERY_DIR/lvs-before.txt"
dmsetup status >"$RECOVERY_DIR/dmsetup-before.txt"
~~~

vgcfgbackup сохраняет конфигурацию LVM. Содержимое дисков ВМ и метаданные отображения блоков thin-pool требуют собственных копий.

В данном случае Meta% была 100%, Data% — 99.44%, dmsetup показывал ro. В VG имелось 1.63 ГиБ свободных extents. Предварительный расчёт и затем фактическое расширение:

~~~bash
lvextend --test --poolmetadatasize +1G onn_adv-hv-04-ssd/fast_stor

lvextend --poolmetadatasize +1G onn_adv-hv-04-ssd/fast_stor

lvs -o lv_name,lv_attr,lv_size,data_percent,metadata_percent \
    onn_adv-hv-04-ssd/fast_stor
dmsetup status onn_adv--hv--04--ssd-fast_stor-tpool
~~~

--test проверяет расчёт LVM без применения. Успешный расчёт сам по себе не подтверждает исправность работающего пула.

Если Data% также достигла 100%, обеспечьте свободные физические блоки пула или подготовьте расширение его области данных. Расширение только метаданных не создаёт дополнительной ёмкости для данных ВМ. Конкретный размер расширения выбирайте по фактически свободным extents VG и потребностям пула.

В нашем случае метаданные увеличились с 1.63 до 2.63 ГиБ, Meta% снизилась до 62%, пул перешёл в rw. Если после расширения остаются ro, needs_check или ошибки thin metadata, остановитесь и подготовьте отдельное офлайн-восстановление thin-pool. Эта ветка в инциденте не выполнялась.

LVM разделяет заполнение данных и метаданных thin-pool. Оба показателя требуют контроля до достижения 100%. [Справка lvmthin](https://www.man7.org/linux/man-pages/man7/lvmthin.7.html)

### 7.2. Проверка записи и возврат свободных блоков

На хосте 04 выполните короткую проверку на ФС FastVolume:

~~~bash
python - <<'PY'
import os
import tempfile

mount = '/data/storage/fastvolume'
fd, path = tempfile.mkstemp(prefix='.recovery-probe-', dir=mount)
try:
    os.write(fd, b'recovery-write-check\n' * 200)
    os.fsync(fd)
finally:
    os.close(fd)

os.unlink(path)
directory = os.open(mount, os.O_RDONLY)
try:
    os.fsync(directory)
finally:
    os.close(directory)
print('write/fsync/unlink OK')
PY
~~~

Затем:

~~~bash
fstrim -v /data/storage/fastvolume
lvs -o lv_name,data_percent,metadata_percent onn_adv-hv-04-ssd/fast_stor
~~~

fstrim передаёт информацию о свободных блоках этой файловой системы нижележащему устройству. Переходите к нему после восстановления записи и проверки работоспособности ФС.

В нашем случае освобождено 520.4 ГиБ, Data% снизилась до 84.24%, Meta% — примерно до 52.60%. Остановка ВМ FastVolume для этой операции не потребовалась.

## 8. Подготовка общего StdVolume к офлайн-ремонту

Поводом были сообщения XFS о повреждении метаданных и принудительной остановке ФС. Ошибка затронула несколько ВМ, включая машины на втором гипервизоре.

Перед изменениями определите всех потребителей домена по запросу раздела 4.1. Проверьте также вложенные NFS-экспорты и клиентов вне этого engine. В инциденте на StdVolume было восемь активных или приостановленных ВМ — ADV-SAM01, ADV-SAM02, AESYN2, ICCTM-DC2, NVIDIA-LICSRV, PG1cRestore, VM-CRM1C и WEB-SRV1.

Остановка и ремонт общего домена выполняются в согласованное окно. Для приостановленных ВМ отключение питания теряет несохранённое состояние памяти.

### 8.1. Сохранить состояния и остановить потребителей через engine

В Python-сеансе раздела 4.2 задайте список из текущего SQL-запроса. Для нашего инцидента:

~~~python
ids = [
    'b9fdf390-c0b5-4930-bbf6-032376451101',
    'a7b27f13-8094-4156-b6ca-98d7bcb157c4',
    '8da28c6e-d8e9-43b9-9ebe-45914c10988b',
    '3a506e2b-fef9-4a36-b13d-a5d75f292d76',
    '0d202e5f-78ac-47b5-9604-aaa4dbcb7630',
    'f9b86234-acb9-4b63-92ff-1de3bd3bb351',
    'c255bb1c-7052-40ce-a2e3-4f9882e2d61d',
    '803d4686-9b6e-4c15-9090-6eeeaa728a4b',
]
states_before = [vm_state(i) for i in ids]
save_json('domain-vms-before.json', states_before)

for i in ids:
    service = vms.vm_service(i)
    vm = service.get()
    if vm.status == types.VmStatus.UP:
        service.shutdown(async_=True)
    elif vm.status == types.VmStatus.PAUSED:
        service.stop(async_=True)

~~~

После запроса дождитесь выключения и повторно получите состояния:

~~~python
print([(vms.vm_service(i).get().name, str(vms.vm_service(i).get().status)) for i in ids])
~~~

Если штатное выключение не завершилось из-за недоступного диска, после согласования отключения питания выполните для конкретной ВМ:

~~~python
vms.vm_service('a7b27f13-8094-4156-b6ca-98d7bcb157c4').stop(async_=True)
~~~

Перед обслуживанием домена обязательна повторная проверка:

~~~python
assert all(vms.vm_service(i).get().status == types.VmStatus.DOWN for i in ids)
~~~

### 8.2. Деактивировать домен

Python:

~~~python
dc_id = 'fe21ed28-e381-11ea-923c-00163e7e996d'
sd_id = 'b4fa552e-4a11-4017-8e54-75f518aaa075'
domain = (
    system.data_centers_service().data_center_service(dc_id)
    .storage_domains_service().storage_domain_service(sd_id)
)
print(domain.get().status)
domain.deactivate(async_=True)
~~~

В инциденте обычная деактивация не завершилась из-за невозможности записать OVF на недоступную ФС. После подтверждения этой причины был использован:

~~~python
assert all(vms.vm_service(i).get().status == types.VmStatus.DOWN for i in ids)
domain.deactivate(async_=True, force=True)
~~~

Параметр force разрешает деактивацию при неудачном обновлении OVF. Он имеет конкретный смысл в этой операции API. [RHV 4.3 REST API, deactivate](https://docs.redhat.com/en/documentation/red_hat_virtualization/4.3/pdf/rest_api_guide/Red_Hat_Virtualization-4.3-REST_API_Guide-en-US.pdf)

После ответа API проверьте окончательное состояние и аудит. Асинхронный ответ означает принятие запроса.

На engine:

~~~bash
/usr/share/ovirt-engine/dbscripts/engine-psql.sh <<'SQL'
SELECT storage_id,status
FROM storage_pool_iso_map
WHERE storage_id='b4fa552e-4a11-4017-8e54-75f518aaa075';

SELECT log_time,log_type_name,left(message,350)
FROM audit_log
WHERE storage_domain_id='b4fa552e-4a11-4017-8e54-75f518aaa075'
ORDER BY log_time DESC LIMIT 12;
SQL
~~~

На каждом гипервизоре:

~~~bash
findmnt -rn -t nfs,nfs4 -o SOURCE,TARGET |
    grep 'adv-hv-04.advengineering.ru:_data_storage_stdvolume'

sanlock client status | grep b4fa552e
virsh -r list --all
~~~

При успешно отключённом домене соответствующие NFS-монтирования исчезли с обоих хостов, аренды sanlock этого домена отсутствовали.

### 8.3. Снять экспорты только ремонтируемой ФС

На сервере хранения adv-hv-04:

~~~bash
cp -a /etc/exports "$RECOVERY_DIR/exports-before"
cp -a /var/lib/nfs/etab "$RECOVERY_DIR/etab-before"
exportfs -v
ss -nt state established '( sport = :2049 )'
findmnt -R /data/storage/stdvolume
~~~

Посмотрите /var/lib/nfs/etab и выберите основной экспорт вместе с его вложенными экспортами. В нашем случае команды были следующими:

~~~bash
exportfs -u '10.249.254.90:/data/storage/stdvolume/export_domain'
exportfs -u '10.249.254.51:/data/storage/stdvolume/export_domain'

exportfs -u '10.249.254.129/32:/data/storage/stdvolume/hostedeng'
exportfs -u '10.249.254.108/32:/data/storage/stdvolume/hostedeng'
exportfs -u '10.249.249.0/24:/data/storage/stdvolume/hostedeng'

exportfs -u '10.249.254.129/32:/data/storage/stdvolume'
exportfs -u '10.249.254.108/32:/data/storage/stdvolume'
exportfs -u '10.249.249.0/24:/data/storage/stdvolume'

umount /data/storage/stdvolume
~~~

Для другого контура этот перечень составляется заново. Включение FastVolume или другого домена в такой перечень расширяет окно недоступности.

Если umount вернул target is busy, выясните причину:

~~~bash
findmnt -R /data/storage/stdvolume
cat /proc/locks
sanlock client status
exportfs -v
cat /proc/fs/nfsd/exports
~~~

Для поиска открытых пользовательских файлов можно посмотреть ссылки /proc без обхода повреждённого дерева:

~~~bash
python - <<'PY'
import glob
import os

prefixes = [
    '/data/storage/stdvolume',
    '/rhev/data-center/mnt/adv-hv-04.advengineering.ru:_data_storage_stdvolume',
]
paths = glob.glob('/proc/[0-9]*/fd/*') + glob.glob('/proc/[0-9]*/cwd')
for path in paths:
    try:
        target = os.readlink(path)
        if any(target == p or target.startswith(p + '/') for p in prefixes):
            pid = path.split('/')[2]
            name = open('/proc/' + pid + '/comm').read().strip()
            print(pid, name, path, target)
    except (OSError, IOError):
        pass
PY
~~~

В инциденте после снятия экспортов оставалась NFS-аренда ядра. Проверялась очистка кэша командой exportfs -f. Обычное размонтирование удалось после освобождения аренды. Эта команда затрагивает общий кэш экспортов сервера, поэтому её применимость оценивается отдельно. [Справка exportfs](https://www.man7.org/linux/man-pages/man8/exportfs.8.html)

Условие перехода к ремонту — обычное umount завершилось успешно и проверка mountpoint подтверждает отсутствие монтирования:

~~~bash
if mountpoint -q /data/storage/stdvolume; then
    echo 'STOP: filesystem is still mounted'
    exit 1
fi
~~~

Принудительное или ленивое размонтирование может оставить активные ссылки на ФС. Для xfs_repair требуется действительно освобождённое устройство.

## 9. Сохранение и ремонт XFS

### 9.1. Проверить устройство и сохранить исходное состояние

На adv-hv-04:

~~~bash
export STD_LV=/dev/onn_adv-hv-04/StdVolume
export STD_MOUNT=/data/storage/stdvolume

lsblk -o NAME,TYPE,SIZE,FSTYPE,MOUNTPOINT "$STD_LV"
findmnt -rn -S "$STD_LV"
lvs -a -o lv_name,lv_attr,lv_size,pool_lv,origin,data_percent,metadata_percent \
    onn_adv-hv-04
~~~

Перед модифицирующими шагами обеспечьте сохранение исходных данных. В нашем случае перед xfs_repair создан read-only thin-снимок:

~~~bash
export SNAP_NAME=StdVolume_before_repair_20261105
export SNAP_LV="/dev/onn_adv-hv-04/$SNAP_NAME"

vgcfgbackup -f "$RECOVERY_DIR/std-vg-before-snapshot.vg" onn_adv-hv-04

lvcreate --snapshot --name "$SNAP_NAME" onn_adv-hv-04/StdVolume
lvchange --permission r "onn_adv-hv-04/$SNAP_NAME"

lvs -a -o lv_name,lv_attr,lv_size,pool_lv,origin,data_percent,metadata_percent \
    onn_adv-hv-04
~~~

Для следующего инцидента задайте новое имя снимка. Проверьте Data% и Meta% до создания и при последующих записях. Снимок хранит исходные блоки в том же пуле, изменение origin расходует дополнительное место.

Фактическая хронология была такой — metadump, штатное воспроизведение журнала, повторная диагностика, затем thin-снимок перед ремонтом. Для повторной работы полезно сохранить снимок уже после освобождения устройства и до воспроизведения журнала, если состояние и ёмкость пула это позволяют. Так сохраняется более ранняя точка.

Диагностическая копия метаданных:

~~~bash
xfs_db -r -c sb -c 'p blocksize agcount agblocks icount logblocks' "$STD_LV"

xfs_metadump -g "$STD_LV" "$RECOVERY_DIR/std-before.metadump" \
    >"$RECOVERY_DIR/std-metadump.log" 2>&1
~~~

Файл metadump размещайте на другой исправной ФС. В инциденте он занимал около 2.2 ГиБ в /var/tmp. Такой файл сохраняет метаданные XFS, содержимое дисков ВМ требует снимка или полноценной копии.

### 9.2. Немодифицирующая проверка и грязный журнал

~~~bash
if xfs_repair -n -m 4096 "$STD_LV" >"$RECOVERY_DIR/std-check.log" 2>&1; then
    result=0
else
    result=$?
fi
printf '%s\n' "$result" >"$RECOVERY_DIR/std-check.exit"
tail -n 40 "$RECOVERY_DIR/std-check.log"
~~~

Условный вызов сохраняет код результата даже при включённом set -e. -m 4096 ограничивает используемую память утилиты примерно 4 ГиБ.

Если журнал содержит незавершённые операции, проверка -n может показывать несогласованности до их воспроизведения. В инциденте журнал штатно воспроизведён ядром:

~~~bash
mount /data/storage/stdvolume
umount /data/storage/stdvolume
~~~

На этом этапе экспорты оставались снятыми, ВМ — выключенными. После чистого размонтирования проверку повторили:

~~~bash
if xfs_repair -n -m 4096 "$STD_LV" \
    >"$RECOVERY_DIR/std-post-replay-check.log" 2>&1; then
    result=0
else
    result=$?
fi
printf '%s\n' "$result" >"$RECOVERY_DIR/std-post-replay-check.exit"
~~~

xfs_repair требует размонтированной ФС. Для штатного воспроизведения грязного журнала используется mount с последующим umount. -L принудительно очищает журнал и может потерять незавершённые изменения. При неудаче штатного replay остановитесь и отдельно оцените возможность потери данных. В инциденте -L не потребовался. [Справка xfs_repair](https://www.man7.org/linux/man-pages/man8/xfs_repair.8.html)

### 9.3. Определить файлы, которые ремонт может удалить

Ищите в журнале:

~~~bash
grep -Ei 'would have cleared inode|bad data fork|would.*junk|corrupt|duplicate' \
    "$RECOVERY_DIR/std-post-replay-check.log"
~~~

Мы нашли inode 530. Диагностика:

~~~bash
xfs_db -r -c 'inode 530' \
    -c 'p core.mode core.size core.nextents core.nblocks' "$STD_LV"

xfs_ncheck -i 530 "$STD_LV" >"$RECOVERY_DIR/inode-530-path.txt" 2>&1
~~~

Сохраняйте полный вывод xfs_ncheck — предупреждения могут предшествовать имени файла.

Сопоставление найденного имени тома с ВМ на engine:

~~~bash
/usr/share/ovirt-engine/dbscripts/engine-psql.sh <<'SQL'
SELECT s.vm_name,i.image_guid,i.image_group_id,i.size,i.active
FROM images i
LEFT JOIN disk_vm_element e ON e.disk_id=i.image_group_id
LEFT JOIN vm_static s ON s.vm_guid=e.vm_id
WHERE i.image_guid='dca9158c-e984-4d69-af9c-52fe0f335f66';
SQL
~~~

Так установили, что inode относится к системному диску ADV-TC-02-Clone на 500 ГиБ. До ремонта его исходные блоки были сохранены в thin-снимке.

### 9.4. Ремонт и обязательный контрольный проход

Переходите к этому шагу после сохранения данных, проверки списка затронутых ВМ и подтверждения отсутствия монтирования.

~~~bash
if mountpoint -q "$STD_MOUNT"; then
    echo 'STOP: filesystem is mounted'
    exit 1
fi

if xfs_repair -m 4096 "$STD_LV" >"$RECOVERY_DIR/std-repair.log" 2>&1; then
    result=0
else
    result=$?
fi
printf '%s\n' "$result" >"$RECOVERY_DIR/std-repair.exit"
~~~

При ненулевом результате исследуйте журнал до дальнейших действий.

Затем:

~~~bash
test "$(cat "$RECOVERY_DIR/std-repair.exit")" = 0

if xfs_repair -n -m 4096 "$STD_LV" >"$RECOVERY_DIR/std-verify.log" 2>&1; then
    result=0
else
    result=$?
fi
printf '%s\n' "$result" >"$RECOVERY_DIR/std-verify.exit"
tail -n 35 "$RECOVERY_DIR/std-verify.log"
~~~

В нашем случае оба прохода завершились с кодом 0. Ремонт очистил повреждённый inode 530 и удалил запись образа клона. Остальные потребители домена затем были запущены, а образ клона восстановлен отдельно по разделу 11.

## 10. Возврат домена и ВМ в работу

На сервере хранения:

~~~bash
test "$(cat "$RECOVERY_DIR/std-verify.exit")" = 0
mount /data/storage/stdvolume
findmnt /data/storage/stdvolume
~~~

Выполните проверку write/fsync/unlink из раздела 7.2, заменив mount на /data/storage/stdvolume.

Проверьте, что конфигурация экспортов соответствует сохранённой, затем восстановите экспорты:

~~~bash
cmp /etc/exports "$RECOVERY_DIR/exports-before"
exportfs -ra
exportfs -v
~~~

Если cmp обнаружил отличия, сначала разберите их. exportfs -ra применяет всю текущую конфигурацию экспортов сервера.

В Python-сеансе engine:

~~~python
domain.activate(async_=True)
~~~

Дождитесь успешной активации и проверьте доступ на обоих хостах:

~~~bash
findmnt -rn -t nfs,nfs4 -o SOURCE,TARGET |
    grep 'adv-hv-04.advengineering.ru:_data_storage_stdvolume'

timeout 10 stat \
    /rhev/data-center/mnt/adv-hv-04.advengineering.ru:_data_storage_stdvolume/b4fa552e-4a11-4017-8e54-75f518aaa075/dom_md/metadata
~~~

В Python запускайте ВМ по сохранённому списку состояний:

~~~python
assert domain.get().status == types.StorageDomainStatus.ACTIVE
saved = json.load(open(os.path.join(base, 'domain-vms-before.json')))

for old in saved:
    service = vms.vm_service(old['id'])
    if old['status'] != 'down' and service.get().status == types.VmStatus.DOWN:
        service.start(async_=True)

~~~

Каждый запуск проверяйте по разделу 12. В инциденте восемь ВМ StdVolume вернулись в Up.

## 11. Восстановление отдельного образа из thin-снимка

### 11.1. Условия и открытие снимка

ВМ должна быть выключена. Исходный образ ищется в снимке до ремонта, а восстановленный файл создаётся на исправной рабочей ФС.

Для ADV-TC-02-Clone использовались:

| Параметр | Значение |
|---|---|
| VM ID | 1f1c47d8-038f-4b65-8517-a80446348a20 |
| Disk ID, каталог images | 39d16783-2edc-47fa-8034-88996f1008b4 |
| Volume ID, имя файла | dca9158c-e984-4d69-af9c-52fe0f335f66 |
| Размер | 536870912000 байт, 500 ГиБ |
| Формат | RAW |
| Дополнительный диск | 5d948ca3-238d-47f7-b69a-7f4e09f9b492, 400 ГиБ |

На сервере хранения:

~~~bash
export SNAP_NAME=StdVolume_before_repair_20261105
export SNAP_LV="/dev/onn_adv-hv-04/$SNAP_NAME"
export SNAP_MOUNT="$RECOVERY_DIR/snapshot-ro"
install -d -m 700 "$SNAP_MOUNT"

lvchange --activate y --ignoreactivationskip "onn_adv-hv-04/$SNAP_NAME"
mount -o ro,norecovery,nouuid "$SNAP_LV" "$SNAP_MOUNT"
findmnt "$SNAP_MOUNT"

export RELATIVE_VOLUME=b4fa552e-4a11-4017-8e54-75f518aaa075/images/39d16783-2edc-47fa-8034-88996f1008b4/dca9158c-e984-4d69-af9c-52fe0f335f66
export SOURCE="$SNAP_MOUNT/$RELATIVE_VOLUME"
export FINAL="/data/storage/stdvolume/$RELATIVE_VOLUME"

stat -c 'bytes=%s inode=%i mode=%a owner=%U:%G' "$SOURCE"
file -s "$SOURCE"
cat "$FINAL.meta"
~~~

ro открывает ФС для чтения, norecovery подавляет воспроизведение её журнала, nouuid позволяет открыть снимок с совпадающим UUID. Применимость такого открытия зависит от состояния снимка. В нашем снимке журнал уже был штатно воспроизведён перед его созданием.

Если снимок появился ещё до replay и состояние дерева не позволяет прочитать файл, сохраните исходный read-only снимок и подготовьте отдельную рабочую копию для дальнейшего исследования.

В нашем случае сохранились файлы .meta и .lease исходного тома. Формат в .meta был RAW, LEGALITY — LEGAL, CAP — 536870912000, PUUID — нулевой UUID. Сценарий ниже рассчитан на самостоятельный RAW-образ. Для QCOW2 с backing chain требуется восстановление и проверка всей цепочки.

Проверка чтения отдельных участков, Bash с pipefail:

~~~bash
set -o pipefail
for gb in 0 125 250 375 499; do
    printf '%s GiB\n' "$gb"
    timeout 20 dd if="$SOURCE" bs=1M skip=$((gb * 1024)) count=1 status=none |
        sha256sum
done
~~~

Результат команды с тайм-аутом нужно проверять по коду возврата. Хеш пустого потока также выглядит корректной строкой SHA-256.

Дополнительно мы проверили файловые системы и обнаружение Windows через libguestfs:

~~~bash
LIBGUESTFS_BACKEND=direct \
LIBGUESTFS_CACHEDIR="$RECOVERY_DIR" \
LIBGUESTFS_TMPDIR="$RECOVERY_DIR" \
timeout -k 10 180 guestfish --ro --format=raw -a "$SOURCE" \
    run : list-filesystems : inspect-os
~~~

Получили два раздела NTFS и Windows на /dev/sda2. Открытие --ro и явное указание raw соответствуют документированным параметрам guestfish. [Справка guestfish](https://libguestfs.org/guestfish.1.html)

### 11.2. Копирование в отдельный файл и полная сверка

Сначала оцените запас места:

~~~bash
df -h /data/storage/stdvolume
lvs --units b --nosuffix -o lv_name,lv_size,data_percent,metadata_percent \
    onn_adv-hv-04/standart_stor
~~~

Новая копия на том же thin-pool может потребовать до полного объёма файла. Снимок продолжит удерживать старые блоки. В нашем случае до копирования было занято около 80.6% данных пула, после — около 86.1%.

Подготовьте новые пути. Каталог для промежуточного образа должен отсутствовать:

~~~bash
export THIN_POOL=onn_adv-hv-04/standart_stor
export EXPECTED_BYTES=536870912000
export DEST="/data/storage/stdvolume/.clone-recovery-$(date +%Y%m%d-%H%M%S)/image.raw"
~~~

Ниже обобщённый вариант применённого сценария. Он создаёт файл с запретом перезаписи, останавливается на ошибке чтения, ведёт прогресс и сравнивает хеш при чтении источника с хешем полного повторного чтения копии. Порог 91% данных / 90% метаданных выбран для этого инцидента. Перед повторной работой задайте более подходящие пределы с учётом пула и остальных ВМ.

~~~bash
cat >"$RECOVERY_DIR/copy-verified.py" <<'PY'
from __future__ import print_function
import ctypes
import hashlib
import os
import subprocess
import time

source = os.environ['SOURCE']
destination = os.environ['DEST']
pool = os.environ['THIN_POOL']
size = int(os.environ['EXPECTED_BYTES'])
work = os.environ['RECOVERY_DIR']
chunk_size = 8 * 1024**2
zeros = b'\0' * chunk_size

def log(message):
    print(time.strftime('%H:%M:%S'), message)
    import sys
    sys.stdout.flush()

def check_pool():
    output = subprocess.check_output([
        'lvs', '--noheadings', '-o', 'data_percent,metadata_percent', pool,
    ], close_fds=True)
    data, metadata = map(float, output.split())
    if data >= 91 or metadata >= 90:
        raise RuntimeError('Pool limit reached: %.2f / %.2f' % (data, metadata))
    return data, metadata

def main():
    assert os.path.isfile(source)
    assert os.stat(source).st_size == size
    assert not os.path.exists(destination)
    assert not os.path.exists(os.path.join(work, 'verified.sha256'))
    check_pool()
    os.mkdir(os.path.dirname(destination), 0o700)

    expected = hashlib.sha256()
    position = 0
    started = last = time.time()
    with open(source, 'rb', 0) as src:
        fd = os.open(destination, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        with os.fdopen(fd, 'wb', 0) as dst:
            while position < size:
                block = src.read(min(chunk_size, size - position))
                if not block:
                    raise IOError('Unexpected EOF at %d' % position)
                expected.update(block)
                if block == zeros[:len(block)]:
                    dst.seek(len(block), os.SEEK_CUR)
                else:
                    dst.write(block)
                position += len(block)
                if time.time() - last >= 30:
                    os.fsync(dst.fileno())
                    data, metadata = check_pool()
                    log('copy %.2f GiB, %.1f MiB/s, pool %.2f / %.2f' % (
                        position / float(1024**3),
                        position / (time.time() - started) / 1024**2,
                        data, metadata))
                    last = time.time()
            dst.truncate(size)
            os.fsync(dst.fileno())
            libc = ctypes.CDLL('libc.so.6')
            advise = libc.posix_fadvise64
            advise.argtypes = [
                ctypes.c_int, ctypes.c_longlong, ctypes.c_longlong, ctypes.c_int,
            ]
            result = advise(dst.fileno(), 0, 0, 4)
            if result:
                log('Cache eviction advisory returned %d' % result)

    log('Source SHA256 ' + expected.hexdigest())
    actual = hashlib.sha256()
    position = 0
    last = time.time()
    with open(destination, 'rb', 0) as dst:
        while True:
            block = dst.read(chunk_size)
            if not block:
                break
            actual.update(block)
            position += len(block)
            if time.time() - last >= 30:
                check_pool()
                log('verify %.2f GiB' % (position / float(1024**3)))
                last = time.time()

    assert position == size
    assert expected.hexdigest() == actual.hexdigest(), 'SHA256 mismatch'
    marker = os.path.join(work, 'verified.sha256')
    fd = os.open(marker, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(fd, 'w') as output:
        output.write(expected.hexdigest() + '  ' + destination + '\n')
        output.flush()
        os.fsync(output.fileno())
    log('Copy verified: %d bytes, SHA256 %s' % (size, expected.hexdigest()))

if __name__ == '__main__':
    main()
PY

python -m py_compile "$RECOVERY_DIR/copy-verified.py"

nohup ionice -c2 -n7 nice -n 10 \
    python "$RECOVERY_DIR/copy-verified.py" \
    >"$RECOVERY_DIR/copy.log" 2>&1 </dev/null &
printf '%s\n' "$!" >"$RECOVERY_DIR/copy.pid"
~~~

Контроль:

~~~bash
tail -n 15 "$RECOVERY_DIR/copy.log"
ps -p "$(cat "$RECOVERY_DIR/copy.pid")" -o pid,etime,state,wchan:25,comm
lvs -o lv_name,data_percent,metadata_percent onn_adv-hv-04/standart_stor
~~~

Восстановление и полная сверка 500 ГиБ заняли около четырёх часов. После обрыва VPN задача завершилась на хосте. Её результат подтверждён по последней строке Copy verified и файлу verified.sha256.

При ошибке чтения, несовпадении хеша или достижении порога пула публикация не выполняется. Промежуточный файл сохраняется для разбора. Следующую попытку готовьте с отдельным каталогом и явным планом обращения с незавершённой копией.

### 11.3. Вернуть файл под прежний ID тома

Перед публикацией на engine повторно проверьте, что ВМ Down. Если целевой файл появился за время копирования, сначала установите его происхождение.

На сервере хранения, в том же Bash-сеансе с SOURCE, DEST, FINAL и RECOVERY_DIR:

~~~bash
test -s "$RECOVERY_DIR/verified.sha256"
test -f "$FINAL.meta"
test -f "$FINAL.lease"

python - <<'PY'
import os
import re
import stat

work = os.environ['RECOVERY_DIR']
source = os.environ['DEST']
target = os.environ['FINAL']
marker = os.path.join(work, 'verified.sha256')
checksum, recorded_path = open(marker).read().strip().split(None, 1)
assert recorded_path == source
assert re.match('^[0-9a-f]{64}$', checksum)
assert 'Copy verified:' in open(os.path.join(work, 'copy.log')).read()

info = os.stat(source)
assert stat.S_ISREG(info.st_mode)
assert info.st_size == int(os.environ['EXPECTED_BYTES'])
assert info.st_mtime <= os.stat(marker).st_mtime
assert not os.path.lexists(target)

metadata = dict(
    line.strip().split('=', 1)
    for line in open(target + '.meta') if '=' in line
)
assert metadata['FORMAT'] == 'RAW'
assert metadata['LEGALITY'] == 'LEGAL'
assert int(metadata['CAP']) == info.st_size

owner = os.stat(target + '.lease')
os.chown(source, owner.st_uid, owner.st_gid)
os.chmod(source, 0o660)

os.link(source, target)
directory = os.open(os.path.dirname(target), os.O_RDONLY)
try:
    os.fsync(directory)
finally:
    os.close(directory)

with open(os.path.join(work, 'published.txt'), 'w') as output:
    output.write(target + '\n' + checksum + '\n')
print('Published', target)
PY

restorecon "$FINAL"
ls -lZ "$FINAL"
~~~

os.link требует, чтобы промежуточный файл и целевой путь находились на одной файловой системе. При существующей цели операция завершается ошибкой. Если копия размещена на другом хранилище, подготовьте отдельную процедуру переноса и атомарной публикации.

На гипервизоре через NFS от имени vdsm:

~~~bash
runuser -u vdsm -- timeout 20 qemu-img info -U -f raw --output=json \
    /rhev/data-center/mnt/adv-hv-04.advengineering.ru:_data_storage_stdvolume/b4fa552e-4a11-4017-8e54-75f518aaa075/images/39d16783-2edc-47fa-8034-88996f1008b4/dca9158c-e984-4d69-af9c-52fe0f335f66
~~~

Ожидаемый размер — 536870912000, формат raw. qemu-img info подтверждает доступность и параметры файла. Полное совпадение байтов установлено предыдущей сверкой.

### 11.4. Проверить подключение дисков и запустить клон

Python на engine:

~~~python
clone_id = '1f1c47d8-038f-4b65-8517-a80446348a20'
clone = vms.vm_service(clone_id)
assert clone.get().status == types.VmStatus.DOWN
save_json('clone-before-boot.json', vm_state(clone_id))

attachments = clone.disk_attachments_service().list()
print([(a.id, a.disk.id, a.active, a.bootable) for a in attachments])
~~~

Выяснилось, что оба диска были отключены пользователем 30 октября. Наличие attachment в списке не означало active=true.

Для проверки загрузки подключили именно восстановленный системный диск:

~~~python
boot = next(
    a for a in attachments
    if a.disk.id == '39d16783-2edc-47fa-8034-88996f1008b4'
)
assert boot.bootable

original = vms.vm_service('6aebe12e-0026-4e10-93f3-b9ea365ddd30').get()
assert original.status == types.VmStatus.DOWN

if not boot.active:
    clone.disk_attachments_service().attachment_service(boot.id).update(
        types.DiskAttachment(active=True)
    )

clone.start()
~~~

Проверка исходной ВМ была дополнительным условием для запуска клона в этой ситуации. Для своей инфраструктуры проверьте также адреса гостевых ОС и назначение клона.

Дополнительный диск данных на 400 ГиБ в итоговой конфигурации имеет active=false. Его подключение требует решения администратора по назначению ВМ и использованию данных.

### 11.5. Завершить техническую очистку

После успешной публикации и проверки доступа удалите только промежуточную ссылку на тот же inode:

~~~bash
python - <<'PY'
import os
stage = os.environ['DEST']
target = os.environ['FINAL']
assert os.path.samefile(stage, target)
os.unlink(stage)
os.rmdir(os.path.dirname(stage))
PY
~~~

Дополнительная ссылка указывает на тот же изменяемый файл. После запуска ВМ её содержимое также изменялось бы вместе с основным томом.

Закройте чтение снимка:

~~~bash
umount "$SNAP_MOUNT"
lvchange --activate n "onn_adv-hv-04/$SNAP_NAME"
lvs -o lv_name,lv_attr,origin "onn_adv-hv-04/$SNAP_NAME"
~~~

Защитный read-only снимок в выполненных работах сохранён. Для его последующего удаления сначала подтвердите полноту восстановления и наличие нужных независимых копий.

## 12. Проверка запуска и задержка WaitForLaunch

### 12.1. Проверить engine, libvirt и реальный ввод-вывод

На выбранном engine хосте:

~~~bash
virsh -r domstate ADV-TC-02-Clone --reason
virsh -r domblkerror ADV-TC-02-Clone
virsh -r domblkstat ADV-TC-02-Clone sda
~~~

Имя диска sda или vda берите из domblklist. Повторите domblkstat через некоторое время и проверьте изменение счётчиков.

На engine:

~~~bash
/usr/share/ovirt-engine/dbscripts/engine-psql.sh <<'SQL'
SELECT s.vm_name,d.status,d.pause_status,h.vds_name,d.vm_ip,
       left(d.exit_message,200) AS exit_message
FROM vm_static s
JOIN vm_dynamic d USING(vm_guid)
LEFT JOIN vds_static h ON h.vds_id=d.run_on_vds
WHERE s.vm_name IN ('ADV-SAM01','ADV-COS7-01','ADV-SAM06','ADV-TC-02-Clone');

SELECT log_time,vm_name,log_type_name,left(message,300)
FROM audit_log
WHERE log_time>now()-interval '10 minutes'
  AND log_type_name IN (
    'VM_DOWN_ERROR','USER_FAILED_RUN_VM',
    'VM_PAUSED_EIO','VM_PAUSED_ENOSPC'
  )
ORDER BY log_time DESC;
SQL
~~~

В первом цикле ADV-SAM01 перешла из Up в Paused спустя примерно две минуты из-за аварии StdVolume. Поэтому проверка включает несколько минут работы, отсутствие новых ошибок и завершение загрузки гостя.

### 12.2. Если ВМ застряла в WaitForLaunch

На хосте найдите события её VDSM-потока. Для WEB-SRV1:

~~~bash
grep '(vm/803d4686)' /var/log/vdsm/vdsm.log |
    tail -n 35 | cut -c 1-700

ps -eo pid,ppid,etime,stat,wchan:30,args |
    grep '[q]emu-img'

tail -n 30 /var/log/libvirt/qemu/WEB-SRV1.log | cut -c 1-700
virsh -r domjobinfo WEB-SRV1
~~~

В нашем случае подготовка остановилась на qemu-img info, читавшем образ через NFS. Сама ВМ ещё не выполнялась в QEMU. Для диагностики проверяли доступность файла, состояние потоков процесса и журнал ядра.

Для выбранного вспомогательного процесса:

~~~bash
pid=37817
ps -p "$pid" -o pid,ppid,etime,stat,wchan:30,args
ls -l "/proc/$pid/fd"

for thread in /proc/"$pid"/task/*; do
    printf '%s\n' "$thread"
    cat "$thread/wchan"
    cat "$thread/stack"
done
~~~

PID 37817 относится к конкретной прошлой попытке. Получите актуальный PID и подтвердите командную строку до диагностики.

Чтение заголовка можно сравнить локально на NFS-сервере и через NFS. Используйте timeout и pipefail, сохраняйте коды возврата. Одновременно проверьте XFS и thin-pool на новые ошибки.

В нашем случае ожидание завершилось, подготовка продолжилась и WEB-SRV1 запустилась. Принудительное завершение qemu-img или перезапуск VDSM/libvirt для этого запуска не выполнялись.

### 12.3. Проверить гостевую ОС и консоль

Гостевой агент сообщил Windows и IP клона 10.249.254.172. С engine проверили:

~~~bash
ping -c 2 -W 2 10.249.254.172

if timeout 4 bash -c 'exec 3<>/dev/tcp/10.249.254.172/3389'; then
    echo 'RDP reachable'
fi
~~~

Снимок консоли на гипервизоре:

~~~bash
install -d -m 700 /var/tmp/vm-console-check

LIBVIRT_AUTH_FILE=/etc/ovirt-hosted-engine/virsh_auth.conf \
timeout 20 virsh -c qemu:///system screenshot ADV-TC-02-Clone \
    /var/tmp/vm-console-check/console.ppm </dev/null
~~~

Для screenshot использовалась существующая локальная авторизация libvirt. Вызов через virsh -r был отклонён как операция, недоступная read-only соединению. Файл virsh_auth.conf содержит параметры локальной авторизации, его содержимое в журнал работ не выводилось.

Кадр можно забрать через scp. Для отображения в PNG на рабочем компьютере, где установлен Pillow:

~~~powershell
scp root@adv-hv-03.advengineering.ru:/var/tmp/vm-console-check/console.ppm .
python -c "from PIL import Image; Image.open('console.ppm').save('console.png')"
~~~

На первых кадрах Windows сначала показывала чёрный экран, затем Applying computer settings. В конце появился экран блокировки с Ctrl+Alt+Delete.

## 13. Что считать завершением восстановления

Для каждой ВМ подтвердите одновременно:

- Engine сообщает Up и показывает фактический хост запуска.
- Libvirt сообщает running, domblkerror возвращает No errors found.
- Счётчики диска подтверждают чтение и запись.
- Гостевой агент и консоль подтверждают загрузку ОС.
- Доступность нужного сетевого сервиса проверена.
- За период проверки в engine и журнале ядра отсутствуют новые ошибки, относящиеся к восстановлению.
- Фактическая конфигурация подключений дисков и NIC соответствует требуемому назначению ВМ.

Для ADV-TC-02-Clone итог проверен 9 ноября 2026 года:

| Проверка | Результат |
|---|---|
| Образ системного диска | Восстановлены все 500 ГиБ |
| SHA-256 до первого запуска | cc298e76af4c5d4c3db9bdfe48f96db111ddb104066a8f410e27e236aa9abad2 |
| Engine | Up |
| Хост | adv-hv-03.advengineering.ru |
| Гость | Windows, экран блокировки |
| IP | 10.249.254.172 |
| ICMP / RDP | Доступны |
| Ошибки диска | Отсутствуют |
| Системный диск | Подключён |
| Дополнительный диск 400 ГиБ | Отключён в engine |
| Защитный снимок | Сохранён, read-only, деактивирован |

После загрузки ОС системный диск принимает записи, поэтому его текущий хеш закономерно отличается от зафиксированного хеша до запуска.

## 14. Где остались материалы фактических работ

На engine в /var/tmp/vgpu-start-recovery-20261105/:

- before.json и api-custom-properties-before.json с исходными настройками.
- ovn-pki-before.tar с прежними сертификатами, CSR и PKCS12.
- stdvolume-vm-states-before-stop.json с состояниями восьми ВМ.
- clone-disk-states-before.json с исходными подключениями дисков клона.
- recover.py, storage-control.py и clone-start.py с операциями, ограниченными конкретным инцидентом.

На adv-hv-04 в /var/tmp/vgpu-storage-recovery-20261105/:

- Конфигурации VG до изменений и снимка.
- exports-before, etab-before, перечень снятых экспортов.
- stdvolume-before.metadump.
- Журналы диагностик, ремонта XFS и коды завершения.
- adv-tc-02-clone/copy.py, copy.log, verified.sha256 и published.txt.

Защитный снимок находится по пути /dev/onn_adv-hv-04/StdVolume_before_repair_20261105. Исходные сценарии содержат фиксированные UUID и пути этого инцидента. Для нового случая сначала составьте актуальный перечень объектов.

Эта инструкция обобщает выполненные действия. Приведённые интерактивные примеры и сценарий копирования адаптированы для повторного использования. Их синтаксис проверен при подготовке документа, изменяющие операции на production повторно ради документации не выполнялись.




