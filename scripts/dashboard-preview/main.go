package main

import (
	_ "embed"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"time"

	"healthchecks-go/internal/healthchecks"
)

//go:embed checks.yml
var checksYAML []byte

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	listen := flag.String("listen", "127.0.0.1:18081", "HTTP listen address")
	flag.Parse()
	if flag.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	engine, err := newPreviewEngine(time.Now)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", *listen)
	if err != nil {
		return fmt.Errorf("HTTP server failed to listen: %w", err)
	}
	defer listener.Close()
	log.Printf("Starting dashboard preview on http://%s", listener.Addr())
	return http.Serve(listener, healthchecks.NewHandler(engine, ""))
}

func newPreviewEngine(now func() time.Time) (*healthchecks.Engine, error) {
	config, err := healthchecks.ParseConfig(checksYAML, nil)
	if err != nil {
		return nil, fmt.Errorf("parse embedded demo checks: %w", err)
	}

	base := now()
	clock := base.Add(-2 * time.Hour)
	seeding := true
	engine := healthchecks.NewEngine(config, func() time.Time {
		if seeding {
			return clock
		}
		return now()
	})
	seed := func(index int, kind string, offset time.Duration) error {
		clock = base.Add(offset)
		found, err := engine.Ping(config.Checks[index].UUID, healthchecks.Input{Kind: kind})
		if err != nil {
			return fmt.Errorf("seed demo check: %w", err)
		}
		if !found {
			return errors.New("seed demo check: configured check was not found")
		}
		return nil
	}
	if err := seed(0, "success", -5*time.Minute); err != nil {
		return nil, err
	}
	if err := seed(0, "start", -time.Minute); err != nil {
		return nil, err
	}
	if err := seed(1, "success", -8*time.Minute); err != nil {
		return nil, err
	}
	if err := seed(2, "fail", -3*time.Minute); err != nil {
		return nil, err
	}
	seeding = false
	return engine, nil
}
