from __future__ import print_function

import getpass
import json
import os
import sys
import time

import ovirtsdk4
import ovirtsdk4.types as types

TARGETS = [
    ('ADV-SAM01', 'b9fdf390-c0b5-4930-bbf6-032376451101'),
    ('ADV-COS7-01', '1fadd1bc-c533-4152-96cf-61baadc1e495'),
    ('ADV-SAM06', 'e7171027-6ef8-45f9-85d8-86897370766a'),
]
BACKUP = '/var/tmp/vgpu-start-recovery-20261105'


def properties(vm):
    return [{'name': p.name, 'value': p.value} for p in vm.custom_properties or []]


def main():
    action = sys.argv[1]
    if action not in ('inspect', 'apply', 'start', 'status'):
        raise ValueError('Unknown action')
    password = (getpass.getpass('Domain password: ') if sys.stdin.isatty()
                else sys.stdin.readline().rstrip('\r\n'))
    connection = ovirtsdk4.Connection(
        url='https://ovirt.advengineering.ru/ovirt-engine/api',
        username='m.filippov@advengineering.ru',
        password=password,
        ca_file='/etc/pki/ovirt-engine/ca.pem',
        insecure=False,
        debug=False,
        timeout=60,
    )
    password = None
    try:
        services = connection.system_service().vms_service()
        current = []
        for name, vm_id in TARGETS:
            service = services.vm_service(vm_id)
            vm = service.get()
            assert vm.name == name, 'Unexpected VM identity'
            current.append((name, service, vm))
            print(name, 'status=' + str(vm.status),
                  'custom_properties=' + json.dumps(properties(vm)))
        if action == 'inspect':
            for name, service, vm in current:
                for nic in service.nics_service().list():
                    print(name, 'NIC', nic.name, 'linked=', nic.linked,
                          'plugged=', nic.plugged)
                for disk in service.disk_attachments_service().list():
                    print(name, 'disk', disk.disk.id, 'active=', disk.active)
        elif action == 'apply':
            assert os.path.isfile(BACKUP + '/before.json'), 'Missing backup'
            assert all(vm.status == types.VmStatus.DOWN for _, _, vm in current), 'VM must be down'
            before = dict((name, properties(vm)) for name, _, vm in current)
            path = BACKUP + '/api-custom-properties-before.json'
            if not os.path.exists(path):
                fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
                with os.fdopen(fd, 'w') as output:
                    json.dump(before, output, indent=2)
            for name, service, vm in current:
                kept = [p for p in vm.custom_properties or [] if p.name != 'mdev_type']
                if len(kept) != len(vm.custom_properties or []):
                    service.update(types.Vm(custom_properties=kept))
                updated = service.get()
                assert not any(p.name == 'mdev_type' for p in updated.custom_properties or [])
                print(name, 'vGPU property removed; status=' + str(updated.status))
        elif action == 'start':
            for name, service, vm in current:
                assert not any(p.name == 'mdev_type' for p in vm.custom_properties or [])
                if vm.status == types.VmStatus.DOWN:
                    service.start()
                    print(name, 'start requested')
                else:
                    print(name, 'already', vm.status)
        sys.stdout.flush()
    finally:
        connection.close()


if __name__ == '__main__':
    main()
