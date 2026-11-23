from __future__ import print_function

import json
import os
import sys

import ovirtsdk4
import ovirtsdk4.types as types

BASE = '/var/tmp/vgpu-start-recovery-20261105'
POOL = 'fe21ed28-e381-11ea-923c-00163e7e996d'
DOMAIN = 'b4fa552e-4a11-4017-8e54-75f518aaa075'
TARGETS = [
    ('ADV-SAM01', 'b9fdf390-c0b5-4930-bbf6-032376451101'),
    ('ADV-SAM02', 'a7b27f13-8094-4156-b6ca-98d7bcb157c4'),
    ('AESYN2', '8da28c6e-d8e9-43b9-9ebe-45914c10988b'),
    ('ICCTM-DC2', '3a506e2b-fef9-4a36-b13d-a5d75f292d76'),
    ('NVIDIA-LICSRV', '0d202e5f-78ac-47b5-9604-aaa4dbcb7630'),
    ('PG1cRestore', 'f9b86234-acb9-4b63-92ff-1de3bd3bb351'),
    ('VM-CRM1C', 'c255bb1c-7052-40ce-a2e3-4f9882e2d61d'),
    ('WEB-SRV1', '803d4686-9b6e-4c15-9090-6eeeaa728a4b'),
]


def log(*args):
    print(*args)
    sys.stdout.flush()


def main():
    action = sys.argv[1]
    assert action in ('inspect', 'quiesce', 'force-stop', 'deactivate', 'activate', 'restart')
    password = sys.stdin.readline().rstrip('\r\n')
    connection = ovirtsdk4.Connection(
        url='https://ovirt.advengineering.ru/ovirt-engine/api',
        username='m.filippov@advengineering.ru', password=password,
        ca_file='/etc/pki/ovirt-engine/ca.pem', insecure=False,
        debug=False, timeout=45,
    )
    password = None
    try:
        system = connection.system_service()
        current = []
        for name, vm_id in TARGETS:
            service = system.vms_service().vm_service(vm_id)
            vm = service.get()
            assert vm.name == name
            current.append((name, vm_id, service, vm))
            log(name, str(vm.status))
        domain_service = (system.data_centers_service().data_center_service(POOL)
                          .storage_domains_service().storage_domain_service(DOMAIN))
        domain = domain_service.get()
        log('StdVolume domain status:', domain.status)
        if action == 'quiesce':
            path = BASE + '/stdvolume-vm-states-before-stop.json'
            if not os.path.exists(path):
                data = [{'name': n, 'id': i, 'status': str(v.status),
                         'host_id': v.host.id if v.host else None}
                        for n, i, _, v in current]
                fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
                with os.fdopen(fd, 'w') as output:
                    json.dump(data, output, indent=2)
            for name, _, service, vm in current:
                if vm.status == types.VmStatus.PAUSED:
                    service.stop(async_=True)
                    log(name, 'power-off requested: paused by storage EIO')
                elif vm.status == types.VmStatus.UP:
                    service.shutdown(async_=True)
                    log(name, 'guest shutdown requested')
        elif action == 'force-stop':
            assert os.path.exists(BASE + '/stdvolume-vm-states-before-stop.json')
            for name, _, service, vm in current:
                if vm.status != types.VmStatus.DOWN:
                    service.stop(async_=True)
                    log(name, 'power-off requested')
        elif action == 'deactivate':
            assert all(v.status == types.VmStatus.DOWN for _, _, _, v in current)
            domain_service.deactivate(async_=True, force=True)
            log('StdVolume deactivation requested')
        elif action == 'activate':
            domain_service.activate(async_=True)
            log('StdVolume activation requested')
        elif action == 'restart':
            assert domain.status == types.StorageDomainStatus.ACTIVE
            before = json.load(open(BASE + '/stdvolume-vm-states-before-stop.json'))
            previously_active = set(v['id'] for v in before if v['status'] != 'down')
            for name, vm_id, service, vm in current:
                if vm_id in previously_active and vm.status == types.VmStatus.DOWN:
                    service.start(async_=True)
                    log(name, 'start requested')
    finally:
        connection.close()


if __name__ == '__main__':
    main()
