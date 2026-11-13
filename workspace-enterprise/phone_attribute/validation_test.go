package phone_attribute

import (
	"encoding/json"
	"testing"
)

func TestValidPhoneValue(t *testing.T) {
	for _, tc := range []struct {
		value string
		valid bool
	}{
		{`"737328812"`, true},
		{`"711234567"`, true},
		{`"771234567"`, true},
		{`"781234567"`, true},
		{`""`, true},
		{`"hello"`, false},
		{`"0771234567"`, false},
		{`"+967712345678"`, false},
		{`"721234567"`, false},
		{`"73732881"`, false},
		{`null`, false},
		{`12345`, false},
	} {
		if got := validPhoneValue(json.RawMessage(tc.value)); got != tc.valid {
			t.Errorf("validPhoneValue(%s) = %v, want %v", tc.value, got, tc.valid)
		}
	}
}

func TestReserveBatchPhone(t *testing.T) {
	seen := map[string]string{}
	if err := reserveBatchPhone(seen, "737328812", "user-one"); err != nil {
		t.Fatal(err)
	}
	if err := reserveBatchPhone(seen, "737328812", "user-one"); err != nil {
		t.Fatal("same user should be allowed to retain the number:", err)
	}
	if err := reserveBatchPhone(seen, "737328812", "user-two"); err == nil {
		t.Fatal("same number should be rejected for another user")
	}
	if err := reserveBatchPhone(seen, "", "user-two"); err != nil {
		t.Fatal("empty optional number should be allowed:", err)
	}
}
