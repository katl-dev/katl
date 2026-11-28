package queueadmission

import "testing"

// The planner and Evaluate both decide holds here, so a bare line that held
// one and not the other would split them.
func TestDeclaredHold(t *testing.T) {
	for _, tc := range []struct {
		name, body, reason string
		declared           bool
	}{
		{"none", "Closes #5\nholding pattern\n", "", false},
		{"plain", "Closes #5\n\nHold: awaiting the VM gate\n", "awaiting the VM gate", true},
		{"case and indentation", "   hOLD:   needs a journey  \n", "needs a journey", true},
		{"bare", "Why.\n\nHold:\n", "no reason given", true},
		{"all bare", "Hold:\nHold:   \n", "no reason given", true},
		{"bare before a reason", "Hold:\nHold: after #12\nHold: later\n", "after #12", true},
		{"inline mention", "see Hold: later\n", "", false},
		{"fenced", "```\nHold: example\n```\n~~~sh\nHold: another\n~~~\n", "", false},
		{"after a fence", "```\nHold: example\n```\nHold: real\n", "real", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reason, declared := DeclaredHold(tc.body)
			if reason != tc.reason || declared != tc.declared {
				t.Errorf("DeclaredHold() = %q, %v; want %q, %v", reason, declared, tc.reason, tc.declared)
			}
		})
	}
}
