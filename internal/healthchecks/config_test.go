package healthchecks

import (
	"os"
	"strings"
	"testing"
)

const testUUID = "11111111-1111-4111-8111-111111111111"
const otherUUID = "22222222-2222-4222-8222-222222222222"
const testUUIDSeed = "0123456789abcdef0123456789abcdef"

const simpleConfig = `public_url: http://localhost:8000
checks:
  - uuid: 11111111-1111-4111-8111-111111111111
    name: Heartbeat
    every: 5m
    grace: 1m
`

func parseTestConfig(t *testing.T, text string) *Config {
	t.Helper()
	c, err := ParseConfig([]byte(text), os.LookupEnv)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestConfigExample(t *testing.T) {
	t.Setenv("HC_UUID_SEED", testUUIDSeed)
	b, err := os.ReadFile("../../config.example.yml")
	if err != nil {
		t.Fatal(err)
	}
	c := parseTestConfig(t, string(b))
	if len(c.Checks) != 2 {
		t.Fatalf("checks: %d", len(c.Checks))
	}
}

func TestConfigRejectsInvalidDefinitions(t *testing.T) {
	for name, input := range map[string]string{
		"null boolean":           strings.Replace(simpleConfig, "every: 5m", "every: 5m\n    notify: null", 1),
		"numeric name":           strings.Replace(simpleConfig, "name: Heartbeat", "name: 123", 1),
		"unknown field":          strings.Replace(simpleConfig, "every: 5m", "period: 5m", 1),
		"duplicate field":        strings.Replace(simpleConfig, "every: 5m", "every: 5m\n    every: 6m", 1),
		"both schedules":         strings.Replace(simpleConfig, "every: 5m", "every: 5m\n    cron: '* * * * *'", 1),
		"zero grace":             strings.Replace(simpleConfig, "grace: 1m", "grace: 0s", 1),
		"timezone on interval":   strings.Replace(simpleConfig, "every: 5m", "every: 5m\n    timezone: Europe/London", 1),
		"bad cron":               strings.Replace(simpleConfig, "every: 5m", "cron: '99 * * * *'", 1),
		"impossible cron":        strings.Replace(simpleConfig, "every: 5m", "cron: '0 0 31 2 *'", 1),
		"bad timezone":           strings.Replace(simpleConfig, "every: 5m", "cron: '0 2 * * *'\n    timezone: Invalid/Zone", 1),
		"local timezone":         strings.Replace(simpleConfig, "every: 5m", "cron: '0 2 * * *'\n    timezone: Local", 1),
		"invalid uuid":           strings.Replace(simpleConfig, testUUID, "anything", 1),
		"notify without webhook": strings.Replace(simpleConfig, "every: 5m", "every: 5m\n    notify: true", 1),
		"multiple documents":     simpleConfig + "---\n" + simpleConfig,
		"duplicate uuid":         simpleConfig + strings.Split(simpleConfig, "checks:\n")[1],
		"duplicate dependency":   strings.Replace(simpleConfig, "every: 5m", "every: 5m\n    depends_on_regions: [region-a, region-a]", 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseConfig([]byte(input), os.LookupEnv); err == nil {
				t.Fatal("accepted invalid configuration")
			}
		})
	}
}

func TestHTTPURLPortValidation(t *testing.T) {
	for _, endpoint := range []string{
		"https://example.test", "http://example.test:1", "https://example.test:65535",
		"http://[::1]", "http://[::1]:8000",
	} {
		t.Run("valid/"+endpoint, func(t *testing.T) {
			parseTestConfig(t, strings.Replace(simpleConfig, "http://localhost:8000", endpoint, 1))
			parseTestConfig(t, "webhook:\n  name: fixture\n  url: "+endpoint+"/hook\n"+simpleConfig)
		})
	}
	for _, endpoint := range []string{
		"http://example.test:0", "https://example.test:65536", "https://example.test:99999",
		"http://example.test:", "http://[::1]:", "http://[::1]:99999", "https://example.test:abc",
	} {
		t.Run("invalid/"+endpoint, func(t *testing.T) {
			for _, input := range []string{
				strings.Replace(simpleConfig, "http://localhost:8000", endpoint, 1),
				"webhook:\n  name: fixture\n  url: " + endpoint + "/hook?token=private-test-token\n" + simpleConfig,
			} {
				_, err := ParseConfig([]byte(input), os.LookupEnv)
				if err == nil {
					t.Fatal("accepted invalid destination port")
				}
				if strings.Contains(err.Error(), "private-test-token") {
					t.Fatal("URL credentials leaked in validation error")
				}
			}
		})
	}
}

func TestPublicURLRequiresOrigin(t *testing.T) {
	for _, endpoint := range []string{"https://example.test", "https://example.test/", "http://localhost:8000/"} {
		c := parseTestConfig(t, strings.Replace(simpleConfig, "http://localhost:8000", endpoint, 1))
		if c.PublicURL != strings.TrimSuffix(endpoint, "/") {
			t.Fatalf("unexpected normalized public_url: %s", c.PublicURL)
		}
	}
	for _, endpoint := range []string{
		"https://example.test/base", "https://example.test/base/", "https://example.test//", "https://example.test/%2f",
		"https://example.test?token=fixture", "https://example.test#section", "https://example.test?", "https://example.test#",
	} {
		_, err := ParseConfig([]byte(strings.Replace(simpleConfig, "http://localhost:8000", endpoint, 1)), os.LookupEnv)
		if err == nil || !strings.Contains(err.Error(), "public_url") {
			t.Errorf("accepted public_url outside an origin: %s", endpoint)
		}
	}
	// Only public_url is origin-only; webhook destinations still need paths.
	parseTestConfig(t, "webhook:\n  name: test\n  url: https://example.test/api/hook?token=fixture\n"+simpleConfig)
}

