// Package disappearing_messages implements conversation expiry from send time.
// It deliberately does not depend on Burn-on-Read or read receipts.
package disappearing_messages

import (
	"errors"
	"strings"
)

type Policy struct {
	DurationSeconds int64 `json:"duration_seconds"`
}

func validateDuration(seconds int64) error {
	switch seconds {
	case 0, 86400, 604800, 7776000:
		return nil
	default:
		return errors.New("duration must be off, 24 hours, 7 days, or 90 days")
	}
}

func parseDuration(text string) (int64, error) {
	text = strings.NewReplacer("٠", "0", "١", "1", "٢", "2", "٣", "3", "٤", "4", "٥", "5", "٦", "6", "٧", "7", "٨", "8", "٩", "9").Replace(strings.ToLower(strings.TrimSpace(text)))
	text = strings.Join(strings.Fields(text), " ")
	switch text {
	case "24h", "24 ساعة":
		return 86400, nil
	case "7d", "7 أيام", "7 ايام":
		return 604800, nil
	case "90d", "90 يوم", "90 يوما":
		return 7776000, nil
	case "off", "إيقاف", "ايقاف":
		return 0, nil
	default:
		return 0, errors.New("unknown disappearing-message duration")
	}
}

func durationLabel(seconds int64) string {
	switch seconds {
	case 86400:
		return "24 ساعة"
	case 604800:
		return "7 أيام"
	case 7776000:
		return "90 يومًا"
	default:
		return "إيقاف"
	}
}
