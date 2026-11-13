package profile

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDecodeRejectsOversizedProfileAfterValidJSON(t *testing.T) {
	var profile struct {
		ID string `json:"id"`
	}
	require.NoError(t, Decode(strings.NewReader(`{"id":"user"}`), &profile))
	require.Equal(t, "user", profile.ID)

	// A streaming decoder limited to 1 MiB would accept the first object and
	// mistake the artificial EOF after the whitespace for the real response end.
	oversized := `{"id":"user"}` + strings.Repeat(" ", maxProfileBytes)
	require.ErrorContains(t, Decode(strings.NewReader(oversized), &profile), "size limit")
	require.Error(t, Decode(strings.NewReader(`{"id":"user"} {"id":"other"}`), &profile))
}
