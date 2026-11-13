// Copyright (c) 2015-present Mattermost, Inc. All Rights Reserved.
// See LICENSE.txt for license information.

package config

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFileStoreLegacyDCRRedirectURIAllowlist(t *testing.T) {
	for _, tc := range []struct {
		name    string
		value   string
		want    []string
		invalid bool
	}{
		{name: "empty string", value: `""`, want: []string{}},
		{name: "single URI", value: `"https://example.com/callback"`, want: []string{"https://example.com/callback"}},
		{name: "URI with comma", value: `"https://example.com/callback?a=1,2"`, want: []string{"https://example.com/callback?a=1,2"}},
		{name: "array", value: `["https://example.com/**", "cursor://callback/**"]`, want: []string{"https://example.com/**", "cursor://callback/**"}},
		{name: "null", value: `null`, want: []string{}},
		{name: "number", value: `42`, invalid: true},
		{name: "object", value: `{}`, invalid: true},
		{name: "mixed array", value: `["https://example.com/**", 42]`, invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path, cleanup := setupConfigFile(t, minimalConfig)
			defer cleanup()
			data, err := marshalConfig(minimalConfig)
			require.NoError(t, err)
			var raw map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(data, &raw))
			var service map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(raw["ServiceSettings"], &service))
			service["DCRRedirectURIAllowlist"] = json.RawMessage(tc.value)
			raw["ServiceSettings"], err = json.Marshal(service)
			require.NoError(t, err)
			data, err = json.Marshal(raw)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(path, data, 0600))
			backing, err := NewFileStore(path, false)
			require.NoError(t, err)
			store, err := NewStoreFromBacking(backing, nil, false)
			if tc.invalid {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			defer store.Close()
			require.Equal(t, tc.want, store.Get().ServiceSettings.DCRRedirectURIAllowlist)
			require.Equal(t, minimalConfig.ServiceSettings.SiteURL, store.Get().ServiceSettings.SiteURL)
			_, _, err = store.Set(store.Get())
			require.NoError(t, err)
			require.NoError(t, store.Load())
			require.Equal(t, tc.want, getActualFileConfig(t, path).ServiceSettings.DCRRedirectURIAllowlist)
		})
	}
}