func TestWebhookRenderedBodyLimit(t *testing.T) {
	c := parseTestConfig(t, "webhook:\n  name: test\n  url: https://example.test/hook\n  body: '{\"content\": {{ json .message }}}'\n"+simpleConfig)
	if _, err := c.Webhook.render(WebhookEvent{Message: strings.Repeat("\x00", maxWebhookMessageRunes)}); err != nil {
		t.Fatalf("production-sized message rejected: %v", err)
	}
	if _, err := c.Webhook.render(WebhookEvent{Message: strings.Repeat("\x00", 8192)}); err == nil {
		t.Fatal("accepted JSON body exceeding 32 KiB after escaping")
	}
}

func TestWebhookConfigInterpolationAndJSON(t *testing.T) {
	t.Setenv("TEST_WEBHOOK", "https://example.com/hook?token=secret$literal")
	base := "webhook:\n  name: example\n  url: '${TEST_WEBHOOK}'\n  body: |\n    {\"content\": {{ json .message }}}\n" + simpleConfig
	c := parseTestConfig(t, base)
	if c.Webhook.URL != os.Getenv("TEST_WEBHOOK") {
		t.Fatal("incorrect URL expansion")
	}
	for name, input := range map[string]string{
		"missing env":          strings.Replace(base, "${TEST_WEBHOOK}", "${NOT_CONFIGURED}", 1),
		"malformed expression": strings.Replace(base, "${TEST_WEBHOOK}", "${MISSING", 1),
		"empty name":           strings.Replace(base, "name: example", "name: ''", 1),
		"bad body":             strings.Replace(base, "{\"content\": {{ json .message }}}", "{{ .missing }}", 1),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ParseConfig([]byte(input), os.LookupEnv)
			if err == nil {
				t.Fatal("accepted invalid webhook")
			}
			if strings.Contains(err.Error(), "secret$literal") {
				t.Fatal("leaked webhook URL")
			}
		})
	}
}

func TestGeneratedUUIDIdentity(t *testing.T) {
	t.Setenv("HC_UUID_SEED", testUUIDSeed)
	input := strings.Replace(simpleConfig, "  - uuid: "+testUUID+"\n    name:", "  - name:", 1)
	first := parseTestConfig(t, input).Checks[0]
	// Independently calculated HMAC-SHA256 vector, formatted as UUIDv8.
	if first.UUID != "fe068ca6-5f98-8083-9d7c-11f4e4fd90a8" {
		t.Fatalf("generated UUID = %s", first.UUID)
	}
	changed := strings.Replace(input, "every: 5m", "every: 10m\n    tags: backup\n    region: region-a", 1)
	if got := parseTestConfig(t, changed).Checks[0]; got.UUID != first.UUID || got.Key != first.Key {
		t.Fatal("schedule or metadata changed check identity")
	}
	if got := parseTestConfig(t, strings.Replace(input, "Heartbeat", "Renamed", 1)); got.Checks[0].UUID == first.UUID {
		t.Fatal("renaming an automatically identified check did not change its UUID")
	}
	t.Setenv("HC_UUID_SEED", "another-seed-with-at-least-32-bytes")
	if got := parseTestConfig(t, input); got.Checks[0].UUID == first.UUID {
		t.Fatal("changing seed did not change generated UUID")
	}
	if got := parseTestConfig(t, simpleConfig); got.Checks[0].UUID != testUUID {
		t.Fatal("seed changed explicit UUID")
	}
}

func TestGeneratedUUIDRequiresSeed(t *testing.T) {
	input := strings.Replace(simpleConfig, "  - uuid: "+testUUID+"\n    name:", "  - name:", 1)
	for _, seed := range []string{"", "too-short"} {
		t.Setenv("HC_UUID_SEED", seed)
		_, err := ParseConfig([]byte(input), os.LookupEnv)
		if err == nil || !strings.Contains(err.Error(), "HC_UUID_SEED") {
			t.Fatalf("missing seed validation: %v", err)
		}
		if seed != "" && strings.Contains(err.Error(), seed) {
			t.Fatal("seed leaked in validation error")
		}
		if got := parseTestConfig(t, simpleConfig); got.Checks[0].UUID != testUUID {
			t.Fatal("explicit UUID requires a seed")
		}
	}
}

func TestGeneratedUUIDConflictExplainsDuplicateName(t *testing.T) {
	t.Setenv("HC_UUID_SEED", testUUIDSeed)
	input := strings.Replace(simpleConfig, "  - uuid: "+testUUID+"\n    name:", "  - name:", 1)
	duplicate := input + strings.Split(input, "checks:\n")[1]
	_, err := ParseConfig([]byte(duplicate), os.LookupEnv)
	if err == nil || !strings.Contains(err.Error(), "checks[1].name") || !strings.Contains(err.Error(), "duplicate name") || !strings.Contains(err.Error(), "checks[0]") {
		t.Fatalf("duplicate generated identity error = %v", err)
	}
	// Explicit UUIDs allow equal display names but cannot collide with a generated UUID.
	explicit := strings.Replace(simpleConfig, testUUID, "fe068ca6-5f98-8083-9d7c-11f4e4fd90a8", 1)
	_, err = ParseConfig([]byte(input+strings.Split(explicit, "checks:\n")[1]), os.LookupEnv)
	if err == nil || !strings.Contains(err.Error(), "duplicate UUID") || strings.Contains(err.Error(), "duplicate name") {
		t.Fatalf("explicit/generated UUID conflict error = %v", err)
	}
	parseTestConfig(t, input+strings.Split(simpleConfig, "checks:\n")[1])
}
