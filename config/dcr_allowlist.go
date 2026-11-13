// Copyright (c) 2015-present Mattermost, Inc. All Rights Reserved.
// See LICENSE.txt for license information.

package config

import (
	"encoding/json"
	"errors"

	"github.com/mattermost/mattermost/server/public/model"
)

// loadLegacyDCRAllowlist accepts the old scalar representation only when it is
// the reason configuration decoding failed. Normal configs keep the standard
// decoding path, and unrelated type errors must still prevent startup.
func loadLegacyDCRAllowlist(data []byte, cfg *model.Config, decodeErr error) bool {
	var typeErr *json.UnmarshalTypeError
	if !errors.As(decodeErr, &typeErr) || typeErr.Field != "ServiceSettings.DCRRedirectURIAllowlist" || typeErr.Value != "string" {
		return false
	}

	type serviceSettings model.ServiceSettings
	var legacy string
	overlay := struct {
		*model.Config
		ServiceSettings struct {
			*serviceSettings
			DCRRedirectURIAllowlist *string
		}
	}{Config: cfg}
	overlay.ServiceSettings.serviceSettings = (*serviceSettings)(&cfg.ServiceSettings)
	overlay.ServiceSettings.DCRRedirectURIAllowlist = &legacy
	if err := json.Unmarshal(data, &overlay); err != nil {
		return false
	}

	cfg.ServiceSettings.DCRRedirectURIAllowlist = []string{}
	if legacy != "" {
		// Keep URI contents intact: commas and spaces are not list separators.
		cfg.ServiceSettings.DCRRedirectURIAllowlist = []string{legacy}
	}
	return true
}
