package account_linking

import (
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

type loginBody struct {
	io.Reader
	closed   int
	closeErr error
}

func (b *loginBody) Close() error {
	b.closed++
	return b.closeErr
}

func TestReadLoginRequestPreservesBodyAndClose(t *testing.T) {
	for _, tc := range []struct {
		name, payload string
		valid         bool
	}{
		{"valid", `{"login_id":"demo","password":"example"}`, true},
		{"malformed", `{"login_id":`, false},
		{"oversized", strings.Repeat("x", loginMatchBodyLimit+15), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			closeErr := errors.New("original close error")
			original := &loginBody{Reader: strings.NewReader(tc.payload), closeErr: closeErr}
			r := httptest.NewRequest("POST", "/api/v4/users/login", nil)
			r.Body = original
			_, valid := readLoginRequestForMatch(r)
			require.Equal(t, tc.valid, valid)
			data, err := io.ReadAll(r.Body)
			require.NoError(t, err)
			require.Equal(t, tc.payload, string(data), "the native fallback must receive the original bytes")
			require.Zero(t, original.closed, "matching must leave the request body open")
			require.ErrorIs(t, r.Body.Close(), closeErr)
			require.Equal(t, 1, original.closed)
		})
	}
}
