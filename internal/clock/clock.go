// Package clock is the simulator's own time (spec S27, S28).
//
// Everything timed (settling, holds, reversals, webhook sends and retries) is
// a task on one Scheduler. Time only moves when something advances it: a test
// in manual mode, or a ticker following the computer's clock in real mode.
// Tasks run in time order, and tasks due at the same moment run in the order
// they were scheduled, so a replay runs exactly the same way.
package clock

import (
	"container/heap"
	"context"
	"sync"
	"time"
)

// Scheduler is a virtual clock with a queue of tasks. It's safe for concurrent use.
type Scheduler struct {
	mu   sync.Mutex
	now  time.Time
	seq  uint64
	jobs jobHeap

	run sync.Mutex // one AdvanceTo at a time
}

// New starts a clock at start.
func New(start time.Time) *Scheduler {
	return &Scheduler{now: start.UTC()}
}

// Now is the simulator's current time.
func (s *Scheduler) Now() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.now
}

// At runs f when the clock reaches t. A time already past runs at the next advance.
func (s *Scheduler) At(t time.Time, f func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	heap.Push(&s.jobs, job{at: t.UTC(), seq: s.seq, f: f})
}

// After runs f once d has passed on this clock.
func (s *Scheduler) After(d time.Duration, f func()) {
	s.At(s.Now().Add(d), f)
}

// Pending reports how many tasks are waiting.
func (s *Scheduler) Pending() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.jobs)
}

// Advance moves the clock forward by d, running everything due on the way.
func (s *Scheduler) Advance(d time.Duration) {
	s.AdvanceTo(s.Now().Add(d))
}

// AdvanceTo moves the clock to t, running every task due by then in order. A
// task runs with the clock set to its own time, and anything it schedules for
// t or earlier also runs before this returns. The clock never moves backwards.
func (s *Scheduler) AdvanceTo(t time.Time) {
	s.run.Lock()
	defer s.run.Unlock()
	t = t.UTC()
	for {
		s.mu.Lock()
		if len(s.jobs) == 0 || s.jobs[0].at.After(t) {
			if t.After(s.now) {
				s.now = t
			}
			s.mu.Unlock()
			return
		}
		next := heap.Pop(&s.jobs).(job)
		if next.at.After(s.now) {
			s.now = next.at
		}
		s.mu.Unlock()
		next.f() // outside the lock: tasks schedule more tasks and read Now
	}
}

// Follow keeps the clock in step with the computer's clock, starting from
// this clock's current time, until ctx is cancelled (real mode, S27).
func (s *Scheduler) Follow(ctx context.Context, every time.Duration) {
	base, realStart := s.Now(), time.Now()
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.AdvanceTo(base.Add(time.Since(realStart)))
		}
	}
}

type job struct {
	at  time.Time
	seq uint64
	f   func()
}

type jobHeap []job

func (h jobHeap) Len() int { return len(h) }
func (h jobHeap) Less(i, j int) bool {
	if !h[i].at.Equal(h[j].at) {
		return h[i].at.Before(h[j].at)
	}
	return h[i].seq < h[j].seq
}
func (h jobHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *jobHeap) Push(x any)   { *h = append(*h, x.(job)) }
func (h *jobHeap) Pop() any {
	old := *h
	last := old[len(old)-1]
	*h = old[:len(old)-1]
	return last
}
