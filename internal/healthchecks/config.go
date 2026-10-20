package healthchecks

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"text/template"
	"time"
	_ "time/tzdata"

	"github.com/robfig/cron/v3"
	"go.yaml.in/yaml/v3"
)

const (
	MaxChecks      = 128
	HistoryLimit   = 20
	BodyLimit      = 8192
	MaxConfigBytes = 1 << 20
)

var (
	uuidPattern   = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	regionPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	envPattern    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	cronParser    = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
)

type Config struct {
	SiteName  string         `yaml:"-"`
	PublicURL string         `yaml:"public_url"`
	Webhook   *WebhookConfig `yaml:"webhook"`
	Checks    []CheckConfig  `yaml:"checks"`
}

type CheckConfig struct {
	UUID             string   `yaml:"uuid"`
	Name             string   `yaml:"name"`
	Every            string   `yaml:"every"`
	Cron             string   `yaml:"cron"`
	Timezone         string   `yaml:"timezone"`
	Grace            string   `yaml:"grace"`
	Paused           bool     `yaml:"paused"`
	Notify           bool     `yaml:"notify"`
	DependsOnRegions []string `yaml:"depends_on_regions"`
	Key              string   `yaml:"-"`
	interval         time.Duration
	grace            time.Duration
	schedule         cron.Schedule
}

type WebhookConfig struct {
	Name string `yaml:"name"`
	URL  string `yaml:"url"`
	Body string `yaml:"body"`
	body *template.Template
}

type WebhookEvent struct {
	Status           string    `json:"status"`
	Name             string    `json:"name"`
	UniqueKey        string    `json:"unique_key"`
	DependsOnRegions []string  `json:"depends_on_regions"`
	Reason           string    `json:"reason"`
	Time             time.Time `json:"time"`
	URL              string    `json:"url"`
	Message          string    `json:"message"`
}

