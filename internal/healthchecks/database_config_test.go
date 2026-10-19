package healthchecks

import (
	"strings"
	"testing"
)

func TestDatabaseConfigUsesSeparateFieldsAndEscapesPassword(t *testing.T) {
	t.Setenv("PGHOST", "ignored.example.test")
	password := "space quote' backslash\\ and $(literal)"
	values := map[string]string{"DB_HOST": "database.example.test", "DB_NAME": "healthchecks", "DB_USER": "check-user", "DB_PASSWORD": password, "DB_SSLMODE": "verify-full"}
	lookup := func(k string) (string, bool) { v, ok := values[k]; return v, ok }
	c, err := databaseConfig(lookup)
	if err != nil {
		t.Fatal(err)
	}
	if c.Host != values["DB_HOST"] || c.Database != values["DB_NAME"] || c.User != values["DB_USER"] || c.Password != password || c.Port != 5432 {
		t.Fatal("connection fields were not preserved")
	}
	if c.TLSConfig == nil || c.TLSConfig.ServerName != values["DB_HOST"] {
		t.Fatal("TLS settings were not derived from actual DB_HOST")
	}
	for _, test := range []struct{ key, value string }{{"DB_PORT", "65536"}, {"DB_PORT", "0"}, {"DB_SSLMODE", "prefer"}, {"DB_NAME", ""}} {
		t.Run(test.key+test.value, func(t *testing.T) {
			changed := make(map[string]string)
			for k, v := range values {
				changed[k] = v
			}
			changed[test.key] = test.value
			_, err := databaseConfig(func(k string) (string, bool) { v, ok := changed[k]; return v, ok })
			if err == nil || strings.Contains(err.Error(), password) {
				t.Fatalf("invalid configuration error: %v", err)
			}
		})
	}
}
