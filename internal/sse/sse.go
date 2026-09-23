// SPDX-License-Identifier: GPL-3.0-or-later

// Package sse parses a complete server-sent event stream.
package sse

import "strings"

// Event is one dispatched server-sent event. Data joins multi-line data
// fields with "\n".
type Event struct {
	Name string
	Data string
}

// Parse splits stream into events. Comments, events without data and
// unknown fields are dropped; LF and CRLF line endings are accepted.
func Parse(stream []byte) []Event {
	events := []Event{}
	var name string
	var data []string
	dispatch := func() {
		if len(data) > 0 {
			events = append(events, Event{Name: name, Data: strings.Join(data, "\n")})
		}
		name, data = "", nil
	}
	for _, line := range strings.Split(string(stream), "\n") {
		line = strings.TrimSuffix(line, "\r")
		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch {
		case line == "":
			dispatch()
		case field == "event":
			name = value
		case field == "data":
			data = append(data, value)
		}
	}
	dispatch()
	return events
}
