package libvirtx

import "testing"

// Реальный фрагмент /proc/self/mountstats с ядра 5.x, укороченный до тех
// строк, которые разбираются.
const sampleMountStats = `device rootfs mounted on / with fstype rootfs
device /dev/sda1 mounted on /boot with fstype ext4
device nfs-srv:/export/vmstore mounted on /mnt/vmstore with fstype nfs4 statvers=1.1
	opts:	rw,vers=4.2,rsize=1048576,wsize=1048576,hard,proto=tcp
	age:	86400
	impl_id:	name='',domain='',date='0,0'
	caps:	caps=0xffff,wtmult=512,dtsize=1048576,bsize=0,namlen=255
	nfsv4:	bm0=0xfdffbfff,bm1=0x40f9be3e,bm2=0x803
	sec:	flavor=1,pseudoflavor=1
	events:	12 34 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0
	bytes:	100 200 0 0 300 400 5 6
	RPC iostats version: 1.1  p/v: 100003 (nfs)
	xprt:	tcp 875 1 6 0 120 5000 4900 17 0 64 0 0
	per-op statistics
	        NULL: 1 1 0 44 24 0 1 1
	        READ: 1000 1050 3 100000 900000 20 500 700
	       WRITE: 2000 2010 1 800000 40000 30 900 1200
	      COMMIT: 10 10 0 500 400 0 5 6
device nfs-srv:/export/iso mounted on /mnt/iso with fstype nfs statvers=1.1
	xprt:	udp 800 1 100 90 2 0 64
	per-op statistics
	        READ: 50 50 0 1000 2000 1 10 12
`

func TestParseMountStatsFindsNFSMountsOnly(t *testing.T) {
	mounts := ParseMountStats(sampleMountStats)
	if len(mounts) != 2 {
		t.Fatalf("найдено %d монтирований, ожидалось 2 (rootfs и ext4 не в счёт)", len(mounts))
	}
	if mounts[0].MountPoint != "/mnt/vmstore" || mounts[0].Source != "nfs-srv:/export/vmstore" {
		t.Errorf("первое монтирование разобрано неверно: %+v", mounts[0])
	}
	if mounts[0].FSType != "nfs4" {
		t.Errorf("тип ФС %q, ожидался nfs4", mounts[0].FSType)
	}
}

// Счётчики операций складываются по всем видам RPC: оператору нужна сводка по
// монтированию, а не разбор по отдельным вызовам.
func TestParseMountStatsSumsPerOperationCounters(t *testing.T) {
	m := ParseMountStats(sampleMountStats)[0]

	if want := int64(1 + 1000 + 2000 + 10); m.Operations != want {
		t.Errorf("операций %d, ожидалось %d", m.Operations, want)
	}
	if want := int64(1 + 1050 + 2010 + 10); m.Transmits != want {
		t.Errorf("передач %d, ожидалось %d", m.Transmits, want)
	}
	if want := int64(3 + 1); m.MajorTimeout != want {
		t.Errorf("таймаутов %d, ожидалось %d", m.MajorTimeout, want)
	}
}

// Ядро считает передачи, а не повторы. Повтор — это превышение передач над
// операциями; спутать одно с другим значит объявить исправную сеть больной.
func TestRetransmitsAreExcessOverOperations(t *testing.T) {
	m := ParseMountStats(sampleMountStats)[0]

	// 3011 передач против 3011 операций… считаем точно:
	// операций 1+1000+2000+10 = 3011, передач 1+1050+2010+10 = 3071.
	if got, want := m.Retransmits(), int64(60); got != want {
		t.Errorf("повторов %d, ожидалось %d", got, want)
	}

	clean := MountCounters{Operations: 500, Transmits: 500}
	if got := clean.Retransmits(); got != 0 {
		t.Errorf("на исправном монтировании повторов быть не должно, получено %d", got)
	}
	// Счётчики могли обнулиться при перемонтировании — отрицательных повторов не бывает.
	reset := MountCounters{Operations: 500, Transmits: 10}
	if got := reset.Retransmits(); got != 0 {
		t.Errorf("после сброса счётчиков повторов %d, ожидался 0", got)
	}
}

// Смещение полей на транспортной строке различается у tcp и udp: у tcp перед
// счётчиками идут ещё три поля про соединение. Ошибка здесь молча подставила бы
// под «плохие xid» соседнее число — и «сеть теряет пакеты» появилось бы на
// ровном месте.
//
//	tcp: port bind connect connect_time idle | sends recvs bad_xids ...
//	udp: port bind                          | sends recvs bad_xids ...
func TestParseXprtHandlesTCPAndUDP(t *testing.T) {
	mounts := ParseMountStats(sampleMountStats)

	tcp := mounts[0]
	if tcp.Sends != 5000 || tcp.Recvs != 4900 || tcp.BadXIDs != 17 {
		t.Errorf("tcp: sends=%d recvs=%d bad_xids=%d, ожидалось 5000/4900/17",
			tcp.Sends, tcp.Recvs, tcp.BadXIDs)
	}

	udp := mounts[1]
	if udp.Sends != 100 || udp.Recvs != 90 || udp.BadXIDs != 2 {
		t.Errorf("udp: sends=%d recvs=%d bad_xids=%d, ожидалось 100/90/2",
			udp.Sends, udp.Recvs, udp.BadXIDs)
	}
}

func TestParseMountStatsSurvivesGarbage(t *testing.T) {
	for _, raw := range []string{
		"",
		"мусор без структуры\nещё строка\n",
		"device broken mounted on\n",
		"device srv:/x mounted on /m with fstype nfs4\n\txprt:\ttcp\n\tper-op statistics\n\tREAD: 1 2\n",
	} {
		// Единственное требование — не паниковать и не выдумывать данные.
		for _, m := range ParseMountStats(raw) {
			if m.Operations < 0 || m.Transmits < 0 {
				t.Errorf("отрицательные счётчики из %q: %+v", raw, m)
			}
		}
	}
}

func TestISCSISessionHealth(t *testing.T) {
	cases := map[string]bool{
		"LOGGED_IN": true, "logged_in": true,
		"FAILED": false, "FREE": false, "": false,
	}
	for state, want := range cases {
		if got := (ISCSISession{State: state}).Healthy(); got != want {
			t.Errorf("состояние %q: получено %v, ожидалось %v", state, got, want)
		}
	}
}
