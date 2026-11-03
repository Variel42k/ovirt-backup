package api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Variel42k/ovirt-backup/internal/backup"
	"github.com/Variel42k/ovirt-backup/internal/model"
)

func TestJobCompressionPayloadPreservesLegacyClientChoice(t *testing.T) {
	job := model.BackupJob{Compression: "lz4"}
	jobPayload{}.apply(&job)
	if job.Compression != "lz4" {
		t.Fatal("old client reset compression")
	}
	value := " BROTLI "
	jobPayload{Compression: &value}.apply(&job)
	if job.Compression != "brotli" {
		t.Fatal("algorithm not normalized")
	}
	value = ""
	jobPayload{Compression: &value}.apply(&job)
	if job.Compression != "" {
		t.Fatal("inherit not restored")
	}
}

func TestCompressionOptionsMatchCodecs(t *testing.T) {
	options := backupCompressionOptions()
	if len(options) != len(backup.Compressions) {
		t.Fatal("incomplete options")
	}
	for i, name := range backup.Compressions {
		if options[i].Value != name || options[i].Description == "" {
			t.Fatalf("option %d: %+v", i, options[i])
		}
	}
}

func TestAdHocCompressionRejectsInvalidAndNonDiskChoicesBeforeExecution(t *testing.T) {
	for _, body := range []string{
		`{"compression":"neural"}`, `{"compression":"lz4","type":"config"}`, `{"compression":"brotli","type":"ova"}`,
	} {
		s := &Server{}
		r := httptest.NewRequest(http.MethodPost, "/backups", bytes.NewBufferString(body))
		w := httptest.NewRecorder()
		s.handleAdHocBackup(w, r)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("%s: %d %s", body, w.Code, w.Body.String())
		}
	}
}
