package foundation

import (
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// PreviewIngress pairs a console origin with a dedicated preview DNS suffix,
// including its independently configured scheme and port.
type PreviewIngress struct {
	ConsoleOrigin string `json:"consoleOrigin"`
	PreviewOrigin string `json:"previewOrigin"`
}

var dnsLabel = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

// NormalizeHTTPOrigin accepts only an HTTP(S) origin, without credentials,
// query, fragment or a path other than an optional trailing slash.
func NormalizeHTTPOrigin(raw string) (string, error) {
	if len(raw) > 2048 || strings.ContainsAny(raw, " \t\r\n\x00") {
		return "", fmt.Errorf("invalid HTTP origin")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Opaque != "" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(raw, "#") || u.RawPath != "" || (u.Path != "" && u.Path != "/") || (u.Scheme != "http" && u.Scheme != "https") {
		return "", fmt.Errorf("expected an HTTP(S) origin")
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if host == "" || len(host) > 253 {
		return "", fmt.Errorf("invalid origin host")
	}
	if net.ParseIP(host) == nil {
		for _, label := range strings.Split(host, ".") {
			if !dnsLabel.MatchString(label) {
				return "", fmt.Errorf("invalid origin host")
			}
		}
	}
	port := u.Port()
	if strings.HasSuffix(u.Host, ":") {
		return "", fmt.Errorf("invalid origin port")
	}
	if port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return "", fmt.Errorf("invalid origin port")
		}
		port = strconv.Itoa(n)
		if u.Scheme == "https" && port == "443" || u.Scheme == "http" && port == "80" {
			port = ""
		}
	}
	authority := host
	if port != "" {
		authority = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		authority = "[" + host + "]"
	}
	return u.Scheme + "://" + authority, nil
}

func parsePreviewIngresses(raw string, secure bool, legacyDomain string) ([]PreviewIngress, error) {
	if raw == "" {
		return nil, nil
	}
	var ingresses []PreviewIngress
	if len(raw) > 64*1024 || json.Unmarshal([]byte(raw), &ingresses) != nil || len(ingresses) == 0 || len(ingresses) > 16 {
		return nil, fmt.Errorf("MIRA_NODE_PREVIEW_INGRESSES must be a JSON array of 1–16 origin mappings (at most 64 KiB)")
	}
	seen := map[string]bool{}
	domains := []string{}
	if legacyDomain != "" {
		domains = append(domains, legacyDomain)
	}
	expectedScheme := "http"
	if secure {
		expectedScheme = "https"
	}
	for i := range ingresses {
		entry := &ingresses[i]
		console, ce := NormalizeHTTPOrigin(entry.ConsoleOrigin)
		preview, pe := NormalizeHTTPOrigin(entry.PreviewOrigin)
		if ce != nil || pe != nil {
			return nil, fmt.Errorf("MIRA_NODE_PREVIEW_INGRESSES entry %d requires valid consoleOrigin and previewOrigin", i+1)
		}
		cu, _ := url.Parse(console)
		pu, _ := url.Parse(preview)
		if cu.Scheme != expectedScheme || pu.Scheme != expectedScheme {
			return nil, fmt.Errorf("MIRA_NODE_PREVIEW_INGRESSES entry %d schemes must agree with MIRA_SECURE_COOKIES", i+1)
		}
		// Leave room for the 20-character site ID and its separating dot.
		if net.ParseIP(pu.Hostname()) != nil || !strings.Contains(pu.Hostname(), ".") || len(pu.Hostname()) > 232 {
			return nil, fmt.Errorf("MIRA_NODE_PREVIEW_INGRESSES entry %d previewOrigin requires a dedicated DNS suffix", i+1)
		}
		if seen[console] {
			return nil, fmt.Errorf("MIRA_NODE_PREVIEW_INGRESSES contains duplicate console origins")
		}
		seen[console] = true
		entry.ConsoleOrigin, entry.PreviewOrigin = console, preview
		domains = append(domains, pu.Hostname())
	}
	for _, entry := range ingresses {
		u, _ := url.Parse(entry.ConsoleOrigin)
		for _, domain := range domains {
			if u.Hostname() == domain || strings.HasSuffix(u.Hostname(), "."+domain) {
				return nil, fmt.Errorf("MIRA_NODE_PREVIEW_INGRESSES preview suffixes must not capture a console hostname")
			}
		}
	}
	return ingresses, nil
}

func (config Config) PreviewIngressForOrigin(raw string) (PreviewIngress, bool) {
	origin, err := NormalizeHTTPOrigin(raw)
	if err != nil {
		return PreviewIngress{}, false
	}
	for _, ingress := range config.PreviewIngresses {
		if ingress.ConsoleOrigin == origin {
			return ingress, true
		}
	}
	return PreviewIngress{}, false
}
