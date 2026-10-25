package model

import "testing"

func TestClusterRecoveryDefaultsAreSafeAndValidated(t *testing.T) {
	p := DefaultClusterRecoveryPolicy("engine", "cluster")
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	if p.Migration.Enabled || p.VM.Override || p.VM.Enabled || p.Disruptive() {
		t.Fatal("new policies must not enable power operations")
	}
	p.Migration.AllowForceStopTarget = true
	if p.Validate() == nil {
		t.Fatal("force stop without post-start rollback authorization")
	}
	p.Migration.RollbackAfterTargetStart = true
	if p.Validate() != nil || !p.Disruptive() {
		t.Fatal("explicit disruptive policy rejected")
	}
	p.Migration.NoProgressSeconds = 0
	if p.Validate() == nil {
		t.Fatal("unbounded progress wait accepted")
	}
}

func TestVMRecoveryScopeRequiresExplicitClusterPolicyAndTag(t *testing.T) {
	p := DefaultClusterRecoveryPolicy("s", "c").VM
	v := &VM{Tags: []string{"test"}}
	if p.AppliesTo(v) {
		t.Fatal("legacy policy overridden by default")
	}
	p.Override = true
	p.RequiredTag = "test"
	if !p.AppliesTo(v) {
		t.Fatal("tagged VM excluded")
	}
	if p.AppliesTo(&VM{Tags: []string{"production"}}) {
		t.Fatal("unselected VM affected")
	}
	p.RequiredTag = ""
	if !p.AppliesTo(&VM{}) {
		t.Fatal("empty selector must cover cluster")
	}
}
