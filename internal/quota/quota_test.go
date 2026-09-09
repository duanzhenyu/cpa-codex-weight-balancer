package quota

import (
	"testing"
	"time"
)

var now = time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)

func TestExpiredIsInclusiveOfTheResetInstant(t *testing.T) {
	for name, w := range map[string]Window{
		"past":       {ResetAt: now.Add(-time.Second)},
		"exactly at": {ResetAt: now},
		"zero value": {},
	} {
		if !w.Expired(now) {
			t.Fatalf("%s: want expired", name)
		}
	}
	if (Window{ResetAt: now.Add(time.Second)}).Expired(now) {
		t.Fatal("a future reset is not expired")
	}
}

func TestUsableRejectsUnscorableValues(t *testing.T) {
	usable := func(w Window) bool { return w.Usable(now) }
	if !usable(Window{Remaining: 0, ResetAt: now.Add(time.Hour)}) {
		t.Fatal("an empty but live window is still scorable")
	}
	if usable(Window{Remaining: -1, ResetAt: now.Add(time.Hour)}) {
		t.Fatal("negative remaining is not scorable")
	}
	if usable(Window{Remaining: 5, ResetAt: now.Add(-time.Hour)}) {
		t.Fatal("an expired window is not scorable")
	}
}

func TestScaledMarksTierComparable(t *testing.T) {
	s := Snapshot{
		Weekly:   Window{Remaining: 60, ResetAt: now.Add(time.Hour)},
		FiveHour: Window{Remaining: 40, ResetAt: now.Add(time.Hour)},
	}
	got := s.Scaled(1000)
	if got.Weekly.Remaining != 60000 || !got.Weekly.Absolute {
		t.Fatalf("weekly %+v", got.Weekly)
	}
	if got.FiveHour.Remaining != 40000 || !got.FiveHour.Absolute {
		t.Fatalf("5h %+v", got.FiveHour)
	}
}

func TestScaledIsANoOpWithoutCapacity(t *testing.T) {
	s := Snapshot{Weekly: Window{Remaining: 60, ResetAt: now.Add(time.Hour)}}
	for _, capacity := range []float64{0, -1} {
		got := s.Scaled(capacity)
		if got.Weekly.Remaining != 60 || got.Weekly.Absolute {
			t.Fatalf("capacity %v: %+v", capacity, got.Weekly)
		}
	}
}

func TestScaledDoesNotMutateTheReceiver(t *testing.T) {
	s := Snapshot{Weekly: Window{Remaining: 60, ResetAt: now.Add(time.Hour)}}
	_ = s.Scaled(1000)
	if s.Weekly.Remaining != 60 || s.Weekly.Absolute {
		t.Fatalf("receiver mutated: %+v", s.Weekly)
	}
}
