// SPDX-License-Identifier: GPL-3.0-or-later

// Package usage extracts the model, token usage and cost of one Messages
// API response, streamed or not. It performs no I/O.
package usage

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/joncbenderkh/cc-proxy/internal/sse"
)

// CacheCreation splits cache_creation_input_tokens by cache lifetime.
type CacheCreation struct {
	Ephemeral5mInputTokens int64 `json:"ephemeral_5m_input_tokens"`
	Ephemeral1hInputTokens int64 `json:"ephemeral_1h_input_tokens"`
}

// ServerToolUse counts server-side tool calls billed per use.
type ServerToolUse struct {
	WebSearchRequests int64 `json:"web_search_requests"`
}

// Usage mirrors the usage object of a Messages API response.
// InputTokens is the uncached remainder of the prompt only.
type Usage struct {
	InputTokens              int64          `json:"input_tokens"`
	OutputTokens             int64          `json:"output_tokens"`
	CacheCreationInputTokens int64          `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64          `json:"cache_read_input_tokens"`
	CacheCreation            *CacheCreation `json:"cache_creation,omitempty"`
	ServerToolUse            *ServerToolUse `json:"server_tool_use,omitempty"`
	Speed                    string         `json:"speed,omitempty"`
	ServiceTier              string         `json:"service_tier,omitempty"`
	InferenceGeo             string         `json:"inference_geo,omitempty"`
}

// Message is what one response reports about the turn it answered.
// CostUSD is nil when the model has no known price.
type Message struct {
	ID         string   `json:"id"`
	Model      string   `json:"model"`
	StopReason string   `json:"stop_reason,omitempty"`
	Usage      Usage    `json:"usage"`
	CostUSD    *float64 `json:"cost_usd,omitempty"`
}

var errNoMessage = errors.New("no message in response")

// FromJSON reads a non-streaming Messages API response body.
func FromJSON(body []byte) (Message, error) {
	var message struct {
		Type string `json:"type"`
		Message
	}
	if err := json.Unmarshal(body, &message); err != nil {
		return Message{}, err
	}
	if message.Type != "message" {
		return Message{}, errNoMessage
	}
	return priced(message.Message), nil
}

// FromStream reads the events of a streaming Messages API response. The
// usage in message_delta events is cumulative and overrides the counts
// from message_start field by field. A truncated stream yields whatever
// it reported before it ended.
func FromStream(events []sse.Event) (Message, error) {
	var message Message
	started := false
	for _, event := range events {
		switch event.Name {
		case "message_start":
			var start struct {
				Message Message `json:"message"`
			}
			if err := json.Unmarshal([]byte(event.Data), &start); err != nil {
				return Message{}, fmt.Errorf("message_start: %w", err)
			}
			message, started = start.Message, true
		case "message_delta":
			var delta struct {
				Delta struct {
					StopReason string `json:"stop_reason"`
				} `json:"delta"`
				Usage json.RawMessage `json:"usage"`
			}
			if err := json.Unmarshal([]byte(event.Data), &delta); err != nil {
				return Message{}, fmt.Errorf("message_delta: %w", err)
			}
			if delta.Delta.StopReason != "" {
				message.StopReason = delta.Delta.StopReason
			}
			if delta.Usage != nil {
				if err := json.Unmarshal(delta.Usage, &message.Usage); err != nil {
					return Message{}, fmt.Errorf("message_delta usage: %w", err)
				}
			}
		}
	}
	if !started {
		return Message{}, errNoMessage
	}
	return priced(message), nil
}

func priced(message Message) Message {
	message.CostUSD = nil
	if cost, ok := Cost(message.Model, message.Usage); ok {
		message.CostUSD = &cost
	}
	return message
}
