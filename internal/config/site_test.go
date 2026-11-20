package config

import "testing"

func TestSiteCoversSubdomainsButKeepsLocalOriginsExact(t *testing.T) {
	for origin, want := range map[string]string{
		"https://mooc2-ans.chaoxing.com": "https://chaoxing.com",
		"https://chaoxing.com":           "https://chaoxing.com",
		"https://studev.hicancan.top":    "https://hicancan.top",
		"https://www.bbc.co.uk":          "https://bbc.co.uk",
		"http://127.0.0.1:4321":          "http://127.0.0.1:4321",
		"http://localhost:4321":          "http://localhost:4321",
		"https://example.com:8443":       "https://example.com:8443",
	} {
		if got, err := SiteOf(origin); err != nil || got != want {
			t.Errorf("SiteOf(%s) = %q, %v; want %q", origin, got, err, want)
		}
	}
	for _, bad := range []string{"https://co.uk", "https://example.com/", "ftp://example.com", "https://*.example.com", "https://user@example.com"} {
		if ValidSite(bad) {
			t.Errorf("%s accepted as a site", bad)
		}
	}
	if ValidSite("https://mooc1.chaoxing.com") {
		t.Error("a subdomain is not itself a site")
	}
}
