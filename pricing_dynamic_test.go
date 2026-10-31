//go:build windows

package main

import "testing"

func TestParadisePricingSupportsOfficialPackagesAndCustomDurations(t *testing.T) {
	catalog := pricingCatalog()
	if len(catalog.Entries) != 36 {
		t.Fatalf("catalog has %d entries; want 36", len(catalog.Entries))
	}
	cases := []struct {
		category      string
		players, mins int
		amount        int
	}{
		{"PC Pro", 1, 60, 70}, {"PC Pro", 4, 15, 35},
		{"PC Lite", 1, 30, 30}, {"PC Lite", 4, 60, 110},
		{"PS5", 1, 60, 80}, {"PS5", 2, 15, 40},
		{"Racing Lite", 1, 60, 50}, {"Racing Pro", 1, 30, 50},
	}
	for _, tc := range cases {
		quote, err := quoteForDuration(tc.category, tc.players, tc.mins)
		if err != nil || quote.Amount != tc.amount || !quote.Listed {
			t.Errorf("%s/%d/%d = %+v, err=%v; want listed ₹%d", tc.category, tc.players, tc.mins, quote, err, tc.amount)
		}
	}
	for _, tc := range []struct {
		category string
		players  int
		mins     int
		amount   int
	}{
		{"PC Lite", 1, 20, 20}, // 15-minute package plus five ₹1/minute remainder.
		{"PC Lite", 1, 45, 45}, // 30 + 15 package combination.
		{"PC Lite", 1, 75, 65}, // 60 + 15 package combination.
		{"PC Pro", 1, 90, 110}, // 60 + 30; not a linear 90-minute rate.
	} {
		quote, err := quoteForDuration(tc.category, tc.players, tc.mins)
		if err != nil || quote.Amount != tc.amount || quote.Listed {
			t.Errorf("custom %s/%d/%d = %+v, err=%v; want ₹%d", tc.category, tc.players, tc.mins, quote, err, tc.amount)
		}
	}
}

func TestParadisePricingAmountInverseAndFixedAssignments(t *testing.T) {
	quote, err := durationForAmount("PC Lite", 1, 20)
	if err != nil || quote.DurationMinutes != 20 || quote.Amount != 20 {
		t.Fatalf("₹20 inverse quote = %+v, err=%v; want 20 minutes", quote, err)
	}
	quote, err = durationForAmount("PC Pro", 1, 70)
	if err != nil || quote.DurationMinutes != 60 || quote.Amount != 70 {
		t.Fatalf("PC Pro ₹70 inverse quote = %+v, err=%v; want 60 minutes", quote, err)
	}
	for _, tc := range []struct {
		name, want string
	}{
		{"PC 1", "PC Pro"}, {"PC2-PG-SKV", "PC Pro"},
		{"PC3", "PC Lite"}, {"PC-4", "PC Lite"}, {"PS5", "PS5"},
	} {
		if got := assignedCategoryForPC(PC{Name: tc.name}); got != tc.want {
			t.Errorf("assigned category for %q = %q; want %q", tc.name, got, tc.want)
		}
	}
}
