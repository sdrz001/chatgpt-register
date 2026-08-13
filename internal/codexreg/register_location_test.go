package codexreg

import "testing"

func TestParseRegisterLocationPrefersOpenAIPricingCountry(t *testing.T) {
	log := `
2026-08-12 13:11:16 浏览器后端: cloakbrowser
2026-08-12 13:11:18 出口 IP=198.44.166.3 位置=US/Ashburn 时区=America/New_York
2026-08-13 07:52:21 network fetch GET host=chatgpt.com path=/backend-anon/checkout_pricing_config/configs/DE status=200
`
	got := ParseRegisterLocation(log)
	if got.Country != "DE" || got.IP != "198.44.166.3" || got.City != "Ashburn" || got.Timezone != "America/New_York" {
		t.Fatalf("got=%+v", got)
	}
}

func TestParseRegisterLocationUsesLastPricingCountry(t *testing.T) {
	log := `
path=/backend-anon/checkout_pricing_config/configs/JP status=200
path=/backend-anon/checkout_pricing_config/configs/JP status=200
`
	got := ParseRegisterLocation(log)
	if got.Country != "JP" || got.IP != "" {
		t.Fatalf("got=%+v", got)
	}
}

func TestParseRegisterLocationFallsBackToExitGeo(t *testing.T) {
	got := ParseRegisterLocation("📍 出口 IP=203.0.113.8 位置=JP/Tokyo 时区=Asia/Tokyo (35.6, 139.6)")
	if got.Country != "JP" || got.IP != "203.0.113.8" || got.City != "Tokyo" {
		t.Fatalf("got=%+v", got)
	}
}

func TestMergeExitLocationKeepsGeoWhenPricingOnlyHasCountry(t *testing.T) {
	got := MergeExitLocation(
		ExitLocation{Country: "US", IP: "198.51.100.2", City: "Ashburn"},
		ParseRegisterLocation("path=/backend-anon/checkout_pricing_config/configs/JP status=200"),
	)
	if got.Country != "JP" || got.IP != "198.51.100.2" || got.City != "Ashburn" {
		t.Fatalf("got=%+v", got)
	}
}

func TestFormatExitLocationRoundTripsThroughParser(t *testing.T) {
	line := FormatExitLocation(ExitLocation{Country: "DE", IP: "203.0.113.10", City: "Frankfurt", Timezone: "Europe/Berlin"})
	got := ParseRegisterLocation(line)
	if got.Country != "DE" || got.IP != "203.0.113.10" || got.City != "Frankfurt" || got.Timezone != "Europe/Berlin" {
		t.Fatalf("line=%q got=%+v", line, got)
	}
}
