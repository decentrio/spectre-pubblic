package services

import (
	"testing"
	"time"
)

func TestDeriveCosmosRefreshInterval(t *testing.T) {
	trustingPeriod := 16 * time.Hour

	got, err := deriveCosmosRefreshInterval(Config{
		RefreshInterval: DEFAULT_REFRESH_INTERVAL,
	}, trustingPeriod)
	if err != nil {
		t.Fatalf("derive default interval: %v", err)
	}
	if got != 15*time.Hour {
		t.Fatalf("default interval = %s, want 15h", got)
	}

	_, err = deriveCosmosRefreshInterval(Config{
		RefreshInterval:           15 * time.Hour,
		RefreshIntervalConfigured: true,
	}, trustingPeriod)
	if err == nil {
		t.Fatal("expected configured interval at safety boundary to be rejected")
	}

	got, err = deriveCosmosRefreshInterval(Config{
		RefreshInterval:           14 * time.Hour,
		RefreshIntervalConfigured: true,
	}, trustingPeriod)
	if err != nil {
		t.Fatalf("derive configured safe interval: %v", err)
	}
	if got != 14*time.Hour {
		t.Fatalf("configured interval = %s, want 14h", got)
	}
}

func TestParseRotationThreshold(t *testing.T) {
	def, err := ParseRotationThreshold("")
	if err != nil {
		t.Fatalf("empty value should fall back to default: %v", err)
	}
	if def.Numerator != 5 || def.Denominator != 6 {
		t.Fatalf("default = %d/%d, want 5/6", def.Numerator, def.Denominator)
	}

	for _, ok := range []string{"3/4", "1/1", "5/6"} {
		if _, err := ParseRotationThreshold(ok); err != nil {
			t.Fatalf("expected %q to be valid: %v", ok, err)
		}
	}
	for _, bad := range []string{"2/3", "1/2", "7/6", "0/1", "1/0", "abc", "5"} {
		if _, err := ParseRotationThreshold(bad); err == nil {
			t.Fatalf("expected %q to be rejected", bad)
		}
	}
}
