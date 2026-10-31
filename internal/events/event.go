// Package events implements the compat /events dedupe logic (§7).
package events

import (
	"encoding/json"
	"regexp"
	"strings"
)

// Kind is how an event affects dedupe state.
type Kind int

const (
	KindOther     Kind = iota // forwarded untouched
	KindHealth                // health_status, subject to dedupe
	KindLifecycle             // start/restart/die/stop/destroy/remove, resets state
)

// Event holds the fields of a raw event that the dedupe logic needs.
type Event struct {
	Kind        Kind
	ContainerID string
	Name        string // Actor.Attributes.name, for logging
	Health      string // empty when Kind == KindHealth but no value was found
}

// rawEvent lists only the fields from §7.3; everything else is ignored.
type rawEvent struct {
	Type         string `json:"Type"`
	Action       string `json:"Action"`
	Status       string `json:"status"`
	ID           string `json:"id"`
	HealthStatus string `json:"HealthStatus"`
	Actor        struct {
		ID         string `json:"ID"`
		Attributes struct {
			Name         string `json:"name"`
			HealthStatus string `json:"health_status"`
		} `json:"Attributes"`
	} `json:"Actor"`
}

var dockerHealthAction = regexp.MustCompile(`^health_status:\s*(\S+)$`)

var lifecycleActions = map[string]bool{
	"start": true, "restart": true, "die": true,
	"stop": true, "destroy": true, "remove": true,
}

// Classify parses only the fields listed in §7.3.
// An error means the bytes weren't a valid event; the caller forwards them raw.
func Classify(raw []byte) (Event, error) {
	var r rawEvent
	if err := json.Unmarshal(raw, &r); err != nil {
		return Event{}, err
	}

	ev := Event{ContainerID: r.Actor.ID, Name: r.Actor.Attributes.Name}
	if ev.ContainerID == "" {
		ev.ContainerID = r.ID
	}
	if r.Type != "container" {
		return ev, nil
	}

	action := r.Action
	if action == "" {
		action = r.Status
	}
	switch {
	case strings.HasPrefix(action, "health_status"):
		ev.Kind = KindHealth
		ev.Health = healthValue(r, action)
	case lifecycleActions[action]:
		ev.Kind = KindLifecycle
	}
	return ev, nil
}

func healthValue(r rawEvent, action string) string {
	if r.HealthStatus != "" {
		return r.HealthStatus
	}
	if m := dockerHealthAction.FindStringSubmatch(action); m != nil {
		return m[1]
	}
	return r.Actor.Attributes.HealthStatus
}
