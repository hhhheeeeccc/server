package intune

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"time"
)

var errKeyService = errors.New("Microsoft signing keys unavailable")

type keyCache struct {
	keys       map[string]*rsa.PublicKey
	refreshed  time.Time
	attempted  time.Time
	refreshing chan struct{}
}

func issuerFor(tenant, version string) string {
	if version == "1.0" {
		return "https://sts.windows.net/" + tenant + "/"
	}
	return "https://login.microsoftonline.com/" + tenant + "/v2.0"
}

func trustedEndpoint(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Host == "login.microsoftonline.com" && u.User == nil && u.Fragment == ""
}

func (s *Service) getJSON(ctx context.Context, address string, value any) error {
	if !trustedEndpoint(address) {
		return errKeyService
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return errKeyService
	}
	response, err := s.client.Do(req)
	if err != nil {
		return errKeyService
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return errKeyService
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil || len(raw) > 1<<20 || json.Unmarshal(raw, value) != nil {
		return errKeyService
	}
	return nil
}

func (s *Service) loadKeys(ctx context.Context, tenant, version string) (map[string]*rsa.PublicKey, error) {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	address := "https://login.microsoftonline.com/" + tenant
	if version == "2.0" {
		address += "/v2.0"
	}
	address += "/.well-known/openid-configuration"
	var metadata struct {
		Issuer  string `json:"issuer"`
		KeysURL string `json:"jwks_uri"`
	}
	if err := s.getJSON(ctx, address, &metadata); err != nil {
		return nil, err
	}
	if metadata.Issuer != issuerFor(tenant, version) {
		return nil, errKeyService
	}
	var document struct {
		Keys []struct {
			ID        string `json:"kid"`
			Type      string `json:"kty"`
			Use       string `json:"use"`
			Algorithm string `json:"alg"`
			N         string `json:"n"`
			E         string `json:"e"`
			Issuer    string `json:"issuer"`
		} `json:"keys"`
	}
	if err := s.getJSON(ctx, metadata.KeysURL, &document); err != nil {
		return nil, err
	}
	if len(document.Keys) > 1000 {
		return nil, errKeyService
	}
	keys := make(map[string]*rsa.PublicKey)
	for _, item := range document.Keys {
		if item.Type != "RSA" || (item.Use != "" && item.Use != "sig") || (item.Algorithm != "" && item.Algorithm != "RS256") {
			continue
		}
		if item.Issuer != "" && strings.ReplaceAll(item.Issuer, "{tenantid}", tenant) != metadata.Issuer {
			continue
		}
		n, nErr := base64.RawURLEncoding.DecodeString(item.N)
		e, eErr := base64.RawURLEncoding.DecodeString(item.E)
		if nErr != nil || eErr != nil || len(e) == 0 || len(e) > 4 || len(n) < 256 || len(n) > 1024 || item.ID == "" || len(item.ID) > 256 {
			return nil, errKeyService
		}
		exponent := new(big.Int).SetBytes(e).Int64()
		if exponent < 3 || exponent > 2147483647 || exponent%2 == 0 || keys[item.ID] != nil {
			return nil, errKeyService
		}
		modulus := new(big.Int).SetBytes(n)
		if modulus.BitLen() < 2048 {
			return nil, errKeyService
		}
		keys[item.ID] = &rsa.PublicKey{N: modulus, E: int(exponent)}
	}
	if len(keys) == 0 {
		return nil, errKeyService
	}
	return keys, nil
}

func (s *Service) signingKey(ctx context.Context, tenant, version, kid string) (*rsa.PublicKey, error) {
	for {
		s.mu.Lock()
		if err := ctx.Err(); err != nil {
			s.mu.Unlock()
			return nil, errKeyService
		}
		cacheID := tenant + ":" + version + ":public"
		entry := s.keys[cacheID]
		if entry == nil {
			// Keep only the current tenant's two protocol-version caches.
			for id := range s.keys {
				if !strings.HasPrefix(id, tenant+":") {
					delete(s.keys, id)
				}
			}
			entry = &keyCache{}
			s.keys[cacheID] = entry
		}
		now := time.Now()
		key := entry.keys[kid]
		if key != nil && now.Sub(entry.refreshed) < time.Hour {
			s.mu.Unlock()
			return key, nil
		}
		if entry.refreshing != nil {
			// Keep serving a recently trusted key during rotation, but a cold or
			// unknown key must await the current fetch rather than fail just because
			// another login started it first. Never wait while holding the mutex.
			if key != nil && now.Sub(entry.refreshed) < 24*time.Hour {
				s.mu.Unlock()
				return key, nil
			}
			done := entry.refreshing
			s.mu.Unlock()
			select {
			case <-ctx.Done():
				return nil, errKeyService
			case <-done:
				continue
			}
		}
		if !entry.attempted.IsZero() && now.Sub(entry.attempted) < 5*time.Minute {
			refreshed := entry.refreshed
			s.mu.Unlock()
			if key != nil && now.Sub(refreshed) < 24*time.Hour {
				return key, nil
			}
			return nil, errors.New("unknown or expired signing key")
		}
		entry.attempted = now
		entry.refreshing = make(chan struct{})
		s.mu.Unlock()

		keys, err := s.loadKeys(ctx, tenant, version)
		s.mu.Lock()
		defer s.mu.Unlock()
		close(entry.refreshing)
		entry.refreshing = nil
		if s.keys[cacheID] != entry {
			return nil, errKeyService
		}
		if err != nil {
			// Cancellation of one login is not a provider outage. Let a live
			// caller retry instead of denying every cold-cache login for 5 minutes.
			if ctx.Err() != nil {
				entry.attempted = time.Time{}
			}
			if key != nil && now.Sub(entry.refreshed) < 24*time.Hour {
				return key, nil
			}
			return nil, errKeyService
		}
		entry.keys, entry.refreshed = keys, now
		if key = keys[kid]; key != nil {
			return key, nil
		}
		return nil, errors.New("unknown signing key")
	}
}
