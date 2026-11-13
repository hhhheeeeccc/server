package disappearing_messages

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestConversationDurations(t *testing.T) {
	for _, seconds := range []int64{0, 86400, 604800, 7776000} {
		require.NoError(t, validateDuration(seconds))
	}
	for _, seconds := range []int64{-1, 5, 60, 86399, 86401, 9223372036854775807} {
		require.Error(t, validateDuration(seconds))
	}
}

func TestArabicConversationCommand(t *testing.T) {
	for text, seconds := range map[string]int64{
		"24h": 86400, "24 ساعة": 86400, "٢٤ ساعة": 86400,
		"7d": 604800, "7 أيام": 604800, "٧ أيام": 604800,
		"90d": 7776000, "90 يوم": 7776000, "٩٠ يوم": 7776000,
		"إيقاف": 0, "off": 0,
	} {
		actual, err := parseDuration(text)
		require.NoError(t, err, text)
		require.Equal(t, seconds, actual, text)
	}
	_, err := parseDuration("60")
	require.Error(t, err, "never silently interpret the old read-timer syntax")
}
