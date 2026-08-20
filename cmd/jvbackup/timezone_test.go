package main

import "testing"

func TestDefaultTimezonePrefersApplicationConfiguration(t *testing.T) {
	t.Setenv("JHV_SCHEDULER_TIMEZONE", "Asia/Yekaterinburg")
	t.Setenv("TZ", "Europe/Moscow")
	if got := defaultTimezone(); got != "Asia/Yekaterinburg" {
		t.Fatalf("default timezone = %q", got)
	}
}

func TestDefaultTimezoneFallsBackFromInvalidConfigurationToHostEnvironment(t *testing.T) {
	t.Setenv("JHV_SCHEDULER_TIMEZONE", "Mars/Olympus")
	t.Setenv("TZ", "Europe/Moscow")
	if got := defaultTimezone(); got != "Europe/Moscow" {
		t.Fatalf("default timezone = %q", got)
	}
}
