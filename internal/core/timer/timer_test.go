package timer

import (
	"testing"
	"time"
)

func TestTimerIdentityAndFrozenTarget(t *testing.T) {
	a := Request{TenantID: "org", SubjectRef: "bot", ScopeID: "channel", TimerID: "reminder", Revision: 1, DueAt: time.Date(2026, 10, 19, 12, 0, 0, 123456789, time.UTC), OpaqueActionRef: "task:1"}
	b := a
	b.DueAt = b.DueAt.In(time.FixedZone("+8", 8*3600)).Truncate(time.Microsecond)
	if err := a.Validate(); err != nil {
		t.Fatal(err)
	}
	if a.Digest() != b.Digest() {
		t.Fatal("equivalent absolute timestamps must replay identically")
	}
	b.OpaqueActionRef = "task:2"
	if a.Key() != b.Key() || a.Fire().FireID != b.Fire().FireID || a.Digest() == b.Digest() {
		t.Fatal("same business key must expose a payload conflict")
	}
	b = a
	b.Revision++
	if a.Key() == b.Key() || a.Fire().FireID == b.Fire().FireID {
		t.Fatal("revisions need independent workflow/fire identities")
	}
	target := Target{Ref: "bots", TenantID: "org", SubjectRef: "bot", ScopeID: "channel", CallbackURL: "http://bots/internal/timers/fire", BearerToken: "old"}
	rotated := target
	rotated.BearerToken = "new"
	if target.Digest() != rotated.Digest() {
		t.Fatal("token rotation must preserve frozen routing identity")
	}
	rotated.CallbackURL = "http://other/internal/timers/fire"
	if target.Digest() == rotated.Digest() {
		t.Fatal("callback destination must be frozen")
	}
	invalid := target
	invalid.CallbackURL = "http://secret@example.com/callback"
	if invalid.Validate() == nil {
		t.Fatal("userinfo in configured URL must be rejected")
	}
}
