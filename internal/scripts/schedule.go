package scripts

import (
	"errors"
	"fmt"
	"slices"
	"time"
)

// Schedule kinds.
const (
	KindEvery  = "every"  // every Every (a duration), counted from the last run
	KindDaily  = "daily"  // every day at At
	KindWeekly = "weekly" // on Days at At
)

const (
	minEvery = time.Minute
	maxEvery = 31 * 24 * time.Hour
)

// Schedule runs a saved script by itself while Nexora is running. Times are
// in the computer's local time zone.
type Schedule struct {
	Enabled bool   `json:"enabled"`
	Kind    string `json:"kind"`
	// Every is a duration such as "30m" or "6h" (KindEvery).
	Every string `json:"every,omitempty"`
	// At is the time of day, "HH:MM" (KindDaily, KindWeekly).
	At string `json:"at,omitempty"`
	// Days are the weekdays to run on, 0 = Sunday (KindWeekly).
	Days []int `json:"days,omitempty"`
	// CatchUp runs the script once at launch if a run was due while Nexora
	// was closed. Several missed runs still give one run.
	CatchUp bool `json:"catchUp,omitempty"`
}

func (s *Schedule) validate() error {
	switch s.Kind {
	case KindEvery:
		d, err := time.ParseDuration(s.Every)
		if err != nil {
			return fmt.Errorf("schedule: %q is not a duration like 30m or 6h", s.Every)
		}
		if d < minEvery || d > maxEvery {
			return fmt.Errorf("schedule: the interval must be between 1 minute and 31 days")
		}
		s.At, s.Days = "", nil
	case KindDaily, KindWeekly:
		if _, _, err := parseClock(s.At); err != nil {
			return err
		}
		s.Every = ""
		if s.Kind == KindDaily {
			s.Days = nil
			break
		}
		if len(s.Days) == 0 {
			return errors.New("schedule: pick at least one day")
		}
		for _, d := range s.Days {
			if d < 0 || d > 6 {
				return fmt.Errorf("schedule: day %d is not 0 (Sunday) to 6 (Saturday)", d)
			}
		}
		slices.Sort(s.Days)
		s.Days = slices.Compact(s.Days)
	default:
		return fmt.Errorf("schedule: unknown kind %q (every, daily or weekly)", s.Kind)
	}
	return nil
}

// Next returns the first run time strictly after t. The schedule must be
// valid.
func (s Schedule) Next(t time.Time) time.Time {
	if s.Kind == KindEvery {
		d, _ := time.ParseDuration(s.Every)
		return t.Add(d).Truncate(time.Second)
	}
	h, m, _ := parseClock(s.At)
	t = t.Local()
	// Eight days covers a weekly schedule with a single day; time.Date
	// normalises day overflow and DST gaps.
	for i := range 8 {
		c := time.Date(t.Year(), t.Month(), t.Day()+i, h, m, 0, 0, time.Local)
		if !c.After(t) {
			continue
		}
		if s.Kind == KindWeekly && !slices.Contains(s.Days, int(c.Weekday())) {
			continue
		}
		return c
	}
	return t.Add(24 * time.Hour) // unreachable for a valid schedule
}

func parseClock(at string) (h, m int, err error) {
	if _, err := fmt.Sscanf(at, "%d:%d", &h, &m); err != nil || len(at) != 5 || h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, 0, fmt.Errorf("schedule: %q is not a time like 09:30", at)
	}
	return h, m, nil
}