func ParseConfig(data []byte, lookup func(string) (string, bool)) (*Config, error) {
	if len(data) > MaxConfigBytes {
		return nil, errors.New("configuration exceeds 1 MiB")
	}
	var node yaml.Node
	if err := yaml.Unmarshal(data, &node); err != nil {
		return nil, errors.New("invalid YAML syntax")
	}
	if err := validateYAMLNode(&node, "", 0); err != nil {
		return nil, err
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var c Config
	if err := dec.Decode(&c); err != nil {
		return nil, errors.New("invalid YAML: check syntax, field names, duplicate keys, and value types")
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return nil, errors.New("configuration must contain one YAML document")
	}
	if lookup != nil {
		c.SiteName, _ = lookup("HC_SITE_NAME")
	}
	c.SiteName = strings.TrimSpace(c.SiteName)
	if c.SiteName == "" {
		c.SiteName = "Healthchecks"
	}
	if !validHTTPURL(c.PublicURL, false) {
		return nil, errors.New("public_url must be an HTTP(S) origin with port 1-65535 if set; paths, user information, query, and fragment are not supported")
	}
	c.PublicURL = strings.TrimRight(c.PublicURL, "/")
	if len(c.Checks) == 0 || len(c.Checks) > MaxChecks {
		return nil, fmt.Errorf("checks must contain between 1 and %d entries", MaxChecks)
	}
	if c.Webhook != nil {
		if err := c.Webhook.validate(lookup); err != nil {
			return nil, err
		}
	}
	seen := make(map[string]int)
	keys := make(map[string]bool)
	for i := range c.Checks {
		x := &c.Checks[i]
		if err := x.validate(); err != nil {
			return nil, fmt.Errorf("checks[%d]: %w", i, err)
		}
		if previous, exists := seen[x.UUID]; exists {
			return nil, fmt.Errorf("checks[%d].uuid: duplicate UUID (already used by checks[%d])", i, previous)
		}
		if keys[x.Key] {
			return nil, fmt.Errorf("checks[%d]: unique_key collision", i)
		}
		seen[x.UUID], keys[x.Key] = i, true
		if x.Notify && c.Webhook == nil {
			return nil, fmt.Errorf("checks[%d]: notify requires a webhook", i)
		}
	}
	return &c, nil
}

func validateYAMLNode(n *yaml.Node, field string, depth int) error {
	if depth > 16 || n.Kind == yaml.AliasNode {
		return errors.New("YAML aliases and deeply nested configuration are not supported")
	}
	if n.Kind == yaml.ScalarNode {
		if len(n.Value) > 16384 {
			return errors.New("configuration field exceeds 16 KiB")
		}
		if n.Tag == "!!bool" && (field == "paused" || field == "notify") {
			return nil
		}
		if n.Tag != "!!str" {
			return errors.New("configuration values must be strings or declared booleans; null is not allowed")
		}
	}
	if n.Kind == yaml.MappingNode {
		for i := 0; i < len(n.Content); i += 2 {
			if n.Content[i].Tag != "!!str" {
				return errors.New("configuration field names must be strings")
			}
			if err := validateYAMLNode(n.Content[i+1], n.Content[i].Value, depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	for _, child := range n.Content {
		if err := validateYAMLNode(child, field, depth+1); err != nil {
			return err
		}
	}
	return nil
}

func (c *CheckConfig) validate() error {
	if strings.TrimSpace(c.Name) == "" || len(c.Name) > 100 {
		return errors.New("name must contain 1 to 100 bytes")
	}
	if !uuidPattern.MatchString(c.UUID) {
		return errors.New("uuid must be a lowercase canonical UUID")
	}
	if (c.Every == "") == (c.Cron == "") {
		return errors.New("specify exactly one of every or cron")
	}
	var err error
	c.grace, err = time.ParseDuration(c.Grace)
	if err != nil || c.grace <= 0 {
		return errors.New("grace must be a positive duration")
	}
	if c.Every != "" {
		if c.Timezone != "" {
			return errors.New("timezone is only valid with cron")
		}
		c.interval, err = time.ParseDuration(c.Every)
		if err != nil || c.interval <= 0 {
			return errors.New("every must be a positive duration")
		}
	} else {
		if len(c.Cron) > 100 {
			return errors.New("cron exceeds 100 bytes")
		}
		if c.Timezone == "" {
			c.Timezone = "UTC"
		}
		if len(c.Timezone) > 100 {
			return errors.New("timezone exceeds 100 bytes")
		}
		if c.Timezone == "Local" {
			return errors.New("timezone Local is not supported; use UTC or an explicit IANA timezone")
		}
		if _, err := time.LoadLocation(c.Timezone); err != nil {
			return errors.New("unknown IANA timezone")
		}
		c.schedule, err = parseSchedule(c.Cron, c.Timezone)
		if err != nil {
			return err
		}
	}
	if len(c.DependsOnRegions) > 32 {
		return errors.New("too many region dependencies")
	}
	regions := make(map[string]bool)
	for _, r := range c.DependsOnRegions {
		if !regionPattern.MatchString(r) || regions[r] {
			return errors.New("invalid or duplicate region dependency")
		}
		regions[r] = true
	}
	h := sha1.Sum([]byte(strings.ReplaceAll(c.UUID, "-", "")[:16]))
	c.Key = hex.EncodeToString(h[:])
	return nil
}

func (c *CheckConfig) next(after time.Time) time.Time {
	if c.interval != 0 {
		return after.Add(c.interval)
	}
	return c.schedule.Next(after)
}

func validHTTPURL(s string, allowPathAndQuery bool) bool {
	if len(s) > 4096 {
		return false
	}
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
		return false
	}
	if !allowPathAndQuery && ((u.EscapedPath() != "" && u.EscapedPath() != "/") || strings.ContainsAny(s, "?#")) {
		return false
	}
	if port := u.Port(); port != "" {
		n, err := strconv.ParseUint(port, 10, 16)
		return err == nil && n > 0
	}
	return !strings.HasSuffix(u.Host, ":")
}

func expandURL(value string, lookup func(string) (string, bool)) (string, error) {
	var b strings.Builder
	for value != "" {
		i := strings.IndexByte(value, '$')
		if i < 0 {
			b.WriteString(value)
			break
		}
		b.WriteString(value[:i])
		value = value[i:]
		end := strings.IndexByte(value, '}')
		if !strings.HasPrefix(value, "${") || end < 0 || !envPattern.MatchString(value[2:end]) {
			return "", errors.New("webhook.url contains an invalid ENV expression; use ${NAME}")
		}
		v, ok := lookup(value[2:end])
		if !ok || v == "" {
			return "", errors.New("webhook.url references a missing or empty ENV variable")
		}
		b.WriteString(v)
		value = value[end+1:]
		if b.Len() > 4096 {
			return "", errors.New("webhook.url exceeds 4096 bytes")
		}
	}
	return b.String(), nil
}

func (w *WebhookConfig) validate(lookup func(string) (string, bool)) error {
	if strings.TrimSpace(w.Name) == "" || len(w.Name) > 100 {
		return errors.New("webhook.name must contain 1 to 100 bytes")
	}
	var err error
	w.URL, err = expandURL(w.URL, lookup)
	if err != nil {
		return err
	}
	if !validHTTPURL(w.URL, true) {
		return errors.New("webhook.url must resolve to an absolute HTTP(S) URL with port 1-65535 if set, without user information or fragment")
	}
	if len(w.Body) > 16384 {
		return errors.New("webhook.body exceeds 16 KiB")
	}
	if w.Body == "" {
		return nil
	}
	w.body, err = template.New("webhook").Option("missingkey=error").Funcs(template.FuncMap{"json": func(v any) (string, error) { b, e := json.Marshal(v); return string(b), e }}).Parse(w.Body)
	if err != nil {
		return errors.New("invalid webhook.body template")
	}
	for _, status := range []string{"down", "up"} {
		_, err := w.render(WebhookEvent{Status: status, Name: "Example \"check\"", Message: "Example\nmessage", Time: time.Unix(0, 0).UTC(), DependsOnRegions: []string{}})
		if err != nil {
			return err
		}
	}
	return nil
}

func (w *WebhookConfig) render(event WebhookEvent) ([]byte, error) {
	data, err := json.Marshal(event)
	if err != nil {
		return nil, errors.New("cannot encode webhook event")
	}
	if w.body == nil {
		return data, nil
	}
	var fields map[string]any
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil, errors.New("cannot prepare webhook event")
	}
	var b bytes.Buffer
	if err := w.body.Execute(limitedWriter{writer: &b, limit: 32768}, fields); err != nil {
		return nil, errors.New("cannot render webhook.body template")
	}
	if b.Len() > 32768 || !json.Valid(b.Bytes()) {
		return nil, errors.New("webhook.body must render valid JSON within 32 KiB")
	}
	return b.Bytes(), nil
}

type limitedWriter struct {
	writer *bytes.Buffer
	limit  int
}

func (w limitedWriter) Write(p []byte) (int, error) {
	if len(p) > w.limit-w.writer.Len() {
		return 0, errors.New("rendered webhook body exceeds limit")
	}
	return w.writer.Write(p)
}
