// Package ssrf validates webhook destinations at registration time. The
// worker performs the authoritative connect-time check; this layer exists to
// reject obviously unsafe destinations early with a stable reason code.
package ssrf

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

// Resolver is the DNS dependency. net.DefaultResolver satisfies it.
type Resolver interface {
	LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
}

// Validator validates HTTPS destinations.
type Validator struct {
	Resolver           Resolver
	Timeout            time.Duration
	AllowLocalReceiver bool
}

// Reason is a machine-readable rejection with a client-safe message.
type Reason struct {
	Code   string
	Detail string
}

func (r *Reason) Error() string { return r.Code + ": " + r.Detail }

func reject(code, detail string) error { return &Reason{Code: code, Detail: detail} }

// New returns a validator using the system resolver.
func New() *Validator {
	return &Validator{Resolver: net.DefaultResolver, Timeout: 3 * time.Second}
}

// Validate parses and resolves a destination. It rejects anything that is not
// an unambiguous public HTTPS URL on port 443.
func (v *Validator) Validate(ctx context.Context, raw string) (*url.URL, error) {
	if len(raw) == 0 || len(raw) > 2048 {
		return nil, reject("url_invalid", "URL must be between 1 and 2048 characters.")
	}
	if hasAmbiguousAuthority(raw) {
		return nil, reject("ambiguous_authority", "Destination authority contains ambiguous characters.")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, reject("url_invalid", "URL is not valid.")
	}
	if !strings.EqualFold(u.Scheme, "https") {
		return nil, reject("scheme_not_https", "Destination scheme must be https.")
	}
	if u.Opaque != "" || u.Host == "" {
		return nil, reject("url_invalid", "Destination must be an absolute URL with a host.")
	}
	if u.User != nil {
		return nil, reject("userinfo_present", "Destination must not contain user information.")
	}
	if u.Fragment != "" || strings.Contains(raw, "#") {
		return nil, reject("fragment_present", "Destination must not contain a fragment.")
	}
	if v.AllowLocalReceiver {
		h := strings.ToLower(u.Hostname())
		if h == "receiver" || h == "127.0.0.1" || h == "localhost" {
			return u, nil
		}
	}
	if port := u.Port(); port != "" && port != "443" {
		return nil, reject("port_not_allowed", "Destination port must be 443.")
	}
	rawHost := strings.ToLower(u.Hostname())
	if rawHost == "" {
		return nil, reject("host_missing", "Destination host is missing.")
	}
	if strings.HasSuffix(rawHost, ".") {
		return nil, reject("trailing_dot", "Destination host must not end with a dot.")
	}
	host := rawHost
	for _, r := range host {
		if r > 127 {
			return nil, reject("host_not_allowed", "Destination host must be ASCII.")
		}
	}
	if hasDeniedSuffix(host) {
		return nil, reject("denied_suffix", "Destination host uses a reserved or local suffix.")
	}

	var addrs []netip.Addr
	if ip, err := netip.ParseAddr(host); err == nil {
		if ip.Is4In6() {
			return nil, reject("ip_denied", "IPv4-mapped IPv6 addresses are not permitted.")
		}
		addrs = []netip.Addr{ip}
	} else {
		if strings.Contains(u.Host, "[") || strings.Contains(u.Host, "]") {
			return nil, reject("ip_denied", "Destination IPv6 literal is not permitted.")
		}
		if isNonCanonicalIPv4(host) {
			return nil, reject("ip_denied", "Non-canonical IPv4 address encodings are not permitted.")
		}
		if !strings.Contains(host, ".") {
			return nil, reject("host_not_fqdn", "Destination host must be a fully qualified domain name.")
		}
		timeout := v.Timeout
		if timeout <= 0 {
			timeout = 3 * time.Second
		}
		lookupCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		resolved, err := v.Resolver.LookupIPAddr(lookupCtx, host)
		if err != nil {
			return nil, reject("dns_error", "Destination host could not be resolved.")
		}
		if len(resolved) == 0 {
			return nil, reject("no_addresses", "Destination host resolved to zero addresses.")
		}
		for _, a := range resolved {
			ip, ok := netip.AddrFromSlice(a.IP)
			if !ok {
				return nil, reject("ip_denied", "Destination resolved to an unsupported address.")
			}
			addrs = append(addrs, ip.Unmap())
		}
	}
	if len(addrs) == 0 {
		return nil, reject("no_addresses", "Destination host resolved to zero addresses.")
	}
	for _, ip := range addrs {
		if !IsGlobal(ip) {
			return nil, reject("ip_denied", "Destination resolves to a private, local, or reserved address.")
		}
	}
	return u, nil
}

