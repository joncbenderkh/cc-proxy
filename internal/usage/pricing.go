// SPDX-License-Identifier: GPL-3.0-or-later

package usage

import (
	"math"
	"strings"
)

// PricesCheckedOn is the date the rates below were last compared with
// https://platform.claude.com/docs/en/about-claude/pricing (Claude API,
// standard service tier; batch and partner-cloud pricing are not
// modeled).
const PricesCheckedOn = "2026-09-23"

// Rates are USD per million tokens.
type Rates struct {
	Input        float64
	CacheWrite5m float64
	CacheWrite1h float64
	CacheRead    float64
	Output       float64
	// FastMultiplier scales every rate for speed "fast"; zero means the
	// model has no fast-mode price.
	FastMultiplier float64
}

const (
	webSearchUSD      = 10.0 / 1000
	usInferenceFactor = 1.1
)

var prices = map[string]Rates{
	"claude-fable-5-1":  {10, 12.50, 20, 0.25, 50, 0},
	"claude-mythos-5-1": {10, 12.50, 20, 0.25, 50, 0},
	"claude-fable-5":    {10, 12.50, 20, 1, 50, 0},
	"claude-mythos-5":   {10, 12.50, 20, 1, 50, 0},
	"claude-opus-5-5":   {4, 5, 8, 0.20, 20, 2},
	"claude-opus-5":     {5, 6.25, 10, 0.50, 25, 2},
	"claude-opus-4-8":   {5, 6.25, 10, 0.50, 25, 2},
	"claude-opus-4-7":   {5, 6.25, 10, 0.50, 25, 0},
	"claude-opus-4-6":   {5, 6.25, 10, 0.50, 25, 0},
	"claude-opus-4-5":   {5, 6.25, 10, 0.50, 25, 0},
	"claude-opus-4-1":   {15, 18.75, 30, 1.50, 75, 0},
	"claude-opus-4":     {15, 18.75, 30, 1.50, 75, 0},
	"claude-opus-4-0":   {15, 18.75, 30, 1.50, 75, 0},
	"claude-sonnet-5":   {2, 2.50, 4, 0.20, 10, 0},
	"claude-sonnet-4-6": {3, 3.75, 6, 0.30, 15, 0},
	"claude-sonnet-4-5": {3, 3.75, 6, 0.30, 15, 0},
	"claude-sonnet-4":   {3, 3.75, 6, 0.30, 15, 0},
	"claude-sonnet-4-0": {3, 3.75, 6, 0.30, 15, 0},
	"claude-haiku-4-5":  {1, 1.25, 2, 0.10, 5, 0},
	"claude-3-5-haiku":  {0.80, 1, 1.60, 0.08, 4, 0},
}

// Cost returns the USD cost of usage on model, or false when the model (or
// its fast mode) has no known price. Without a cache_creation breakdown,
// cache writes are priced at the 5-minute rate.
func Cost(model string, u Usage) (float64, bool) {
	rates, ok := lookup(model)
	if !ok {
		return 0, false
	}
	scale := 1.0
	if u.Speed == "fast" {
		if rates.FastMultiplier == 0 {
			return 0, false
		}
		scale = rates.FastMultiplier
	}
	if u.InferenceGeo == "us" {
		scale *= usInferenceFactor
	}
	write5m, write1h := u.CacheCreationInputTokens, int64(0)
	if u.CacheCreation != nil {
		write5m, write1h = u.CacheCreation.Ephemeral5mInputTokens, u.CacheCreation.Ephemeral1hInputTokens
	}
	perMTok := float64(u.InputTokens)*rates.Input +
		float64(write5m)*rates.CacheWrite5m +
		float64(write1h)*rates.CacheWrite1h +
		float64(u.CacheReadInputTokens)*rates.CacheRead +
		float64(u.OutputTokens)*rates.Output
	cost := perMTok / 1e6 * scale
	if u.ServerToolUse != nil {
		cost += float64(u.ServerToolUse.WebSearchRequests) * webSearchUSD
	}
	return math.Round(cost*1e9) / 1e9, true
}

// lookup matches a model id exactly or as a dated snapshot of a priced
// alias ("claude-haiku-4-5-20251001"), so an unknown newer model is never
// priced as an older one.
func lookup(model string) (Rates, bool) {
	if rates, ok := prices[model]; ok {
		return rates, true
	}
	alias, date, ok := cutLast(model, "-")
	if !ok || len(date) != 8 || strings.Trim(date, "0123456789") != "" {
		return Rates{}, false
	}
	rates, ok := prices[alias]
	return rates, ok
}

func cutLast(s, sep string) (before, after string, found bool) {
	if i := strings.LastIndex(s, sep); i >= 0 {
		return s[:i], s[i+len(sep):], true
	}
	return s, "", false
}
