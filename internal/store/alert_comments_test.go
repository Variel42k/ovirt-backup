package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Variel42k/ovirt-backup/internal/model"
)

// Раздел принятых — журнал: оповещение остаётся в нём с пояснениями и после
// закрытия, и после того, как загорелось снова.
func TestAcceptedAlertsKeepComments(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	raise := func(objectID string) *model.Alert {
		t.Helper()
		if err := s.RaiseAlert(ctx, &model.Alert{
			ServerID: "srv", Scope: model.ScopeStorageDomain, ObjectID: objectID, ObjectName: objectID,
			Kind: model.AlertStorageDomainFull, Severity: model.SeverityWarning, Message: "домен заполнен",
		}); err != nil {
			t.Fatalf("raise %s: %v", objectID, err)
		}
		items, err := s.ListAlerts(ctx, AlertFilter{ServerID: "srv", ObjectID: objectID})
		if err != nil || len(items) != 1 {
			t.Fatalf("list %s: %v, %d rows", objectID, err, len(items))
		}
		return items[0]
	}
	accepted := func() []*model.Alert {
		t.Helper()
		items, err := s.ListAlerts(ctx, AlertFilter{Accepted: true})
		if err != nil {
			t.Fatalf("list accepted: %v", err)
		}
		return items
	}

	explained := raise("domain-explained")
	silent := raise("domain-silent")
	untouched := raise("domain-untouched")

	// К непринятому оповещению пояснение не пишут: сначала его берут в работу.
	if _, err := s.AddAlertComment(ctx, untouched.ID, "admin", "рано"); !errors.Is(err, ErrConflict) {
		t.Fatalf("comment on a firing alert: %v, want conflict", err)
	}
	if err := s.AckAlert(ctx, explained.ID, "admin", "  домен общий с ISO, расширение заказано  "); err != nil {
		t.Fatalf("ack with comment: %v", err)
	}
	if err := s.AckAlert(ctx, silent.ID, "admin", ""); err != nil {
		t.Fatalf("ack without comment: %v", err)
	}
	if err := s.AckAlert(ctx, explained.ID, "other", "повторно"); !errors.Is(err, ErrConflict) {
		t.Fatalf("second ack: %v, want conflict", err)
	}
	if err := s.AckAlert(ctx, "missing", "admin", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ack of a missing alert: %v, want not found", err)
	}
	if _, err := s.AddAlertComment(ctx, explained.ID, "admin", "расширили 02.10"); err != nil {
		t.Fatalf("add comment: %v", err)
	}

	items := accepted()
	if len(items) != 2 {
		t.Fatalf("accepted = %d rows, want the two acknowledged alerts", len(items))
	}
	byID := map[string]*model.Alert{}
	for _, item := range items {
		byID[item.ID] = item
	}
	got := byID[explained.ID]
	if got == nil || got.AckedBy != "admin" || got.AckedAt == nil || len(got.Comments) != 2 ||
		got.Comments[0].Message != "домен общий с ISO, расширение заказано" || got.Comments[1].Message != "расширили 02.10" {
		t.Fatalf("explained alert lost its comments or their order: %+v", got)
	}
	if other := byID[silent.ID]; other == nil || len(other.Comments) != 0 {
		t.Fatalf("alert accepted without a comment: %+v", other)
	}
	// Обычный список пояснения не несёт: они нужны только разделу принятых.
	plain, _ := s.ListAlerts(ctx, AlertFilter{ServerID: "srv", ObjectID: explained.ObjectID})
	if len(plain) != 1 || plain[0].Comments != nil {
		t.Fatalf("plain listing carries comments: %+v", plain)
	}

	// Закрылось и загорелось снова: отметка о принятии сброшена, но
	// оповещение с пояснениями из раздела не пропадает.
	for _, objectID := range []string{explained.ObjectID, silent.ObjectID} {
		if err := s.ResolveAlert(ctx, "srv", model.ScopeStorageDomain, objectID, model.AlertStorageDomainFull); err != nil {
			t.Fatalf("resolve %s: %v", objectID, err)
		}
	}
	raise(explained.ObjectID)
	raise(silent.ObjectID)
	items = accepted()
	if len(items) != 1 || items[0].ID != explained.ID || items[0].State != model.AlertFiring ||
		items[0].AckedAt != nil || items[0].AckedBy != "" || len(items[0].Comments) != 2 {
		t.Fatalf("after reopening accepted = %+v, want only the explained alert, firing and not acked", items)
	}

	// Чистка истории убирает закрытые оповещения, кроме тех, что с пояснениями.
	for _, objectID := range []string{explained.ObjectID, silent.ObjectID} {
		if err := s.ResolveAlert(ctx, "srv", model.ScopeStorageDomain, objectID, model.AlertStorageDomainFull); err != nil {
			t.Fatalf("resolve %s: %v", objectID, err)
		}
	}
	removed, err := s.PurgeResolvedAlerts(ctx, time.Now().Add(time.Hour))
	if err != nil || removed != 1 {
		t.Fatalf("purge removed %d rows, %v; want only the alert without comments", removed, err)
	}
	comments, err := s.ListAlertComments(ctx, explained.ID)
	if err != nil || len(comments) != 2 {
		t.Fatalf("comments after purge: %d, %v", len(comments), err)
	}
}
