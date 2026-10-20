package healthchecks

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
)

var cronFieldPattern = regexp.MustCompile(`^(\*|[0-9]{1,2}|\*/[0-9]{1,2})$`)

// Fixed minute/hour schedules expect only the first occurrence of a repeated
// local time. All other calendar calculations remain with the cron library.
type fixedTimeSchedule struct {
	*cron.SpecSchedule
}

func (s fixedTimeSchedule) Next(after time.Time) time.Time {
	next := s.SpecSchedule.Next(after)
	for !next.IsZero() {
		local := next.In(s.Location)
		start, _ := local.ZoneBounds()
		if start.IsZero() {
			return next
		}
		_, before := start.Add(-time.Nanosecond).Zone()
		_, current := local.Zone()
		repeatedUntil := start.Add(time.Duration(before-current) * time.Second)
		if !next.Before(repeatedUntil) {
			return next
		}
		next = s.SpecSchedule.Next(next)
	}
	return next
}

func parseSchedule(expression, zone string) (cron.Schedule, error) {
	fields := strings.Fields(expression)
	if len(fields) != 5 {
		return nil, errors.New("cron requires five fields")
	}
	for i, field := range fields {
		if !cronFieldPattern.MatchString(field) {
			return nil, fmt.Errorf("unsupported cron field %d: use * or a number; */N is allowed only for minutes and hours", i+1)
		}
		if strings.HasPrefix(field, "*/") {
			if i > 1 {
				return nil, errors.New("cron steps are allowed only for minutes and hours")
			}
			step, _ := strconv.Atoi(field[2:])
			maximum := 60
			if i == 1 {
				maximum = 24
			}
			if step < 1 || step > maximum {
				return nil, fmt.Errorf("cron field %d step must be between 1 and %d", i+1, maximum)
			}
		}
	}
	if fields[2] != "*" && fields[4] != "*" {
		return nil, errors.New("cron cannot restrict both day-of-month and day-of-week")
	}
	// Keep the two conventional Sunday spellings without a second range parser.
	if weekday, err := strconv.Atoi(fields[4]); err == nil && weekday == 7 {
		fields[4] = "0"
	}
	s, err := cronParser.Parse("CRON_TZ=" + zone + " " + strings.Join(fields, " "))
	if err != nil {
		return nil, errors.New("cron field value is outside its allowed range")
	}
	spec := s.(*cron.SpecSchedule)
	if zone != "UTC" && !strings.HasPrefix(fields[0], "*") && !strings.HasPrefix(fields[1], "*") {
		s = fixedTimeSchedule{spec}
	}
	// Check one occurrence per calendar year across a Gregorian cycle, without
	// enumerating every run. This also catches annual windows skipped by DST.
	for year := 2000; year < 2400; year++ {
		start := time.Date(year, 1, 1, 0, 0, 0, 0, spec.Location)
		next := s.Next(start.Add(-time.Nanosecond))
		if next.IsZero() || next.In(spec.Location).Year() != year {
			return nil, errors.New("cron must have an occurrence in every calendar year")
		}
	}
	return s, nil
}

func formatScheduleInterval(value time.Duration) string {
	var parts []string
	for _, unit := range []struct {
		size time.Duration
		name string
	}{
		{24 * time.Hour, "day"}, {time.Hour, "hour"}, {time.Minute, "minute"},
	} {
		amount := value / unit.size
		if amount == 0 {
			continue
		}
		name := unit.name
		if amount != 1 {
			name += "s"
		}
		parts = append(parts, fmt.Sprintf("%d %s", amount, name))
		value %= unit.size
	}
	if value > 0 {
		name := "seconds"
		if value == time.Second {
			name = "second"
		}
		parts = append(parts, strconv.FormatFloat(value.Seconds(), 'f', -1, 64)+" "+name)
	}
	return strings.Join(parts, " ")
}
