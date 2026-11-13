// Copyright 2016 Russell Haering et al.
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at http://www.apache.org/licenses/LICENSE-2.0
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//
// Modified for Workspace: standard XML Encryption digest/MGF identifiers,
// independent OAEP/MGF hashes, and bounds checks before symmetric decryption.

package encryptedresponse

import (
	"bytes"
	"crypto"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	_ "crypto/sha1" // RSA-OAEP's XML Encryption default digest and MGF.
	_ "crypto/sha256"
	_ "crypto/sha512"
	"crypto/tls"
	"encoding/base64"
	"encoding/xml"
	"fmt"

	"github.com/mattermost/gosaml2/types"
)

type encryptionMethod struct {
	Algorithm    string              `xml:",attr"`
	DigestMethod *types.DigestMethod `xml:"http://www.w3.org/2000/09/xmldsig# DigestMethod"`
	MGF          *struct {
		Algorithm string `xml:",attr"`
	} `xml:"http://www.w3.org/2009/xmlenc11# MGF"`
	OAEPparams string `xml:"http://www.w3.org/2001/04/xmlenc# OAEPparams"`
}

type encryptedKey struct {
	X509Data         string `xml:"KeyInfo>X509Data>X509Certificate"`
	CipherValue      string `xml:"CipherData>CipherValue"`
	EncryptionMethod encryptionMethod
}

type encryptedAssertion struct {
	XMLName          xml.Name         `xml:"urn:oasis:names:tc:SAML:2.0:assertion EncryptedAssertion"`
	EncryptionMethod encryptionMethod `xml:"EncryptedData>EncryptionMethod"`
	EncryptedKey     encryptedKey     `xml:"EncryptedData>KeyInfo>EncryptedKey"`
	DetEncryptedKey  encryptedKey     `xml:"EncryptedKey"`
	CipherValue      string           `xml:"EncryptedData>CipherData>CipherValue"`
}

func digestHash(method *types.DigestMethod) (crypto.Hash, error) {
	if method == nil {
		return crypto.SHA1, nil
	}
	switch method.Algorithm {
	case "", types.MethodSHA1:
		return crypto.SHA1, nil
	case "http://www.w3.org/2001/04/xmlenc#sha256", types.MethodSHA256:
		return crypto.SHA256, nil
	case "http://www.w3.org/2001/04/xmldsig-more#sha384":
		return crypto.SHA384, nil
	case "http://www.w3.org/2001/04/xmlenc#sha512", types.MethodSHA512:
		return crypto.SHA512, nil
	default:
		return 0, fmt.Errorf("unsupported OAEP digest")
	}
}

func (method encryptionMethod) mgfHash() (crypto.Hash, error) {
	// XML Encryption 1.0 fixes MGF1 to SHA-1. XML Encryption 1.1 also
	// defaults to SHA-1 when MGF is absent, independently of DigestMethod.
	if method.MGF == nil {
		return crypto.SHA1, nil
	}
	if method.Algorithm == types.MethodRSAOAEP && method.MGF.Algorithm != "http://www.w3.org/2009/xmlenc11#mgf1sha1" {
		return 0, fmt.Errorf("XML Encryption 1.0 requires MGF1-SHA1")
	}
	switch method.MGF.Algorithm {
	case "http://www.w3.org/2009/xmlenc11#mgf1sha1":
		return crypto.SHA1, nil
	case "http://www.w3.org/2009/xmlenc11#mgf1sha256":
		return crypto.SHA256, nil
	case "http://www.w3.org/2009/xmlenc11#mgf1sha384":
		return crypto.SHA384, nil
	case "http://www.w3.org/2009/xmlenc11#mgf1sha512":
		return crypto.SHA512, nil
	default:
		return 0, fmt.Errorf("unsupported OAEP MGF")
	}
}

