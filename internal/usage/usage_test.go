// SPDX-License-Identifier: GPL-3.0-or-later

package usage

import (
	"os"
	"reflect"
	"testing"

	"github.com/joncbenderkh/cc-proxy/internal/sse"
)

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	body, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func cost(usd float64) *float64 { return &usd }

func TestFromJSON(t *testing.T) {
	tests := []struct {
		name    string
		body    []byte
		want    Message
		wantErr bool
	}{
		{"recorded message", readFixture(t, "message.json"), Message{
			ID: "msg_01Haiku", Model: "claude-haiku-4-5-20251001", StopReason: "end_turn",
			Usage: Usage{InputTokens: 1000, OutputTokens: 500, CacheCreationInputTokens: 2000,
				CacheReadInputTokens: 10000, ServiceTier: "standard"},
			CostUSD: cost(0.007),
		}, false},
		{"unpriced model", []byte(`{"type":"message","id":"msg_1","model":"claude-next-9","usage":{"input_tokens":1}}`),
			Message{ID: "msg_1", Model: "claude-next-9", Usage: Usage{InputTokens: 1}}, false},
		{"error body", []byte(`{"type":"error","error":{"type":"overloaded_error"}}`), Message{}, true},
		{"not json", []byte("upstream timeout"), Message{}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := FromJSON(tt.body)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got  %+v\nwant %+v", got, tt.want)
			}
		})
	}
}

func TestFromStream(t *testing.T) {
	events := sse.Parse(readFixture(t, "stream.sse"))
	opus := Message{
		ID: "msg_011CfKrYBZGXtEpo53rrZrkx", Model: "claude-opus-5-5", StopReason: "end_turn",
		Usage: Usage{InputTokens: 96, OutputTokens: 89, CacheCreationInputTokens: 2415, CacheReadInputTokens: 156784,
			CacheCreation: &CacheCreation{Ephemeral1hInputTokens: 2415},
			ServiceTier:   "standard", InferenceGeo: "not_available"},
		CostUSD: cost(0.0528408),
	}
	truncated := opus
	truncated.StopReason, truncated.Usage.OutputTokens, truncated.CostUSD = "", 2, cost(0.0511008)

	tests := []struct {
		name    string
		events  []sse.Event
		want    Message
		wantErr bool
	}{
		{"recorded stream", events, opus, false},
		{"truncated after message_start", events[:1], truncated, false},
		{"no message_start", events[1:], Message{}, true},
		{"error event only", []sse.Event{{Name: "error", Data: `{"type":"error"}`}}, Message{}, true},
		{"malformed message_delta", append(events[:1:1], sse.Event{Name: "message_delta", Data: "{"}), Message{}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := FromStream(tt.events)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got  %+v\nwant %+v", got, tt.want)
			}
		})
	}
}
