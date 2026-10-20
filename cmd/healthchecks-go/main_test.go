package main

import (
	"bytes"
	"errors"
	"flag"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"syscall"
	"testing"
)

func TestLogLevel(t *testing.T) {
	previous := slog.Default()
	t.Cleanup(func() { slog.SetDefault(previous) })
	for _, tc := range []struct {
		level string
		want  []string
	}{
		{"", []string{"INFO", "WARN", "ERROR"}},
		{"debug", []string{"DEBUG", "INFO", "WARN", "ERROR"}},
		{"info", []string{"INFO", "WARN", "ERROR"}},
		{"WARN", []string{"WARN", "ERROR"}},
		{"error", []string{"ERROR"}},
	} {
		t.Run(tc.level, func(t *testing.T) {
			var output bytes.Buffer
			if err := configureLogging(tc.level, &output); err != nil {
				t.Fatal(err)
			}
			slog.Debug("debug message")
			slog.Info("info message")
			slog.Warn("warn message")
			slog.Error("error message")
			lines := strings.Split(strings.TrimSpace(output.String()), "\n")
			if len(lines) != len(tc.want) {
				t.Fatalf("unexpected log entries: %s", output.String())
			}
			for i, level := range tc.want {
				if !strings.Contains(lines[i], "level="+level) {
					t.Fatalf("entry %d: %s, want %s", i, lines[i], level)
				}
			}
		})
	}
}

func TestInvalidLogLevelRejectsStartup(t *testing.T) {
	for _, value := range []string{"verbose", "info+1"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("HC_LOG_LEVEL", value)
			if _, err := runCommand(t, "-config", "../../config.example.yml", "-check"); err == nil || !strings.Contains(err.Error(), "HC_LOG_LEVEL") {
				t.Fatalf("invalid level accepted: %v", err)
			}
		})
	}
}

func TestHTTPServerErrorsAtEveryLogLevel(t *testing.T) {
	previous := slog.Default()
	previousLevel := slog.SetLogLoggerLevel(slog.LevelInfo)
	t.Cleanup(func() {
		slog.SetDefault(previous)
		slog.SetLogLoggerLevel(previousLevel)
	})
	for _, level := range []string{"debug", "info", "warn", "error"} {
		t.Run(level, func(t *testing.T) {
			var output bytes.Buffer
			if err := configureLogging(level, &output); err != nil {
				t.Fatal(err)
			}
			server := &http.Server{}
			if err := server.Serve(&failingAcceptListener{}); !errors.Is(err, net.ErrClosed) {
				t.Fatalf("unexpected server error: %v", err)
			}
			if got := output.String(); !strings.Contains(got, "level=ERROR") || !strings.Contains(got, "http: Accept error:") || !strings.Contains(got, syscall.EMFILE.Error()) {
				t.Fatalf("HTTP accept failure missing at ERROR severity: %q", got)
			}
		})
	}
}

type failingAcceptListener struct{ failed bool }

func (l *failingAcceptListener) Accept() (net.Conn, error) {
	if !l.failed {
		l.failed = true
		return nil, &net.OpError{Op: "accept", Net: "tcp", Err: syscall.EMFILE}
	}
	return nil, net.ErrClosed
}

func (*failingAcceptListener) Close() error   { return nil }
func (*failingAcceptListener) Addr() net.Addr { return &net.TCPAddr{} }

func TestCheckConfigurationWithoutMetricsKey(t *testing.T) {
	t.Setenv("HC_METRICS_KEY", "")
	if _, err := runCommand(t, "-config", "../../config.example.yml", "-check"); err != nil {
		t.Fatalf("configuration validation without metrics key: %v", err)
	}
}

func TestListPingURLs(t *testing.T) {
	output, err := runCommand(t, "-config", "../../config.example.yml", "-check", "-list-ping-urls")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output, "http://localhost:8000/ping/11111111-1111-4111-8111-111111111111") || !strings.Contains(output, "Daily backup") {
		t.Fatalf("missing ping URLs: %s", output)
	}
	if len(strings.Split(strings.TrimSpace(output), "\n")) != 2 {
		t.Fatalf("unexpected listing: %s", output)
	}
}

func TestListenErrorPreservesCause(t *testing.T) {
	_, err := runCommand(t, "-config", "../../config.example.yml", "-listen", "127.0.0.1:99999")
	var cause *net.OpError
	if !errors.As(err, &cause) || cause.Op != "listen" || !strings.Contains(err.Error(), "99999") {
		t.Fatalf("listener error lost its cause or invalid port: %v", err)
	}
}

func runCommand(t *testing.T, args ...string) (string, error) {
	t.Helper()
	previousArgs, previousFlags, previousStdout := os.Args, flag.CommandLine, os.Stdout
	previousLogger := slog.Default()
	defer slog.SetDefault(previousLogger)
	defer func() { os.Args, flag.CommandLine, os.Stdout = previousArgs, previousFlags, previousStdout }()
	f, err := os.CreateTemp(t.TempDir(), "stdout")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	os.Stdout = f
	os.Args = append([]string{"healthchecks-go"}, args...)
	flag.CommandLine = flag.NewFlagSet(os.Args[0], flag.ContinueOnError)
	runErr := run()
	output, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	return string(output), runErr
}
