// SPDX-License-Identifier: GPL-3.0-or-later

package sse

import (
	"reflect"
	"testing"
)

func TestParse(t *testing.T) {
	tests := []struct {
		name   string
		stream string
		want   []Event
	}{
		{"empty", "", []Event{}},
		{"single", "event: ping\ndata: {}\n\n", []Event{{"ping", "{}"}}},
		{"unterminated last event", "event: a\ndata: 1", []Event{{"a", "1"}}},
		{"crlf", "event: a\r\ndata: 1\r\n\r\n", []Event{{"a", "1"}}},
		{"multi-line data", "data: x\ndata: y\n\n", []Event{{"", "x\ny"}}},
		{"comment and no data", ": keep-alive\n\nevent: lonely\n\n", []Event{}},
		{"value without space", "event:a\ndata:1\n\n", []Event{{"a", "1"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Parse([]byte(tt.stream)); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}
