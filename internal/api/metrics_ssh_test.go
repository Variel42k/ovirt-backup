package api

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/Variel42k/ovirt-backup/internal/sshstats"
)

// sshCollector — только счётчик SSH-подключений, без базы и оценки защиты.
type sshCollector struct {
	desc    *prometheus.Desc
	samples []sshstats.Sample
}

func (c sshCollector) Describe(ch chan<- *prometheus.Desc) { ch <- c.desc }
func (c sshCollector) Collect(ch chan<- prometheus.Metric) {
	collectSSHConnections(ch, c.desc, c.samples)
}

func TestSSHConnectionsMetric(t *testing.T) {
	registry := prometheus.NewPedanticRegistry()
	registry.MustRegister(sshCollector{
		desc: newBackupCollector(nil, nil, nil).desc["ssh_connections"],
		samples: []sshstats.Sample{
			{Host: "10.0.0.2:22", Component: sshstats.Libvirt, Connected: 2, Failed: 1},
			{Host: "10.0.0.3:22", Component: sshstats.DBDump, Connected: 1},
		},
	})
	families, err := registry.Gather()
	if err != nil {
		t.Fatalf("метрика не проходит проверку реестра: %v", err)
	}
	if len(families) != 1 || families[0].GetName() != "ovirt_backup_ssh_connections_total" ||
		families[0].GetType().String() != "COUNTER" {
		t.Fatalf("families = %v", families)
	}

	got := map[string]float64{}
	for _, m := range families[0].GetMetric() {
		labels := map[string]string{}
		for _, l := range m.GetLabel() {
			labels[l.GetName()] = l.GetValue()
		}
		got[labels["component"]+" "+labels["host"]+" "+labels["result"]] = m.GetCounter().GetValue()
	}
	// Неудачных подключений к 10.0.0.3 не было — ряда с result="failed" нет.
	want := map[string]float64{
		"libvirt 10.0.0.2:22 ok":     2,
		"libvirt 10.0.0.2:22 failed": 1,
		"db-dump 10.0.0.3:22 ok":     1,
	}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("%s = %v, want %v (all: %v)", k, got[k], v, got)
		}
	}
}
