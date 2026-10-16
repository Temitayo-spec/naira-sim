package clock

import (
	"context"
	"testing"
	"time"
)

var start = time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)

func TestRunsInTimeOrderThenScheduleOrder(t *testing.T) {
	s := New(start)
	var got []string
	s.After(2*time.Hour, func() { got = append(got, "c") })
	s.After(time.Hour, func() { got = append(got, "a") })
	s.After(time.Hour, func() { got = append(got, "b") }) // same time: after "a"
	s.Advance(3 * time.Hour)
	if want := "abc"; join(got) != want {
		t.Fatalf("ran %q, want %q", join(got), want)
	}
}

func TestTaskSeesItsOwnTime(t *testing.T) {
	s := New(start)
	var at time.Time
	s.After(90*time.Minute, func() { at = s.Now() })
	s.Advance(5 * time.Hour)
	if want := start.Add(90 * time.Minute); !at.Equal(want) {
		t.Errorf("task saw %v, want %v", at, want)
	}
	if want := start.Add(5 * time.Hour); !s.Now().Equal(want) {
		t.Errorf("clock at %v, want %v", s.Now(), want)
	}
}

func TestTasksScheduledWhileRunningAlsoRunIfDue(t *testing.T) {
	s := New(start)
	var got []string
	s.After(time.Minute, func() {
		got = append(got, "first")
		s.After(time.Minute, func() { got = append(got, "second") }) // due at +2m
		s.After(time.Hour, func() { got = append(got, "later") })    // not due
	})
	s.Advance(10 * time.Minute)
	if want := "firstsecond"; join(got) != want {
		t.Fatalf("ran %q, want %q", join(got), want)
	}
	if s.Pending() != 1 {
		t.Errorf("%d pending, want 1", s.Pending())
	}
}

func TestNothingRunsBeforeItsTime(t *testing.T) {
	s := New(start)
	ran := false
	s.After(time.Hour, func() { ran = true })
	s.Advance(59 * time.Minute)
	if ran {
		t.Fatal("ran a minute early")
	}
	s.Advance(time.Minute)
	if !ran {
		t.Fatal("didn't run on time")
	}
}

func TestNeverMovesBackwards(t *testing.T) {
	s := New(start)
	s.AdvanceTo(start.Add(-time.Hour))
	if !s.Now().Equal(start) {
		t.Errorf("clock moved back to %v", s.Now())
	}
}

func TestFollowTracksRealTime(t *testing.T) {
	s := New(start)
	ran := make(chan struct{})
	s.After(20*time.Millisecond, func() { close(ran) })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Follow(ctx, 5*time.Millisecond)
	select {
	case <-ran:
	case <-time.After(2 * time.Second):
		t.Fatal("a real-mode task never ran")
	}
}

func join(xs []string) string {
	out := ""
	for _, x := range xs {
		out += x
	}
	return out
}
