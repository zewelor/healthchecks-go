package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"healthchecks-go/internal/healthchecks"
)

func main() {
	if err := run(); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}

func run() error {
	path := flag.String("config", "config.yml", "YAML configuration file")
	listen := flag.String("listen", ":8000", "HTTP listen address")
	check := flag.Bool("check", false, "validate configuration and required environment without starting the server")
	list := flag.Bool("list-ping-urls", false, "validate configuration and print check names and ping URLs without starting the server")
	flag.Parse()
	if flag.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	key := os.Getenv("HC_METRICS_KEY")
	data, err := healthchecks.ReadConfigFile(*path)
	if err != nil {
		return err
	}
	c, err := healthchecks.ParseConfig(data, os.LookupEnv)
	if err != nil {
		return err
	}
	if *list {
		for _, check := range c.Checks {
			fmt.Printf("%q\t%s/ping/%s\n", check.Name, c.PublicURL, check.UUID)
		}
		return nil
	}
	if *check {
		fmt.Println("Configuration is valid.")
		return nil
	}
	listener, err := net.Listen("tcp", *listen)
	if err != nil {
		return fmt.Errorf("HTTP server failed to listen: %w", err)
	}
	defer listener.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	e, err := healthchecks.OpenEngine(ctx, c, time.Now, os.LookupEnv)
	if err != nil {
		return err
	}
	defer e.Close()
	w := healthchecks.NewConfigWatcher(*path, e, os.LookupEnv)
	if _, err := w.Check(); err != nil {
		return err
	}
	var wg sync.WaitGroup
	wg.Go(func() { e.RunWebhook(ctx) })
	wg.Go(func() {
		w.Run(ctx, func(err error) {
			if err != nil {
				log.Printf("Configuration reload rejected: %v", err)
			} else {
				log.Print("Configuration reloaded.")
			}
		})
	})
	wg.Go(func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := e.Evaluate(); err != nil {
					log.Printf("State evaluation failed: %v", err)
				}
			}
		}
	})
	server := &http.Server{Addr: *listen, Handler: healthchecks.NewHandler(e, key), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	wg.Go(func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			log.Printf("HTTP shutdown failed: %v", err)
			_ = server.Close()
		}
	})
	log.Printf("Starting HTTP server on %s; check state is stored in PostgreSQL.", *listen)
	err = server.Serve(listener)
	stop()
	wg.Wait()
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("HTTP server failed to listen or serve: %w", err)
	}
	return nil
}
