package ip_filtering

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"

	"github.com/mattermost/mattermost/server/public/model"
)

func (s *Service) enforce(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.enabled() {
			next.ServeHTTP(w, r)
			return
		}
		if s.initErr != nil {
			http.Error(w, "IP filtering is unavailable", http.StatusServiceUnavailable)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), policyTimeout)
		policy, err := s.readPolicy(ctx)
		cancel()
		if err != nil {
			http.Error(w, "IP filtering is unavailable", http.StatusServiceUnavailable)
			return
		}
		clientIP, ok := s.clientIP(r)
		if !ok || !policyAllows(policy, clientIP) {
			http.Error(w, "IP address is not allowed", http.StatusForbidden)
			return
		}
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/api/v4/ip_filtering") {
			if !preservesClientIP(w, r, clientIP) {
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Service) clientIP(r *http.Request) (netip.Addr, bool) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return netip.Addr{}, false
	}
	remote, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, false
	}
	remote = remote.Unmap()
	for _, proxy := range s.trusted {
		if !proxy.Contains(remote) {
			continue
		}
		values := r.Header.Values(s.header)
		if len(values) != 1 {
			return netip.Addr{}, false
		}
		forwarded := strings.TrimSpace(values[0])
		if forwarded == "" || strings.Contains(forwarded, ",") {
			return netip.Addr{}, false
		}
		client, err := netip.ParseAddr(forwarded)
		if err != nil {
			return netip.Addr{}, false
		}
		return client.Unmap(), true
	}
	return remote, true
}

func policyAllows(policy *model.AllowedIPRanges, ip netip.Addr) bool {
	hasEnabled := false
	for _, item := range *policy {
		if !item.Enabled {
			continue
		}
		hasEnabled = true
		prefix, err := netip.ParsePrefix(item.CIDRBlock)
		if err == nil && prefix.Contains(ip) {
			return true
		}
	}
	return !hasEnabled
}

func preservesClientIP(w http.ResponseWriter, r *http.Request, clientIP netip.Addr) bool {
	const maxPolicyBytes = 64 << 10
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxPolicyBytes+1))
	if err != nil {
		http.Error(w, "Unable to read IP policy", http.StatusBadRequest)
		return false
	}
	if len(raw) > maxPolicyBytes {
		http.Error(w, "IP policy is too large", http.StatusRequestEntityTooLarge)
		return false
	}
	r.Body = io.NopCloser(bytes.NewReader(raw))
	var requested model.AllowedIPRanges
	if json.Unmarshal(raw, &requested) != nil {
		// The native handler decodes only the first JSON value. Allowing it to
		// handle trailing data would bypass this self-lockout check.
		http.Error(w, "Invalid IP policy JSON", http.StatusBadRequest)
		return false
	}
	policy, err := normalizePolicy(&requested)
	if err != nil {
		return true // The service reports invalid CIDR values.
	}
	if !policyAllows(policy, clientIP) {
		http.Error(w, "IP policy would exclude the current administrator", http.StatusBadRequest)
		return false
	}
	return true
}
