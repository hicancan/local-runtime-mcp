package config

import (
	"errors"
	"net"
	"net/url"
	"strings"

	"golang.org/x/net/publicsuffix"
)

// SiteOf names the site a web grant covers for one page origin: scheme plus registrable domain, so every subdomain of
// one service shares a grant. A local name, an IP address or an explicit port stays its exact origin.
func SiteOf(origin string) (string, error) {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" || u.User != nil || strings.ContainsAny(u.Host, "*\\") || u.Scheme != "https" && u.Scheme != "http" || origin != u.Scheme+"://"+u.Host {
		return "", errors.New("a site needs an exact HTTP(S) origin")
	}
	host := u.Hostname()
	if u.Port() != "" || net.ParseIP(host) != nil || !strings.Contains(host, ".") {
		return origin, nil
	}
	domain, err := publicsuffix.EffectiveTLDPlusOne(strings.ToLower(host))
	if err != nil {
		return "", errors.New("a site needs a registrable domain")
	}
	return u.Scheme + "://" + domain, nil
}

// ValidSite reports whether s is already the site it names, as a grant must be.
func ValidSite(s string) bool {
	site, err := SiteOf(s)
	return err == nil && site == s
}
