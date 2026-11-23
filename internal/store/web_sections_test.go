package store

import (
	"context"
	"testing"

	"github.com/Variel42k/ovirt-backup/internal/model"
)

func TestWebSectionsDefaultsAndPersistence(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	value, err := s.WebSections(ctx)
	if err != nil || value != model.DefaultWebSections() {
		t.Fatal(value, err)
	}
	for _, expected := range []model.WebSections{{Discovery: false}, {Discovery: true}} {
		if err := s.SetWebSections(ctx, expected, "admin"); err != nil {
			t.Fatal(err)
		}
		// Другой экземпляр Store должен наблюдать значение DB без локального кэша процесса.
		reader := New(s.db, s.cipher)
		actual, err := reader.WebSections(ctx)
		if err != nil || actual != expected {
			t.Fatalf("got %+v, expected %+v: %v", actual, expected, err)
		}
	}
}
