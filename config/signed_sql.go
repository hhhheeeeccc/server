// Copyright (c) 2015-present Mattermost, Inc. All Rights Reserved.
// See LICENSE.txt for license information.

package config

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"strings"

	"github.com/mattermost/mattermost/server/public/model"
)

const signedSQLPrefix = "signed:rsa:v1:"

// Only the public verification key ships with the server. The signing key lives
// in the separate desktop generator. Signed DSNs are readable, not confidential.
//
//go:embed database-public.pem
var databasePublicKeyPEM []byte

type signedSQLDocument struct {
	Data      []byte `json:"data"`
	Signature []byte `json:"signature"`
}

func sqlVerificationKey() (*rsa.PublicKey, error) {
	block, _ := pem.Decode(databasePublicKeyPEM)
	if block == nil || block.Type != "PUBLIC KEY" {
		return nil, fmt.Errorf("invalid embedded SQL public key")
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("invalid embedded SQL public key")
	}
	key, ok := parsed.(*rsa.PublicKey)
	if !ok || key.N.BitLen() < 2048 {
		return nil, fmt.Errorf("embedded SQL verification key must be RSA with at least 2048 bits")
	}
	return key, nil
}

func verifySQLDataSource(value string) (string, error) {
	if !strings.HasPrefix(value, "signed:") {
		if strings.HasPrefix(value, "encrypted:") {
			return "", fmt.Errorf("encrypted SQL configuration must be converted by the external database generator")
		}
		return value, nil
	}
	failure := fmt.Errorf("invalid signed SQL connection; use the external generator with the matching private key")
	if !strings.HasPrefix(value, signedSQLPrefix) || len(value) > 262144 {
		return "", failure
	}
	data, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(value, signedSQLPrefix))
	if err != nil {
		return "", failure
	}
	var document signedSQLDocument
	if json.Unmarshal(data, &document) != nil || len(document.Data) == 0 || len(document.Data) > 65536 {
		return "", failure
	}
	public, err := sqlVerificationKey()
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(append([]byte(signedSQLPrefix), document.Data...))
	if rsa.VerifyPSS(public, crypto.SHA256, digest[:], document.Signature, &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash}) != nil {
		return "", failure
	}
	return string(document.Data), nil
}

func sqlConfigHasSignedValues(cfg *model.Config) bool {
	if cfg == nil {
		return false
	}
	if cfg.SqlSettings.DataSource != nil && strings.HasPrefix(*cfg.SqlSettings.DataSource, "signed:") {
		return true
	}
	for _, values := range [][]string{cfg.SqlSettings.DataSourceReplicas, cfg.SqlSettings.DataSourceSearchReplicas} {
		for _, value := range values {
			if strings.HasPrefix(value, "signed:") {
				return true
			}
		}
	}
	return false
}

func mapSQLDataSources(sql, previous *model.SqlSettings, convert func(string, string) (string, error)) error {
	if previous == nil {
		previous = &model.SqlSettings{}
	}
	if sql.DataSource != nil {
		old := ""
		if previous.DataSource != nil {
			old = *previous.DataSource
		}
		value, err := convert(*sql.DataSource, old)
		if err != nil {
			return err
		}
		sql.DataSource = new(value)
	}
	for _, field := range []struct{ values, previous []string }{
		{sql.DataSourceReplicas, previous.DataSourceReplicas},
		{sql.DataSourceSearchReplicas, previous.DataSourceSearchReplicas},
	} {
		for i, value := range field.values {
			old := ""
			if i < len(field.previous) {
				old = field.previous[i]
			}
			converted, err := convert(value, old)
			if err != nil {
				return err
			}
			field.values[i] = converted
		}
	}
	return nil
}

func resolveSignedSQL(cfg *model.Config) error {
	return mapSQLDataSources(&cfg.SqlSettings, nil, func(value, _ string) (string, error) {
		return verifySQLDataSource(value)
	})
}

// Preserve the original signed envelope on save. The server cannot sign changed
// credentials: administrators must generate a new token outside the server.
func preserveSignedSQL(cfg, previous, submitted *model.Config) error {
	if !sqlConfigHasSignedValues(cfg) && !sqlConfigHasSignedValues(previous) && !sqlConfigHasSignedValues(submitted) {
		return nil
	}
	if submitted != nil {
		if err := mapSQLDataSources(&cfg.SqlSettings, &submitted.SqlSettings, func(value, token string) (string, error) {
			if strings.HasPrefix(token, "signed:") {
				plain, err := verifySQLDataSource(token)
				if err != nil {
					return "", err
				}
				if value == plain {
					return token, nil
				}
			}
			return value, nil
		}); err != nil {
			return err
		}
	}
	var old *model.SqlSettings
	if previous != nil {
		old = &previous.SqlSettings
	}
	return mapSQLDataSources(&cfg.SqlSettings, old, func(value, previous string) (string, error) {
		if value == "" {
			return value, nil
		}
		if strings.HasPrefix(value, signedSQLPrefix) {
			if _, err := verifySQLDataSource(value); err != nil {
				return "", err
			}
			return value, nil
		}
		if strings.HasPrefix(previous, signedSQLPrefix) {
			plain, err := verifySQLDataSource(previous)
			if err != nil {
				return "", err
			}
			if plain == value {
				return previous, nil
			}
		}
		return "", fmt.Errorf("changed SQL connections require a new signed value from the external database generator")
	})
}
