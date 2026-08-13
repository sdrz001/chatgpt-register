package codexreg

import (
	"regexp"
	"strings"
)

var (
	pricingCountryRe = regexp.MustCompile(`checkout_pricing_config/configs/([A-Z]{2})`)
	exitGeoRe        = regexp.MustCompile(`出口 IP=(\S+) 位置=([A-Za-z]{2})/(\S+)(?: 时区=(\S+))?`)
)

// ExitLocation 是注册时的市场/出口位置。
// Country 优先用 OpenAI 定价国家，其次才是出口 GeoIP。
type ExitLocation struct {
	Country  string
	IP       string
	City     string
	Timezone string
}

func DetectProxyExit(proxy string) ExitLocation {
	geo := lookupGeoIPViaRequest(Input{Proxy: proxy})
	if geo == nil {
		return ExitLocation{}
	}
	return ExitLocation{
		Country:  strings.ToUpper(strings.TrimSpace(geo.CountryCode)),
		IP:       strings.TrimSpace(geo.Query),
		City:     strings.TrimSpace(geo.City),
		Timezone: strings.TrimSpace(geo.Timezone),
	}
}

func FormatExitLocation(loc ExitLocation) string {
	if loc.IP == "" && loc.Country == "" {
		return ""
	}
	city := loc.City
	if city == "" {
		city = "-"
	}
	tz := loc.Timezone
	if tz == "" {
		tz = "-"
	}
	return "出口 IP=" + loc.IP + " 位置=" + loc.Country + "/" + city + " 时区=" + tz
}

func ParseRegisterLocation(log string) ExitLocation {
	var loc ExitLocation
	if matches := exitGeoRe.FindAllStringSubmatch(log, -1); len(matches) > 0 {
		last := matches[len(matches)-1]
		loc.IP = last[1]
		loc.Country = strings.ToUpper(last[2])
		loc.City = strings.TrimRight(last[3], "，。;")
		if len(last) > 4 {
			loc.Timezone = last[4]
		}
	}
	if matches := pricingCountryRe.FindAllStringSubmatch(log, -1); len(matches) > 0 {
		loc.Country = matches[len(matches)-1][1]
	}
	return loc
}

func MergeExitLocation(base, overlay ExitLocation) ExitLocation {
	if overlay.Country != "" {
		base.Country = overlay.Country
	}
	if overlay.IP != "" {
		base.IP = overlay.IP
	}
	if overlay.City != "" {
		base.City = overlay.City
	}
	if overlay.Timezone != "" {
		base.Timezone = overlay.Timezone
	}
	return base
}