func hasAmbiguousAuthority(raw string) bool {
	afterScheme := raw
	if idx := strings.Index(raw, "://"); idx >= 0 {
		afterScheme = raw[idx+3:]
	}
	authority := afterScheme
	if idx := strings.IndexAny(afterScheme, "/?#"); idx >= 0 {
		authority = afterScheme[:idx]
	}
	return strings.Contains(authority, `\`) || strings.Contains(authority, "%")
}

var deniedSuffixes = []string{
	".localhost",
	".local",
	".internal",
	".home.arpa",
	".lan",
	".corp",
	".intranet",
	".test",
	".example",
	".invalid",
}

func hasDeniedSuffix(host string) bool {
	if host == "localhost" {
		return true
	}
	for _, suffix := range deniedSuffixes {
		if strings.HasSuffix(host, suffix) {
			return true
		}
	}
	return false
}

// isNonCanonicalIPv4 detects shorthand (127.1), decimal (2130706433), hex
// (0x7f000001), or octal (0177.0.0.1) IPv4 representations where every label
// is numeric or hex-numeric. Valid DNS FQDNs cannot have an all-numeric TLD.
func isNonCanonicalIPv4(host string) bool {
	parts := strings.Split(host, ".")
	if len(parts) == 0 {
		return false
	}
	for _, p := range parts {
		if p == "" {
			return false
		}
		if strings.HasPrefix(p, "0x") && len(p) > 2 {
			for _, r := range p[2:] {
				if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
					return false
				}
			}
			continue
		}
		for _, r := range p {
			if r < '0' || r > '9' {
				return false
			}
		}
	}
	return true
}

// AuditURL removes query and fragment so credentials embedded in query
// parameters never reach the audit log.
func AuditURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}

var deniedV4 = mustPrefixes(
	"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8",
	"169.254.0.0/16", "172.16.0.0/12", "192.0.0.0/24", "192.0.2.0/24",
	"192.88.99.0/24", "192.168.0.0/16", "198.18.0.0/15", "198.51.100.0/24",
	"203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4",
)

var deniedV6 = mustPrefixes(
	"::/128", "::1/128", "::ffff:0:0/96", "64:ff9b::/96", "64:ff9b:1::/48",
	"100::/64", "2001::/32", "2001:1::1/128", "2001:1::2/128", "2001:2::/48",
	"2001:3::/32", "2001:4:112::/48", "2001:10::/28", "2001:20::/28",
	"2001:db8::/32", "2002::/16", "3fff::/20", "5f00::/16",
	"fc00::/7", "fe80::/10", "ff00::/8",
)

var globalV6 = netip.MustParsePrefix("2000::/3")

// IsGlobal reports whether an IP is globally routable for this application.
// It uses explicit deny ranges rather than a country/geo database.
func IsGlobal(ip netip.Addr) bool {
	if !ip.IsValid() || ip.Is4In6() {
		return false
	}
	if ip.IsUnspecified() || ip.IsLoopback() || ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() {
		return false
	}
	if ip.Is4() {
		for _, p := range deniedV4 {
			if p.Contains(ip) {
				return false
			}
		}
		return true
	}
	if !globalV6.Contains(ip) {
		return false
	}
	for _, p := range deniedV6 {
		if p.Contains(ip) {
			return false
		}
	}
	return true
}

func mustPrefixes(raw ...string) []netip.Prefix {
	out := make([]netip.Prefix, 0, len(raw))
	for _, s := range raw {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			panic(fmt.Sprintf("invalid denied prefix %q: %v", s, err))
		}
		out = append(out, p)
	}
	return out
}

// AsReason extracts a Reason from an error chain.
func AsReason(err error) (*Reason, bool) {
	var r *Reason
	if errors.As(err, &r) {
		return r, true
	}
	return nil, false
}
