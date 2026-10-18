package healthchecks

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

func TestCronSimOracleVectors(t *testing.T) {
	data, err := os.ReadFile("../../tests/compatibility/cron_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var vectors struct {
		Timezone string `json:"timezone"`
		Cases    []struct {
			Name       string   `json:"name"`
			Expression string   `json:"expression"`
			Now        string   `json:"now"`
			Timezone   string   `json:"timezone"`
			Next       []string `json:"next"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(data, &vectors); err != nil {
		t.Fatal(err)
	}
	for _, v := range vectors.Cases {
		t.Run(v.Name, func(t *testing.T) {
			zone := v.Timezone
			if zone == "" {
				zone = vectors.Timezone
			}
			c := parseTestConfig(t, strings.Replace(simpleConfig, "every: 5m", "cron: '"+v.Expression+"'\n    timezone: "+zone, 1))
			now, err := time.Parse(time.RFC3339, v.Now)
			if err != nil {
				t.Fatal(err)
			}
			for _, expected := range v.Next {
				want, err := time.Parse(time.RFC3339, expected)
				if err != nil {
					t.Fatal(err)
				}
				got := c.Checks[0].next(now)
				if !got.After(now) {
					t.Fatalf("next occurrence %s must be later than %s", got, now)
				}
				if !got.Equal(want) {
					t.Fatalf("after %s: got %s, want %s", now, got, want)
				}
				now = got
			}
		})
	}
}

func TestCronWhitelist(t *testing.T) {
	for _, expression := range []string{
		"* * * * *", "*/15 * * * *", "0 */6 * * *", "30 2 * * *",
		"0 3 * * 0", "0 3 * * 7", "0 0 31 * *", "0 0 1 1 *", "0 0 28 2 *", "0 0 * 12 1",
	} {
		t.Run("accept/"+expression, func(t *testing.T) {
			input := strings.Replace(simpleConfig, "every: 5m", "cron: '"+expression+"'", 1)
			parseTestConfig(t, input)
		})
	}
	for _, expression := range []string{
		"0,30 * * * *", "0-0/5 * * * *", "0 0 * * 1-1/2", "0 0 * * 1-5",
		"0 0 * * MON", "0 0 L * *", "0 0 ? * *", "0 0 */2 * *", "0 0 * */2 *", "0 0 * * */2",
		"0 0 1 * 1", "*/0 * * * *", "*/61 * * * *", "0 */25 * * *", "0 0 29 2 *", "0 0 31 4 *",
	} {
		t.Run("reject/"+expression, func(t *testing.T) {
			input := strings.Replace(simpleConfig, "every: 5m", "cron: '"+expression+"'", 1)
			if _, err := ParseConfig([]byte(input), os.LookupEnv); err == nil {
				t.Fatal("accepted cron outside the supported subset")
			}
		})
	}
}

// These expectations deliberately follow robfig/cron rather than CronSim.
func TestCronLibraryDSTBehavior(t *testing.T) {
	for _, tc := range []struct {
		name, expression, zone, now string
		next                        []string
	}{
		{"spring-skips-missing-time", "30 1 * * *", "Europe/London", "2026-03-29T00:59:00Z", []string{
			"2026-03-30T01:30:00+01:00",
		}},
		{"half-hour-transition-may-skip-valid-times", "*/15 2 * * *", "Australia/Lord_Howe", "2026-04-05T00:50:00+11:00", []string{
			"2026-04-05T02:30:00+10:30", "2026-04-05T02:45:00+10:30",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := parseTestConfig(t, strings.Replace(simpleConfig, "every: 5m", "cron: '"+tc.expression+"'\n    timezone: "+tc.zone, 1))
			now, err := time.Parse(time.RFC3339, tc.now)
			if err != nil {
				t.Fatal(err)
			}
			for _, value := range tc.next {
				want, err := time.Parse(time.RFC3339, value)
				if err != nil {
					t.Fatal(err)
				}
				got := c.Checks[0].next(now)
				if !got.Equal(want) || !got.After(now) {
					t.Fatalf("after %s: got %s, want %s", now, got, want)
				}
				now = got
			}
		})
	}
}

func TestCronFixedTimeSkipsRepeatedOccurrence(t *testing.T) {
	for _, tc := range []struct{ name, expression, zone, after, want string }{
		{"before-first-occurrence", "30 2 * * *", "Europe/Warsaw", "2026-10-25T02:29:59+02:00", "2026-10-25T02:30:00+02:00"},
		{"exact-first-occurrence", "30 2 * * *", "Europe/Warsaw", "2026-10-25T02:30:00+02:00", "2026-10-26T02:30:00+01:00"},
		{"after-first-ping", "30 2 * * *", "Europe/Warsaw", "2026-10-25T02:30:05+02:00", "2026-10-26T02:30:00+01:00"},
		{"at-clock-change", "30 2 * * *", "Europe/Warsaw", "2026-10-25T02:00:00+01:00", "2026-10-26T02:30:00+01:00"},
		{"during-repeated-hour", "30 2 * * *", "Europe/Warsaw", "2026-10-25T02:20:00+01:00", "2026-10-26T02:30:00+01:00"},
		{"after-second-ping", "30 2 * * *", "Europe/Warsaw", "2026-10-25T02:30:05+01:00", "2026-10-26T02:30:00+01:00"},
		{"end-of-repeated-hour", "0 3 * * *", "Europe/Warsaw", "2026-10-25T02:30:05+02:00", "2026-10-25T03:00:00+01:00"},
		{"weekly", "30 2 * * 0", "Europe/Warsaw", "2026-10-25T02:30:05+02:00", "2026-11-01T02:30:00+01:00"},
		{"half-hour", "45 1 * * *", "Australia/Lord_Howe", "2026-04-05T01:45:05+11:00", "2026-04-06T01:45:00+10:30"},
		{"utc", "30 2 * * *", "UTC", "2026-10-25T02:30:05Z", "2026-10-26T02:30:00Z"},
		{"wildcard-hour-keeps-repeated-time", "30 * * * *", "Europe/Warsaw", "2026-10-25T02:30:05+02:00", "2026-10-25T02:30:00+01:00"},
		{"wildcard-minute-keeps-repeated-time", "*/15 2 * * *", "Europe/Warsaw", "2026-10-25T02:45:05+02:00", "2026-10-25T02:00:00+01:00"},
		{"wildcard-keeps-repeated-hour", "*/15 * * * *", "Europe/Warsaw", "2026-10-25T02:45:05+02:00", "2026-10-25T02:00:00+01:00"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			schedule, err := parseSchedule(tc.expression, tc.zone)
			if err != nil {
				t.Fatal(err)
			}
			after, err := time.Parse(time.RFC3339, tc.after)
			if err != nil {
				t.Fatal(err)
			}
			want, err := time.Parse(time.RFC3339, tc.want)
			if err != nil {
				t.Fatal(err)
			}
			if got := schedule.Next(after); !got.Equal(want) || !got.After(after) {
				t.Fatalf("after %s: got %s, want %s", after, got, want)
			}
		})
	}
}

func TestCronRequiresAnOccurrenceEveryCalendarYear(t *testing.T) {
	for _, tc := range []struct{ expression, zone string }{
		{"0 0 29 2 *", "UTC"},
		// This wildcard schedule skips the entire annual window in DST-gap years.
		{"*/30 1 29 3 *", "Europe/London"},
	} {
		t.Run(tc.expression+"/"+tc.zone, func(t *testing.T) {
			input := strings.Replace(simpleConfig, "every: 5m", "cron: '"+tc.expression+"'\n    timezone: "+tc.zone, 1)
			_, err := ParseConfig([]byte(input), os.LookupEnv)
			if err == nil || !strings.Contains(err.Error(), "every calendar year") {
				t.Fatalf("annual cadence validation: %v", err)
			}
		})
	}
}
