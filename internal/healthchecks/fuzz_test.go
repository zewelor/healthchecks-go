package healthchecks

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func FuzzUnknownPing(f *testing.F) {
	for _, s := range []string{"", "/start", "/256", "?rid=invalid", "//", "/../"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, suffix string) {
		if len(suffix) > 1024 {
			t.Skip()
		}
		c, err := ParseConfig([]byte(simpleConfig), os.LookupEnv)
		if err != nil {
			t.Fatal(err)
		}
		e := NewEngine(c, time.Now)
		h := NewHandler(e, "test-token")
		r := &http.Request{Method: "GET", URL: &url.URL{Path: "/ping/" + otherUUID + suffix}, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(nil))}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 404 {
			t.Fatalf("unknown identifier: HTTP %d", w.Code)
		}
		if v := e.Snapshot(); len(v) != 1 || len(v[0].Events) != 0 || v[0].Status != "awaiting" {
			t.Fatal("unknown ping changed configured state")
		}
	})
}

func FuzzWebhookJSON(f *testing.F) {
	for _, s := range []string{"normal", "quote \" newline\n", `</script>`, "☃️", strings.Repeat("\x00", 8192), strings.Repeat("界", maxWebhookMessageRunes+1)} {
		f.Add(s)
	}
	c, err := ParseConfig([]byte("webhook:\n  name: test\n  url: https://example.com/hook\n  body: |\n    {\"content\": {{ json .message }}}\n"+simpleConfig), os.LookupEnv)
	if err != nil {
		f.Fatal(err)
	}
	f.Fuzz(func(t *testing.T, message string) {
		if len(message) > 8192 || !utf8.ValidString(message) {
			t.Skip()
		}
		message = limitRunes(message, maxWebhookMessageRunes)
		b, err := c.Webhook.render(WebhookEvent{Message: message})
		if err != nil {
			t.Fatal(err)
		}
		var payload struct {
			Content string `json:"content"`
		}
		if err := json.Unmarshal(b, &payload); err != nil {
			t.Fatal(err)
		}
		if payload.Content != message {
			t.Fatal("JSON template did not preserve text")
		}
	})
}
