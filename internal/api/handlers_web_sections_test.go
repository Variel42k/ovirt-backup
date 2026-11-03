package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Variel42k/ovirt-backup/internal/model"
)

func TestWebSectionsRequireCompleteBooleanSettings(t *testing.T) {
	s := &Server{store: testStore(t)}
	for _, body := range []string{`{}`, `{"discovery":false}`, `{"discovery":null,"gitlab_clean":true}`, `{"discovery":"false","gitlab_clean":true}`} {
		rec := httptest.NewRecorder()
		s.handleSetWebSections(rec, httptest.NewRequest(http.MethodPut, "/", bytes.NewBufferString(body)))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: %d %s", body, rec.Code, rec.Body.String())
		}
	}
	rec := httptest.NewRecorder()
	s.handleSetWebSections(rec, httptest.NewRequest(http.MethodPut, "/", bytes.NewBufferString(`{"discovery":false,"gitlab_clean":true}`)))
	if rec.Code != http.StatusOK {
		t.Fatal(rec.Code, rec.Body.String())
	}
	var value model.WebSections
	if err := json.Unmarshal(rec.Body.Bytes(), &value); err != nil {
		t.Fatal(err)
	}
	stored, err := s.store.WebSections(context.Background())
	if err != nil || stored != value || value.Discovery || !value.GitlabClean {
		t.Fatal(stored, value, err)
	}
}
