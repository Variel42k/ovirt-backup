package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"adveng/jh_virt/internal/model"
)

func TestNotificationOutboxSurvivesRetryAndResolution(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	alert := &model.Alert{ServerID: "srv", Scope: model.ScopeVM, ObjectID: "vm-1",
		ObjectName: "db-01", Kind: model.AlertBackupUnprotected,
		Severity: model.SeverityWarning, Message: "ВМ не защищена"}
	if err := s.RaiseAlert(ctx, alert); err != nil {
		t.Fatal(err)
	}

	claimed, ok, err := s.ClaimNotificationAlert(ctx, "scheduler-a", time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim alert: ok=%v err=%v", ok, err)
	}
	next := time.Now().UTC().Add(time.Hour)
	if err := s.ScheduleNotification(ctx, "scheduler-a", claimed, model.NotificationOpened,
		[]string{"webhook"}, []byte(`{"kind":"backup_unprotected"}`), &next); err != nil {
		t.Fatal(err)
	}

	delivery, ok, err := s.ClaimNotificationDelivery(ctx, "sender-a", time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim delivery: ok=%v err=%v", ok, err)
	}
	if delivery.Channel != "webhook" || delivery.Event != model.NotificationOpened {
		t.Fatalf("delivery = %+v", delivery)
	}
	if err := s.CompleteNotificationDelivery(ctx, delivery.ID, "sender-a",
		errors.New("endpoint unavailable"), time.Now().UTC().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	items, err := s.ListNotificationDeliveries(ctx, 10)
	if err != nil || len(items) != 1 {
		t.Fatalf("deliveries: len=%d err=%v", len(items), err)
	}
	if items[0].Status != "queued" || items[0].Attempts != 1 || items[0].LastError == "" {
		t.Fatalf("retry was not persisted: %+v", items[0])
	}

	if err := s.ResolveAlert(ctx, "srv", model.ScopeVM, "vm-1", model.AlertBackupUnprotected); err != nil {
		t.Fatal(err)
	}
	resolved, ok, err := s.ClaimNotificationAlert(ctx, "scheduler-b", time.Minute)
	if err != nil || !ok || resolved.State != model.AlertResolved {
		t.Fatalf("claim resolution: alert=%+v ok=%v err=%v", resolved, ok, err)
	}
}

func TestNotificationMuteAndPolicyRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	settings := model.NotificationSettings{Enabled: true, MinSeverity: model.SeverityWarning,
		DefaultRepeatMinutes: 60, NotifyOnResolved: true, AckStopsRepeats: true, MaxRepeats: 3}
	if err := s.SetNotificationSettings(ctx, settings, "admin"); err != nil {
		t.Fatal(err)
	}
	policy := model.NotificationPolicy{Kind: model.AlertBackupUnprotected, Enabled: false,
		RepeatMinutes: 1440, StopOnAck: true, Channels: []string{"email"}}
	if err := s.ReplaceNotificationPolicies(ctx, []model.NotificationPolicy{policy}, "admin"); err != nil {
		t.Fatal(err)
	}
	override, err := s.NotificationSettingsOverride(ctx)
	if err != nil || override.Enabled == nil || !*override.Enabled || override.DefaultRepeatMinutes == nil || *override.DefaultRepeatMinutes != 60 {
		t.Fatalf("settings override = %+v, err=%v", override, err)
	}
	policies, err := s.ListNotificationPolicies(ctx)
	if err != nil || len(policies) != 1 || policies[0].Enabled || policies[0].Channels[0] != "email" {
		t.Fatalf("policies = %+v, err=%v", policies, err)
	}

	alert := &model.Alert{ServerID: "srv", Scope: model.ScopeVM, ObjectID: "vm-2",
		Kind: model.AlertBackupUnprotected, Severity: model.SeverityWarning, Message: "not protected"}
	if err := s.RaiseAlert(ctx, alert); err != nil {
		t.Fatal(err)
	}
	alerts, err := s.ListAlerts(ctx, AlertFilter{ServerID: "srv"})
	if err != nil || len(alerts) != 1 {
		t.Fatalf("alerts: %+v, %v", alerts, err)
	}
	if err := s.SetAlertNotificationMute(ctx, alerts[0].ID, "admin", "mute", nil, "accepted risk"); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.ClaimNotificationAlert(ctx, "scheduler", time.Minute); err != nil || ok {
		t.Fatalf("muted alert was claimable: ok=%v err=%v", ok, err)
	}
	if err := s.SetAlertNotificationMute(ctx, alerts[0].ID, "admin", "unmute", nil, ""); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.ClaimNotificationAlert(ctx, "scheduler", time.Minute); err != nil || !ok {
		t.Fatalf("unmuted alert was not claimable: ok=%v err=%v", ok, err)
	}
}

func TestNotificationConfigurationIsAtomic(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	initial := model.NotificationSettings{Enabled: true, MinSeverity: model.SeverityWarning,
		DefaultRepeatMinutes: 60, AckStopsRepeats: true}
	initialPolicy := model.NotificationPolicy{Kind: model.AlertBackupStale, Enabled: true,
		RepeatMinutes: 120, StopOnAck: true}
	if err := s.SetNotificationConfiguration(ctx, initial, []model.NotificationPolicy{initialPolicy}, "admin"); err != nil {
		t.Fatal(err)
	}

	replacement := initial
	replacement.Enabled = false
	replacement.DefaultRepeatMinutes = 5
	duplicate := []model.NotificationPolicy{initialPolicy, initialPolicy}
	if err := s.SetNotificationConfiguration(ctx, replacement, duplicate, "admin"); err == nil {
		t.Fatal("duplicate policies unexpectedly committed")
	}
	override, err := s.NotificationSettingsOverride(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if override.Enabled == nil || !*override.Enabled || override.DefaultRepeatMinutes == nil || *override.DefaultRepeatMinutes != 60 {
		t.Fatalf("global settings changed after rollback: %+v", override)
	}
	policies, err := s.ListNotificationPolicies(ctx)
	if err != nil || len(policies) != 1 || policies[0].Kind != model.AlertBackupStale {
		t.Fatalf("policies changed after rollback: %+v, err=%v", policies, err)
	}
}

func TestMutedAlertDoesNotSendResolution(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	alert := &model.Alert{ServerID: "srv", Scope: model.ScopeVM, ObjectID: "vm-muted",
		Kind: model.AlertBackupStale, Severity: model.SeverityWarning, Message: "stale"}
	if err := s.RaiseAlert(ctx, alert); err != nil {
		t.Fatal(err)
	}
	claimed, ok, err := s.ClaimNotificationAlert(ctx, "sender", time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	if err := s.ScheduleNotification(ctx, "sender", claimed, model.NotificationOpened,
		[]string{"webhook"}, []byte(`{"kind":"backup_stale"}`), nil); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAlertNotificationMute(ctx, claimed.ID, "admin", "mute", nil, "accepted risk"); err != nil {
		t.Fatal(err)
	}
	if err := s.ResolveAlert(ctx, "srv", model.ScopeVM, "vm-muted", model.AlertBackupStale); err != nil {
		t.Fatal(err)
	}
	if got, ok, err := s.ClaimNotificationAlert(ctx, "sender", time.Minute); err != nil || ok {
		t.Fatalf("muted resolution was claimable: alert=%+v ok=%v err=%v", got, ok, err)
	}
}
