package events

import (
	"slices"
	"testing"
	"time"
)

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func health(id, status string) Event { return Event{Kind: KindHealth, ContainerID: id, Health: status} }
func lifecycle(id string) Event      { return Event{Kind: KindLifecycle, ContainerID: id} }

func TestDedupeSequences(t *testing.T) {
	tests := []struct {
		name   string
		repeat int
		events []Event
		want   []bool
	}{
		{
			name:   "healthy x5 forwards first two",
			repeat: 1,
			events: []Event{health("a", "healthy"), health("a", "healthy"), health("a", "healthy"), health("a", "healthy"), health("a", "healthy")},
			want:   []bool{true, true, false, false, false},
		},
		{
			name:   "each change is forwarded",
			repeat: 0,
			events: []Event{health("a", "healthy"), health("a", "unhealthy"), health("a", "unhealthy"), health("a", "healthy")},
			want:   []bool{true, true, false, true},
		},
		{
			name:   "start resets state",
			repeat: 0,
			events: []Event{health("a", "healthy"), health("a", "healthy"), lifecycle("a"), health("a", "healthy")},
			want:   []bool{true, false, true, true},
		},
		{
			name:   "containers are independent",
			repeat: 0,
			events: []Event{health("a", "healthy"), health("b", "healthy"), health("a", "healthy"), health("b", "unhealthy")},
			want:   []bool{true, true, false, true},
		},
		{
			name:   "health without value is forwarded",
			repeat: 0,
			events: []Event{health("a", ""), health("a", "")},
			want:   []bool{true, true},
		},
		{
			name:   "non-health events are forwarded",
			repeat: 0,
			events: []Event{{Kind: KindOther, ContainerID: "a"}, {Kind: KindOther, ContainerID: "a"}},
			want:   []bool{true, true},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clock := &fakeClock{t: time.Unix(0, 0)}
			d := NewDeduper(tt.repeat, 0, clock.now)
			var got []bool
			for _, ev := range tt.events {
				got = append(got, d.Decide(ev).Forward)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDedupeDestroyDeletesEntry(t *testing.T) {
	d := NewDeduper(0, 0, time.Now)
	d.Decide(health("a", "healthy"))
	d.Decide(Event{Kind: KindLifecycle, ContainerID: "a"})
	if _, ok := d.state["a"]; ok {
		t.Fatal("entry not deleted")
	}
}

func TestDedupeResync(t *testing.T) {
	clock := &fakeClock{t: time.Unix(0, 0)}
	d := NewDeduper(0, time.Minute, clock.now)

	d.Decide(health("a", "healthy"))
	if d.Decide(health("a", "healthy")).Forward {
		t.Fatal("forwarded before interval")
	}
	clock.advance(time.Minute)
	if got := d.Decide(health("a", "healthy")); !got.Forward || got.Reason != "resync" {
		t.Fatalf("expected resync forward, got %+v", got)
	}
	if d.Decide(health("a", "healthy")).Forward {
		t.Fatal("second forward within the same interval")
	}
}

func TestDedupeSweepsStaleEntries(t *testing.T) {
	clock := &fakeClock{t: time.Unix(0, 0)}
	d := NewDeduper(0, 0, clock.now)

	d.Decide(health("old", "healthy"))
	clock.advance(staleAfter)
	d.Decide(health("new", "healthy"))

	if _, ok := d.state["old"]; ok {
		t.Error("stale entry not swept")
	}
	if _, ok := d.state["new"]; !ok {
		t.Error("fresh entry swept")
	}
}
