package ssrf

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type fakeResolver struct {
	ips []net.IP
	err error
}

func (f fakeResolver) LookupIPAddr(context.Context, string) ([]net.IPAddr, error) {
	if f.err != nil {
		return nil, f.err
	}
	out := make([]net.IPAddr, 0, len(f.ips))
	for _, ip := range f.ips {
		out = append(out, net.IPAddr{IP: ip})
	}
	return out, nil
}

func TestValidateAcceptsPublicHTTPS(t *testing.T) {
	v := &Validator{Resolver: fakeResolver{ips: []net.IP{net.ParseIP("93.184.216.34")}}, Timeout: time.Second}
	u, err := v.Validate(context.Background(), "https://hooks.example.com/path?token=secret")
	if err != nil {
		t.Fatal(err)
	}
	if u.Host != "hooks.example.com" {
		t.Fatalf("host = %q", u.Host)
	}
	if got := AuditURL("https://hooks.example.com/path?token=secret"); got != "https://hooks.example.com/path" {
		t.Fatalf("audit url = %q", got)
	}
}

func TestValidateRejects(t *testing.T) {
	v := &Validator{Resolver: fakeResolver{ips: []net.IP{net.ParseIP("93.184.216.34")}}, Timeout: time.Second}
	cases := []struct {
		url  string
		code string
	}{
		{"http://example.com/hook", "scheme_not_https"},
		{"https://user:pass@example.com/hook", "userinfo_present"},
		{"https://example.com:8443/hook", "port_not_allowed"},
		{"https://localhost/hook", "denied_suffix"},
		{"https://hooks.local/hook", "denied_suffix"},
		{"https://intranet/hook", "host_not_fqdn"},
		{"https://example.com./hook", "trailing_dot"},
		{"https://127.0.0.1/hook", "ip_denied"},
		{"https://10.0.0.5/hook", "ip_denied"},
		{"https://[::1]/hook", "ip_denied"},
		{"https://[::ffff:93.184.216.34]/hook", "ip_denied"},
		{"https://example.com/hook#frag", "fragment_present"},
		{"not a url", "scheme_not_https"},
	}
	for _, tc := range cases {
		_, err := v.Validate(context.Background(), tc.url)
		r, ok := AsReason(err)
		if !ok || r.Code != tc.code {
			t.Fatalf("%s: err = %v, want %s", tc.url, err, tc.code)
		}
	}
}

func TestValidateRejectsMixedDNSAnswers(t *testing.T) {
	v := &Validator{Resolver: fakeResolver{ips: []net.IP{
		net.ParseIP("93.184.216.34"), net.ParseIP("10.1.2.3"),
	}}, Timeout: time.Second}
	if _, err := v.Validate(context.Background(), "https://mixed.example.com/hook"); err == nil {
		t.Fatal("accepted mixed public/private DNS answer")
	}
}

func TestIsGlobalMatrix(t *testing.T) {
	for _, s := range []string{
		"0.0.0.0", "10.1.2.3", "100.64.0.1", "127.0.0.1", "169.254.1.1",
		"172.16.0.1", "192.168.1.1", "198.18.0.1", "224.0.0.1", "255.255.255.255",
		"::1", "::ffff:93.184.216.34", "64:ff9b::7f00:1", "fe80::1", "fc00::1",
		"2001:db8::1", "2002::1", "ff02::1",
	} {
		if IsGlobal(netip.MustParseAddr(s)) {
			t.Fatalf("%s accepted as global", s)
		}
	}
	for _, s := range []string{"1.1.1.1", "93.184.216.34", "2606:4700:4700::1111"} {
		if !IsGlobal(netip.MustParseAddr(s)) {
			t.Fatalf("%s rejected as global", s)
		}
	}
}

func TestSharedConformanceVectors(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "contracts", "ssrf-vectors.json"))
	if err != nil {
		t.Fatalf("read ssrf-vectors.json: %v", err)
	}
	var suite struct {
		Vectors []struct {
			ID          string   `json:"id"`
			Input       string   `json:"input"`
			Stage       string   `json:"stage"`
			ResolvedIPs []string `json:"resolved_ips"`
			Resolution  string   `json:"resolution"`
			Expected    string   `json:"expected"`
			Reason      string   `json:"reason"`
		} `json:"vectors"`
	}
	if err := json.Unmarshal(raw, &suite); err != nil {
		t.Fatalf("parse ssrf-vectors.json: %v", err)
	}
	if len(suite.Vectors) == 0 {
		t.Fatal("no vectors in ssrf-vectors.json")
	}

	for _, vec := range suite.Vectors {
		t.Run(vec.ID, func(t *testing.T) {
			var res fakeResolver
			if vec.Resolution == "error" {
				res.err = errors.New("simulated resolver failure")
			} else {
				for _, s := range vec.ResolvedIPs {
					res.ips = append(res.ips, net.ParseIP(s))
				}
			}
			v := &Validator{Resolver: res, Timeout: time.Second}
			_, err := v.Validate(context.Background(), vec.Input)

			switch vec.Expected {
			case "allow":
				if err != nil {
					t.Fatalf("vec %s (%s): expected allow, got error %v", vec.ID, vec.Input, err)
				}
			case "deny":
				if err == nil {
					t.Fatalf("vec %s (%s): expected deny (%s), got allow", vec.ID, vec.Input, vec.Reason)
				}
				r, ok := AsReason(err)
				if !ok {
					t.Fatalf("vec %s: error %v is not a *Reason", vec.ID, err)
				}
				if r.Code != vec.Reason {
					t.Fatalf("vec %s (%s): reason = %q, want %q", vec.ID, vec.Input, r.Code, vec.Reason)
				}
			case "retry":
				// At registration time DNS failure is denied as dns_error; at delivery time it is retryable.
				if err == nil {
					t.Fatalf("vec %s (%s): expected dns failure, got allow", vec.ID, vec.Input)
				}
				r, ok := AsReason(err)
				if !ok || r.Code != "dns_error" {
					t.Fatalf("vec %s (%s): reason = %v, want dns_error", vec.ID, vec.Input, err)
				}
			default:
				t.Fatalf("unknown expected outcome %q", vec.Expected)
			}
		})
	}
}
