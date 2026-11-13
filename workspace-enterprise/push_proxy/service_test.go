package push_proxy

import (
	"encoding/base64"
	"testing"

	"github.com/mattermost/mattermost/server/public/model"
	"github.com/stretchr/testify/require"
)

func TestServiceOnlyUsesConfiguredGatewayURL(t *testing.T) {
	cfg := &model.Config{}
	pushURL := "https://push.example.test"
	cfg.EmailSettings.PushNotificationServer = &pushURL
	s := &Service{publicURL: pushURL, key: make([]byte, 32)}
	require.True(t, s.configured(cfg))

	otherURL := "https://other.example.test"
	cfg.EmailSettings.PushNotificationServer = &otherURL
	require.False(t, s.configured(cfg))

	cfg.EmailSettings.PushNotificationServer = &pushURL
	s.key = nil
	require.False(t, s.configured(cfg))
	require.False(t, s.configured(nil))
}

func TestNewRejectsUnsafeGatewayURL(t *testing.T) {
	t.Setenv("WORKSPACE_PUSH_GATEWAY_KEY", base64.RawURLEncoding.EncodeToString(make([]byte, 32)))
	for _, gatewayURL := range []string{
		"https://user:pass@push.example.test",
		"https://push.example.test/api/v1",
		"https://push.example.test?redirect=other",
		"https://push.example.test#fragment",
		"ftp://push.example.test",
	} {
		t.Setenv("WORKSPACE_PUSH_GATEWAY_PUBLIC_URL", gatewayURL)
		service := New(nil).(*Service)
		require.Error(t, service.configErr, gatewayURL)
	}
	t.Setenv("WORKSPACE_PUSH_GATEWAY_PUBLIC_URL", "https://push.example.test/")
	service := New(nil).(*Service)
	require.NoError(t, service.configErr)
	require.Equal(t, "https://push.example.test", service.publicURL)
}
