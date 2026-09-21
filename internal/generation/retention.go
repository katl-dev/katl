package generation

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Retention keeps the newest generations and all generations within MaxAge.
// Boot selections and shared artifact references take precedence over this policy.
type Retention struct {
	KeepLast *int   `json:"keepLast,omitempty" yaml:"keepLast,omitempty"`
	MaxAge   string `json:"maxAge,omitempty" yaml:"maxAge,omitempty"`
}

func (r Retention) Limits() (int, time.Duration, error) {
	count, age := 5, 30*24*time.Hour
	if r.KeepLast != nil {
		count = *r.KeepLast
	}
	if count < 1 {
		return 0, 0, fmt.Errorf("keepLast must be at least 1")
	}
	if r.MaxAge != "" {
		value := r.MaxAge
		var err error
		if strings.HasSuffix(value, "d") {
			var days int64
			days, err = strconv.ParseInt(strings.TrimSuffix(value, "d"), 10, 64)
			if err == nil && (days < 0 || days > int64((1<<63-1)/(24*time.Hour))) {
				err = fmt.Errorf("days out of range")
			}
			if err == nil {
				age = time.Duration(days) * 24 * time.Hour
			}
		} else {
			age, err = time.ParseDuration(value)
		}
		if err != nil || age < 0 {
			return 0, 0, fmt.Errorf("maxAge must be a non-negative duration, such as 30d or 720h")
		}
	}
	return count, age, nil
}
