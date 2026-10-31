//go:build windows

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

const (
	minimumSessionCharge     = 15
	maxBilledDurationMinutes = 1440
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

// PricingQuote is the server-authoritative result used by both the dashboard
// and the billing store. DurationMinutes and Amount are always the values that
// will be persisted for the session.
type PricingQuote struct {
	Category        string `json:"category"`
	Players         int    `json:"players"`
	DurationMinutes int    `json:"duration_minutes"`
	Amount          int    `json:"amount"`
	Listed          bool   `json:"listed"`
	Note            string `json:"note,omitempty"`
}

var paradisePriceCatalog = PriceCatalog{
	Currency: "INR",
	Notes: []string{
		"Prices are based on the supplied Paradise Gaming price-list image.",
		"PS5 weekend text in the source image is retained as a note only; no automatic weekend rule is applied.",
		"Listed 15, 30, and 60 minute packages always use their exact prices.",
		"Custom durations use the least-cost combination of listed packages; a remainder under 15 minutes uses the 15-minute package rate, rounded up, with a ₹15 minimum charge.",
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
	quote, err := quoteForDuration(category, players, durationMinutes)
	if err != nil {
		return PriceEntry{}, err
	}
	return PriceEntry{
		Category: quote.Category, Players: quote.Players,
		DurationMinutes: quote.DurationMinutes, Amount: quote.Amount,
		Note: quote.Note,
	}, nil
}

func quoteForDuration(category string, players, durationMinutes int) (PricingQuote, error) {
	if durationMinutes < 1 {
		return PricingQuote{}, fmt.Errorf("duration must be at least 1 minute")
	}
	per15, err := listedPrice(category, players, 15)
	if err != nil {
		return PricingQuote{}, err
	}
	for _, packageMinutes := range []int{15, 30, 60} {
		if entry, err := listedPrice(category, players, packageMinutes); err == nil && durationMinutes == packageMinutes {
			return PricingQuote{Category: category, Players: players, DurationMinutes: durationMinutes, Amount: entry.Amount, Listed: true}, nil
		}
	}

	// First find the least-cost exact package combination for every whole
	// package-sized prefix. The 15-minute package is deliberately not treated
	// as a universal per-minute rate: its exact listed price remains available
	// as a package and is only used to price a remainder below 15 minutes.
	const inf = int(^uint(0) >> 1)
	best := make([]int, durationMinutes+1)
	for i := range best {
		best[i] = inf
	}
	best[0] = 0
	for minutes := 0; minutes <= durationMinutes; minutes++ {
		if best[minutes] == inf {
			continue
		}
		for _, packageMinutes := range []int{15, 30, 60} {
			if minutes+packageMinutes > durationMinutes {
				continue
			}
			entry, err := listedPrice(category, players, packageMinutes)
			if err != nil {
				continue
			}
			if candidate := best[minutes] + entry.Amount; candidate < best[minutes+packageMinutes] {
				best[minutes+packageMinutes] = candidate
			}
		}
	}

	if _, err := listedPrice(category, players, 15); err != nil {
		return PricingQuote{}, err
	}
	amount := inf
	for prefix := 0; prefix <= durationMinutes; prefix++ {
		if best[prefix] == inf {
			continue
		}
		remainder := durationMinutes - prefix
		if remainder >= 15 {
			continue
		}
		candidate := best[prefix] + ceilDiv(remainder*per15.Amount, 15)
		if candidate < amount {
			amount = candidate
		}
	}
	if amount == inf {
		return PricingQuote{}, fmt.Errorf("could not calculate a price for %s, %d player(s), %d minutes", category, players, durationMinutes)
	}
	if amount < minimumSessionCharge {
		amount = minimumSessionCharge
	}
	return PricingQuote{
		Category: category, Players: players, DurationMinutes: durationMinutes,
		Amount: amount, Listed: false,
		Note: "Custom duration: least-cost listed-package combination plus a rounded 15-minute-rate remainder.",
	}, nil
}

func listedPrice(category string, players, durationMinutes int) (PriceEntry, error) {
	for _, entry := range paradisePriceCatalog.Entries {
		if entry.Category == category && entry.Players == players && entry.DurationMinutes == durationMinutes {
			return entry, nil
		}
	}
	return PriceEntry{}, fmt.Errorf("no listed price for %s, %d player(s), %d minutes", category, players, durationMinutes)
}

func ceilDiv(numerator, denominator int) int {
	if numerator <= 0 {
		return 0
	}
	return (numerator + denominator - 1) / denominator
}

// durationForAmount is the inverse of quoteForDuration. It chooses the
// longest duration purchasable with the entered amount, so a manually entered
// amount never causes the backend to charge more than the operator entered.
func durationForAmount(category string, players, amount int) (PricingQuote, error) {
	if amount < minimumSessionCharge {
		return PricingQuote{}, fmt.Errorf("minimum session charge is ₹%d", minimumSessionCharge)
	}
	_, err := listedPrice(category, players, 15)
	if err != nil {
		return PricingQuote{}, err
	}
	// The dashboard and session API intentionally cap one purchase/extension
	// at 24 hours. This also keeps inverse quotes bounded for very large manual
	// amounts while preserving the same package rule.
	maxMinutes := maxBilledDurationMinutes
	bestMinutes := 0
	for minutes := 1; minutes <= maxMinutes; minutes++ {
		quote, quoteErr := quoteForDuration(category, players, minutes)
		if quoteErr == nil && quote.Amount <= amount && minutes > bestMinutes {
			bestMinutes = minutes
		}
	}
	if bestMinutes == 0 {
		return PricingQuote{}, fmt.Errorf("amount must purchase at least 1 minute")
	}
	quote, err := quoteForDuration(category, players, bestMinutes)
	if err != nil {
		return PricingQuote{}, err
	}
	quote.Amount = amount
	quote.Listed = false
	quote.Note = "Manual amount: longest duration available without exceeding the entered amount."
	return quote, nil
}

var assignedPCCategoryPattern = regexp.MustCompile(`(?i)\bpc\s*[-_ ]?([1-4])(?:\D|$)`)

// assignedCategoryForPC is intentionally derived from the existing registry
// name rather than stored as editable PC configuration. This keeps the fixed
// PC 1–4 and PS5 assignments intact across every billing device.
func assignedCategoryForPC(pc PC) string {
	name := strings.TrimSpace(strings.ToLower(pc.Name))
	if strings.Contains(name, "ps5") {
		return "PS5"
	}
	if strings.Contains(name, "racing pro") {
		return "Racing Pro"
	}
	if strings.Contains(name, "racing lite") || strings.Contains(name, "racing simulator") {
		return "Racing Lite"
	}
	match := assignedPCCategoryPattern.FindStringSubmatch(name)
	if len(match) == 2 {
		switch match[1] {
		case "1", "2":
			return "PC Pro"
		case "3", "4":
			return "PC Lite"
		}
	}
	return ""
}

func resolveSessionCategory(pc PC, requested string) (string, error) {
	assigned := assignedCategoryForPC(pc)
	requested = strings.TrimSpace(requested)
	if assigned != "" {
		if requested != "" && requested != assigned {
			return "", fmt.Errorf("%s is fixed to the %s pricing category", pc.Name, assigned)
		}
		return assigned, nil
	}
	if requested == "" {
		return "", errors.New("pricing category is required for this gaming system")
	}
	for _, entry := range paradisePriceCatalog.Entries {
		if entry.Category == requested {
			return requested, nil
		}
	}
	return "", fmt.Errorf("unknown pricing category %q", requested)
}

func pricingSnapshot(entry PriceEntry) string {
	data, err := json.Marshal(entry)
	if err != nil {
		return "{}"
	}
	return string(data)
}
