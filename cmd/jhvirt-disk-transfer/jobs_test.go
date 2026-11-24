package main

import (
	"bufio"
	"io"
	"strings"
	"testing"
)

func TestChooseStartupJob(t *testing.T) {
	newest := savedJob{directory: "/var/tmp/ovirt-transfer-123", tracked: true}
	older := savedJob{directory: "/var/tmp/ovirt-transfer-24", tracked: true}
	tests := []struct {
		name string
		jobs []savedJob
		keys string
		want string
	}{
		{name: "no pending jobs"},
		{name: "single recorded job resumes without input", jobs: []savedJob{newest}, want: newest.directory},
		{name: "legacy job requires selection", jobs: []savedJob{{directory: older.directory}}, keys: "1\n", want: older.directory},
		{name: "several jobs allow selection", jobs: []savedJob{newest, older}, keys: "2\n", want: older.directory},
		{name: "several jobs allow main menu", jobs: []savedJob{newest, older}, keys: "0\n"},
		{name: "active transfer prevents automatic start", jobs: []savedJob{{directory: newest.directory, active: true}, older}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ui := console{input: bufio.NewReader(strings.NewReader(test.keys)), output: io.Discard}
			got, err := chooseStartupJob(ui, test.jobs)
			if err != nil || got != test.want {
				t.Fatalf("got %q, %v; want %q", got, err, test.want)
			}
		})
	}
}
