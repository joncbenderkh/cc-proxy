// SPDX-License-Identifier: GPL-3.0-or-later

// Package transcript extracts the human-readable side of one Messages API
// exchange: the newest prompt of a request and the reply of its response.
// It performs no I/O.
package transcript

import (
	"encoding/json"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/joncbenderkh/cc-proxy/internal/sse"
)

// MaxText bounds the text kept per block, in bytes.
const MaxText = 8 << 10

// Block is one content block, reduced to what a reader needs.
type Block struct {
	Type    string          `json:"type"`
	Text    string          `json:"text,omitempty"`
	Name    string          `json:"name,omitempty"`
	Input   json.RawMessage `json:"input,omitempty"`
	IsError bool            `json:"is_error,omitempty"`
}

type rawBlock struct {
	Type    string          `json:"type"`
	Text    string          `json:"text"`
	Name    string          `json:"name"`
	Input   json.RawMessage `json:"input"`
	Content json.RawMessage `json:"content"`
	IsError bool            `json:"is_error"`
}

// Prompt returns the blocks of the last message of a request when it comes
// from the user. Text blocks that are only harness reminders
// (<system-reminder>) are dropped; tool results keep their text.
func Prompt(request []byte) ([]Block, error) {
	var body struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(request, &body); err != nil {
		return nil, err
	}
	if len(body.Messages) == 0 {
		return nil, errors.New("request has no messages")
	}
	last := body.Messages[len(body.Messages)-1]
	if last.Role != "user" {
		return nil, nil
	}
	blocks := []Block{}
	for _, raw := range contentBlocks(last.Content) {
		switch raw.Type {
		case "text":
			if !strings.HasPrefix(strings.TrimSpace(raw.Text), "<system-reminder>") {
				blocks = append(blocks, Block{Type: "text", Text: truncate(raw.Text)})
			}
		case "tool_result":
			blocks = append(blocks, Block{Type: "tool_result", Text: truncate(joinText(contentBlocks(raw.Content))), IsError: raw.IsError})
		case "image", "document":
			blocks = append(blocks, Block{Type: raw.Type})
		}
	}
	return blocks, nil
}

// ReplyJSON returns the text and tool_use blocks of a non-streaming
// response body.
func ReplyJSON(response []byte) ([]Block, error) {
	var message struct {
		Content []rawBlock `json:"content"`
	}
	if err := json.Unmarshal(response, &message); err != nil {
		return nil, err
	}
	return reply(message.Content), nil
}

// ReplyStream reassembles the text and tool_use blocks of a streaming
// response from its content_block_* events.
func ReplyStream(events []sse.Event) ([]Block, error) {
	var blocks []rawBlock
	var inputs []strings.Builder
	for _, event := range events {
		switch event.Name {
		case "content_block_start":
			var start struct {
				Index        int      `json:"index"`
				ContentBlock rawBlock `json:"content_block"`
			}
			if err := json.Unmarshal([]byte(event.Data), &start); err != nil {
				return nil, err
			}
			for len(blocks) <= start.Index {
				blocks = append(blocks, rawBlock{})
				inputs = append(inputs, strings.Builder{})
			}
			blocks[start.Index] = start.ContentBlock
		case "content_block_delta":
			var delta struct {
				Index int `json:"index"`
				Delta struct {
					Type        string `json:"type"`
					Text        string `json:"text"`
					PartialJSON string `json:"partial_json"`
				} `json:"delta"`
			}
			if err := json.Unmarshal([]byte(event.Data), &delta); err != nil {
				return nil, err
			}
			if delta.Index < 0 || delta.Index >= len(blocks) {
				continue
			}
			switch delta.Delta.Type {
			case "text_delta":
				blocks[delta.Index].Text += delta.Delta.Text
			case "input_json_delta":
				inputs[delta.Index].WriteString(delta.Delta.PartialJSON)
			}
		}
	}
	for i := range blocks {
		if input := inputs[i].String(); input != "" && json.Valid([]byte(input)) {
			blocks[i].Input = json.RawMessage(input)
		}
	}
	return reply(blocks), nil
}

func reply(raw []rawBlock) []Block {
	blocks := []Block{}
	for _, block := range raw {
		switch block.Type {
		case "text":
			blocks = append(blocks, Block{Type: "text", Text: truncate(block.Text)})
		case "tool_use", "server_tool_use":
			input := block.Input
			if len(input) > MaxText {
				input = nil
			}
			blocks = append(blocks, Block{Type: "tool_use", Name: block.Name, Input: input})
		}
	}
	return blocks
}

// contentBlocks accepts content given either as a string or as blocks.
func contentBlocks(content json.RawMessage) []rawBlock {
	var text string
	if json.Unmarshal(content, &text) == nil {
		return []rawBlock{{Type: "text", Text: text}}
	}
	var blocks []rawBlock
	json.Unmarshal(content, &blocks)
	return blocks
}

func joinText(blocks []rawBlock) string {
	var texts []string
	for _, block := range blocks {
		if block.Type == "text" {
			texts = append(texts, block.Text)
		}
	}
	return strings.Join(texts, "\n")
}

func truncate(text string) string {
	if len(text) <= MaxText {
		return text
	}
	cut := MaxText
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut] + "…"
}
