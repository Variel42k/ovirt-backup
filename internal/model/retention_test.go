package model

import (
	"strings"
	"testing"
)

func TestRetentionPolicyRejectsNegativeValues(t *testing.T) {
	tests := []struct {
		name   string
		policy RetentionPolicy
		field  string
	}{
		{"last", RetentionPolicy{KeepLast: -1}, "keep_last"},
		{"hourly", RetentionPolicy{KeepHourly: -1}, "keep_hourly"},
		{"daily", RetentionPolicy{KeepDaily: -1}, "keep_daily"},
		{"weekly", RetentionPolicy{KeepWeekly: -1}, "keep_weekly"},
		{"monthly", RetentionPolicy{KeepMonthly: -1}, "keep_monthly"},
		{"yearly", RetentionPolicy{KeepYearly: -1}, "keep_yearly"},
		{"max age", RetentionPolicy{MaxAge: -1}, "max_age"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.policy.Validate()
			if err == nil || !strings.Contains(err.Error(), test.field) {
				t.Fatalf("ожидалась ошибка для %s, получено %v", test.field, err)
			}
		})
	}

	if err := DefaultRetention().Validate(); err != nil {
		t.Fatalf("правильная политика отвергнута: %v", err)
	}
}