func (ea *encryptedAssertion) DecryptBytes(cert *tls.Certificate) ([]byte, error) {
	key := ea.EncryptedKey
	if key.CipherValue == "" {
		key = ea.DetEncryptedKey
	}
	if key.EncryptionMethod.Algorithm == types.MethodRSAv1_5 {
		// Preserve the upstream legacy decryption path. The new adapter is
		// required for RSA-OAEP, not a change to existing RSA-v1.5 behavior.
		toLegacyKey := func(k encryptedKey) types.EncryptedKey {
			return types.EncryptedKey{X509Data: k.X509Data, CipherValue: k.CipherValue,
				EncryptionMethod: types.EncryptionMethod{Algorithm: k.EncryptionMethod.Algorithm, DigestMethod: k.EncryptionMethod.DigestMethod}}
		}
		legacy := types.EncryptedAssertion{EncryptionMethod: types.EncryptionMethod{Algorithm: ea.EncryptionMethod.Algorithm},
			EncryptedKey: toLegacyKey(ea.EncryptedKey), DetEncryptedKey: toLegacyKey(ea.DetEncryptedKey), CipherValue: ea.CipherValue}
		return legacy.DecryptBytes(cert)
	}
	if key.EncryptionMethod.Algorithm != types.MethodRSAOAEP && key.EncryptionMethod.Algorithm != types.MethodRSAOAEP2 {
		return nil, fmt.Errorf("unsupported encrypted key algorithm")
	}
	if cert == nil || len(cert.Certificate) == 0 {
		return nil, fmt.Errorf("missing decryption certificate")
	}
	private, ok := cert.PrivateKey.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("decryption requires an RSA key")
	}
	if key.X509Data != "" {
		data, err := base64.StdEncoding.DecodeString(key.X509Data)
		if err != nil || !bytes.Equal(data, cert.Certificate[0]) {
			return nil, fmt.Errorf("encrypted key certificate does not match the SP")
		}
	}
	digest, err := digestHash(key.EncryptionMethod.DigestMethod)
	if err != nil {
		return nil, err
	}
	mgf, err := key.EncryptionMethod.mgfHash()
	if err != nil {
		return nil, err
	}
	label, err := base64.StdEncoding.DecodeString(key.EncryptionMethod.OAEPparams)
	if err != nil {
		return nil, fmt.Errorf("invalid OAEP parameters")
	}
	wrapped, err := base64.StdEncoding.DecodeString(key.CipherValue)
	if err != nil {
		return nil, fmt.Errorf("invalid encrypted key")
	}
	symmetric, err := private.Decrypt(rand.Reader, wrapped, &rsa.OAEPOptions{Hash: digest, MGFHash: mgf, Label: label})
	if err != nil {
		return nil, fmt.Errorf("cannot decrypt symmetric key")
	}
	wantBytes := map[string]int{types.MethodAES128GCM: 16, types.MethodAES192GCM: 24, types.MethodAES256GCM: 32,
		types.MethodAES128CBC: 16, types.MethodAES256CBC: 32}[ea.EncryptionMethod.Algorithm]
	if wantBytes == 0 || len(symmetric) != wantBytes {
		return nil, fmt.Errorf("invalid symmetric algorithm or key size")
	}
	block, err := aes.NewCipher(symmetric)
	if err != nil {
		return nil, err
	}
	data, err := base64.StdEncoding.DecodeString(ea.CipherValue)
	if err != nil {
		return nil, fmt.Errorf("invalid encrypted data")
	}
	switch ea.EncryptionMethod.Algorithm {
	case types.MethodAES128GCM, types.MethodAES192GCM, types.MethodAES256GCM:
		gcm, err := cipher.NewGCM(block)
		if err != nil {
			return nil, err
		}
		if len(data) < gcm.NonceSize()+gcm.Overhead() {
			return nil, fmt.Errorf("truncated AES-GCM data")
		}
		return gcm.Open(nil, data[:gcm.NonceSize()], data[gcm.NonceSize():], nil)
	default:
		if len(data) < 2*block.BlockSize() || len(data)%block.BlockSize() != 0 {
			return nil, fmt.Errorf("truncated AES-CBC data")
		}
		iv, data := data[:block.BlockSize()], data[block.BlockSize():]
		cipher.NewCBCDecrypter(block, iv).CryptBlocks(data, data)
		padding := int(data[len(data)-1])
		if padding < 1 || padding > block.BlockSize() || padding > len(data) {
			return nil, fmt.Errorf("invalid AES-CBC padding")
		}
		return data[:len(data)-padding], nil
	}
}
