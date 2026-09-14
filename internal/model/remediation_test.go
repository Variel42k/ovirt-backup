package model

import "testing"

func TestRemediationActionValidForScope(t *testing.T) {
	tests := []struct {
		action RemediationAction
		scope  Scope
		valid  bool
	}{
		{ActionVMStart, ScopeVM, true},
		{ActionVMReset, ScopeVM, true},
		{ActionVMStart, ScopeHost, false},
		{ActionHostActivate, ScopeHost, true},
		{ActionHostFence, ScopeHost, true},
		{ActionHostFence, ScopeVM, false},
		{ActionReconnect, ScopeServer, true},
		{ActionReconnect, ScopeVM, false},
		{RemediationAction("unknown"), ScopeVM, false},
	}

	for _, test := range tests {
		if got := test.action.ValidForScope(test.scope); got != test.valid {
			t.Errorf("%s.ValidForScope(%s) = %v, ожидалось %v", test.action, test.scope, got, test.valid)
		}
	}
}
