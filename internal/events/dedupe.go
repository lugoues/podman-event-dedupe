package events

import "time"

const (
	staleAfter    = 24 * time.Hour
	sweepInterval = time.Hour
)

// Decision is the outcome for one event. Reason is for debug logging.
type Decision struct {
	Forward bool
	Reason  string
}

type entry struct {
	status      string
	repeatsLeft int
	lastSeen    time.Time
}

// Deduper holds the state for one downstream connection (§7.4).
// It is not safe for concurrent use.
type Deduper struct {
	repeat     int
	resync     time.Duration
	now        func() time.Time
	state      map[string]*entry
	lastResync time.Time
	lastSweep  time.Time
}

// NewDeduper takes now as a parameter so tests can inject a clock.
func NewDeduper(repeat int, resync time.Duration, now func() time.Time) *Deduper {
	t := now()
	return &Deduper{
		repeat:     repeat,
		resync:     resync,
		now:        now,
		state:      map[string]*entry{},
		lastResync: t,
		lastSweep:  t,
	}
}

// Decide applies the §7.4 table and the §7.5 sweep.
func (d *Deduper) Decide(ev Event) Decision {
	now := d.now()
	d.sweep(now)

	switch ev.Kind {
	case KindLifecycle:
		delete(d.state, ev.ContainerID)
		return Decision{true, "lifecycle"}
	case KindHealth:
	default:
		return Decision{true, "not a health event"}
	}

	if ev.ContainerID == "" || ev.Health == "" {
		return Decision{true, "health event without container id or value"}
	}

	e, ok := d.state[ev.ContainerID]
	if !ok {
		d.state[ev.ContainerID] = &entry{ev.Health, d.repeat, now}
		return Decision{true, "first seen"}
	}
	e.lastSeen = now
	if e.status != ev.Health {
		e.status, e.repeatsLeft = ev.Health, d.repeat
		return Decision{true, "status changed"}
	}
	if e.repeatsLeft > 0 {
		e.repeatsLeft--
		return Decision{true, "repeat after transition"}
	}
	if d.resync > 0 && now.Sub(d.lastResync) >= d.resync {
		d.lastResync = now
		return Decision{true, "resync"}
	}
	return Decision{false, "unchanged"}
}

func (d *Deduper) sweep(now time.Time) {
	if now.Sub(d.lastSweep) < sweepInterval {
		return
	}
	d.lastSweep = now
	for id, e := range d.state {
		if now.Sub(e.lastSeen) >= staleAfter {
			delete(d.state, id)
		}
	}
}
