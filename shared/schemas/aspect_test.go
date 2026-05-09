package schemas

import "testing"

// Operator role identity tests — added with dashboard-ws-port spec
// (2026-05-09). Pin two invariants:
//
//   1. RoleOperator is recognized as a runtime identity (login flow,
//      WS register frame) but NOT as an on-disk aspect.json role.
//      The split exists so an aspect.json can't declare role:"operator"
//      and slip past file-based identity boundaries — operators are
//      mint-at-login only.
//   2. Known() and IsRuntimeIdentity() are not interchangeable.

func TestRoleKnownExcludesOperator(t *testing.T) {
	if RoleOperator.Known() {
		t.Error("RoleOperator must NOT be Known() — operators are runtime-only, not on-disk aspect.json roles")
	}
}

func TestRoleKnownAcceptsAspectAndFrame(t *testing.T) {
	for _, r := range []Role{RoleAspect, RoleFrame} {
		if !r.Known() {
			t.Errorf("%q must be Known()", r)
		}
	}
}

func TestRoleKnownRejectsUnknown(t *testing.T) {
	if Role("oprator").Known() {
		t.Error("typo'd role must not be Known()")
	}
	if Role("").Known() {
		t.Error("empty role must not be Known() (caller normalizes via EffectiveRole)")
	}
}

func TestIsRuntimeIdentityIncludesAll(t *testing.T) {
	for _, r := range []Role{RoleAspect, RoleFrame, RoleOperator} {
		if !r.IsRuntimeIdentity() {
			t.Errorf("%q must IsRuntimeIdentity() — it's a recognized runtime principal", r)
		}
	}
}

func TestIsRuntimeIdentityRejectsUnknown(t *testing.T) {
	if Role("ghost").IsRuntimeIdentity() {
		t.Error("unrecognized role must not IsRuntimeIdentity()")
	}
}
