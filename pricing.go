//go:build windows

package main

import (
	"encoding/json"
	"fmt"
	"sort"
)

type PriceEntry struct {
	Category        string `json:"category"`
	Players         int    `json:"players"`
	DurationMinutes int    `json:"duration_minutes"`
	Amount          int    `json:"amount"`
	Note            string `json:"note,omitempty"`
}

type PriceCatalog struct {
	Currency string       `json:"currency"`
	Entries  []PriceEntry `json:"entries"`
	Notes    []string     `json:"notes,omitempty"`
}

var paradisePriceCatalog = PriceCatalog{
	Currency: "INR",
	Notes: []string{
		"Prices are based on the supplied Paradise Gaming price-list image.",
		"PS5 weekend text in the source image is retained as a note only; no automatic weekend rule is applied.",
		"Billing uses exact listed durations only. Unlisted durations are rejected instead of inferred.",
	},
	Entries: []PriceEntry{
		{Category: "PC Pro", Players: 1, DurationMinutes: 15, Amount: 20},
		{Category: "PC Pro", Players: 2, DurationMinutes: 15, Amount: 25},
		{Category: "PC Pro", Players: 3, DurationMinutes: 15, Amount: 30},
		{Category: "PC Pro", Players: 4, DurationMinutes: 15, Amount: 35},
		{Category: "PC Pro", Players: 1, DurationMinutes: 30, Amount: 40},
		{Category: "PC Pro", Players: 2, DurationMinutes: 30, Amount: 50},
		{Category: "PC Pro", Players: 3, DurationMinutes: 30, Amount: 60},
		{Category: "PC Pro", Players: 4, DurationMinutes: 30, Amount: 70},
		{Category: "PC Pro", Players: 1, DurationMinutes: 60, Amount: 70},
		{Category: "PC Pro", Players: 2, DurationMinutes: 60, Amount: 90},
		{Category: "PC Pro", Players: 3, DurationMinutes: 60, Amount: 110},
		{Category: "PC Pro", Players: 4, DurationMinutes: 60, Amount: 130},

		{Category: "PC Lite", Players: 1, DurationMinutes: 15, Amount: 15},
		{Category: "PC Lite", Players: 2, DurationMinutes: 15, Amount: 20},
		{Category: "PC Lite", Players: 3, DurationMinutes: 15, Amount: 25},
		{Category: "PC Lite", Players: 4, DurationMinutes: 15, Amount: 30},
		{Category: "PC Lite", Players: 1, DurationMinutes: 30, Amount: 30},
		{Category: "PC Lite", Players: 2, DurationMinutes: 30, Amount: 40},
		{Category: "PC Lite", Players: 3, DurationMinutes: 30, Amount: 50},
		{Category: "PC Lite", Players: 4, DurationMinutes: 30, Amount: 60},
		{Category: "PC Lite", Players: 1, DurationMinutes: 60, Amount: 50},
		{Category: "PC Lite", Players: 2, DurationMinutes: 60, Amount: 70},
		{Category: "PC Lite", Players: 3, DurationMinutes: 60, Amount: 90},
		{Category: "PC Lite", Players: 4, DurationMinutes: 60, Amount: 110},

		{Category: "PS5", Players: 1, DurationMinutes: 15, Amount: 30},
		{Category: "PS5", Players: 2, DurationMinutes: 15, Amount: 40},
		{Category: "PS5", Players: 1, DurationMinutes: 30, Amount: 60},
		{Category: "PS5", Players: 2, DurationMinutes: 30, Amount: 80},
		{Category: "PS5", Players: 1, DurationMinutes: 60, Amount: 80, Note: "Source image also says weekends 90 per hour."},
		{Category: "PS5", Players: 2, DurationMinutes: 60, Amount: 120, Note: "Source image also says weekends 90 per hour."},

		{Category: "Racing Lite", Players: 1, DurationMinutes: 15, Amount: 15},
		{Category: "Racing Lite", Players: 1, DurationMinutes: 30, Amount: 30},
		{Category: "Racing Lite", Players: 1, DurationMinutes: 60, Amount: 50},

		{Category: "Racing Pro", Players: 1, DurationMinutes: 15, Amount: 25},
		{Category: "Racing Pro", Players: 1, DurationMinutes: 30, Amount: 50},
		{Category: "Racing Pro", Players: 1, DurationMinutes: 60, Amount: 90},
	},
}

func pricingCatalog() PriceCatalog {
	catalog := paradisePriceCatalog
	catalog.Entries = append([]PriceEntry(nil), paradisePriceCatalog.Entries...)
	sort.Slice(catalog.Entries, func(i, j int) bool {
		left, right := catalog.Entries[i], catalog.Entries[j]
		if left.Category != right.Category {
			return left.Category < right.Category
		}
		if left.Players != right.Players {
			return left.Players < right.Players
		}
		return left.DurationMinutes < right.DurationMinutes
	})
	return catalog
}

func lookupPrice(category string, players, durationMinutes int) (PriceEntry, error) {
	for _, entry := range paradisePriceCatalog.Entries {
		if entry.Category == category && entry.Players == players && entry.DurationMinutes == durationMinutes {
			return entry, nil
		}
	}
	return PriceEntry{}, fmt.Errorf("no listed price for %s, %d player(s), %d minutes", category, players, durationMinutes)
}

func pricingSnapshot(entry PriceEntry) string {
	data, err := json.Marshal(entry)
	if err != nil {
		return "{}"
	}
	return string(data)
}
