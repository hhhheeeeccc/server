// Copyright 2016 Russell Haering et al.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Modified for Workspace: reuse the original public assertion types with the
// local encrypted-response decoder. See UPSTREAM.json and README_AR.md.

package encryptedresponse

import (
	"fmt"
	saml2 "github.com/mattermost/gosaml2"
)

// RetrieveAssertionInfo takes an encoded response and returns the saml2.AssertionInfo
// contained, or an error message if an error has been encountered.
func (sp *provider) RetrieveAssertionInfo(encodedResponse string) (*saml2.AssertionInfo, error) {
	assertionInfo := &saml2.AssertionInfo{
		Values: make(saml2.Values),
	}

	response, err := sp.ValidateEncodedResponse(encodedResponse)
	if err != nil {
		return nil, saml2.ErrVerification{Cause: err}
	}

	// TODO: Support multiple assertions
	if len(response.Assertions) == 0 {
		return nil, saml2.ErrMissingAssertion
	}

	assertion := response.Assertions[0]
	assertionInfo.Assertions = response.Assertions
	assertionInfo.ResponseSignatureValidated = response.SignatureValidated

	warningInfo, err := sp.VerifyAssertionConditions(&assertion)
	if err != nil {
		return nil, err
	}

	//Get the NameID
	subject := assertion.Subject
	if subject == nil {
		return nil, saml2.ErrMissingElement{Tag: saml2.SubjectTag}
	}

	nameID := subject.NameID
	if nameID == nil {
		return nil, saml2.ErrMissingElement{Tag: saml2.NameIdTag}
	}

	assertionInfo.NameID = nameID.Value

	//Get the actual assertion attributes
	attributeStatement := assertion.AttributeStatement
	if attributeStatement == nil && !sp.AllowMissingAttributes {
		return nil, saml2.ErrMissingElement{Tag: saml2.AttributeStatementTag}
	}

	if attributeStatement != nil {
		for _, attribute := range attributeStatement.Attributes {
			assertionInfo.Values[attribute.Name] = attribute
		}
	}

	if assertion.AuthnStatement != nil {
		if assertion.AuthnStatement.AuthnInstant != nil {
			assertionInfo.AuthnInstant = assertion.AuthnStatement.AuthnInstant
		}
		if assertion.AuthnStatement.SessionNotOnOrAfter != nil {
			assertionInfo.SessionNotOnOrAfter = assertion.AuthnStatement.SessionNotOnOrAfter
		}

		assertionInfo.SessionIndex = assertion.AuthnStatement.SessionIndex
	}

	assertionInfo.WarningInfo = warningInfo
	return assertionInfo, nil
}

// RetrieveAssertionInfo retains gosaml2's signature/namespace checks while using
// XML Encryption's standard OAEP digest and MGF identifiers during decryption.
func RetrieveAssertionInfo(sp *saml2.SAMLServiceProvider, encoded string) (*saml2.AssertionInfo, error) {
	if sp == nil || sp.SPKeyStore == nil || sp.SkipSignatureValidation {
		return nil, fmt.Errorf("encrypted SAML requires a decryption key and signature verification")
	}
	return (&provider{sp}).RetrieveAssertionInfo(encoded)
}
