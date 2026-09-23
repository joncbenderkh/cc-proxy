// SPDX-License-Identifier: GPL-3.0-or-later

package usage

import "testing"

func TestCost(t *testing.T) {
	million := Usage{InputTokens: 1_000_000}
	tests := []struct {
		name   string
		model  string
		usage  Usage
		want   float64
		priced bool
	}{
		{"input only", "claude-sonnet-5", million, 2, true},
		{"output only", "claude-sonnet-5", Usage{OutputTokens: 1_000_000}, 10, true},
		{"cache writes without breakdown use 5m rate", "claude-opus-5", Usage{CacheCreationInputTokens: 1_000_000}, 6.25, true},
		{"cache write breakdown", "claude-opus-5",
			Usage{CacheCreationInputTokens: 2_000_000, CacheCreation: &CacheCreation{1_000_000, 1_000_000}}, 16.25, true},
		{"cache reads at model rate", "claude-fable-5-1", Usage{CacheReadInputTokens: 1_000_000}, 0.25, true},
		{"fast mode doubles every rate", "claude-opus-5-5",
			Usage{InputTokens: 1_000_000, OutputTokens: 1_000_000, CacheReadInputTokens: 1_000_000, Speed: "fast"}, 48.4, true},
		{"fast mode without a fast price", "claude-opus-4-7", Usage{InputTokens: 1, Speed: "fast"}, 0, false},
		{"us inference", "claude-haiku-4-5", Usage{InputTokens: 1_000_000, InferenceGeo: "us"}, 1.1, true},
		{"web search", "claude-haiku-4-5", Usage{ServerToolUse: &ServerToolUse{WebSearchRequests: 3}}, 0.03, true},
		{"dated snapshot", "claude-sonnet-4-20250514", million, 3, true},
		{"unknown model", "claude-next-9", million, 0, false},
		{"newer point release is not priced as its base", "claude-opus-5-7", million, 0, false},
		{"non-date suffix", "claude-opus-5-beta", million, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, priced := Cost(tt.model, tt.usage)
			if got != tt.want || priced != tt.priced {
				t.Fatalf("Cost = %v, %v; want %v, %v", got, priced, tt.want, tt.priced)
			}
		})
	}
}
