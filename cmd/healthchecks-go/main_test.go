package main

import (
	"errors"
	"flag"
	"net"
	"os"
	"strings"
	"testing"
)

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
