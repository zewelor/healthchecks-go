// Command dashboard-preview serves a local dashboard with sample data and no database.
package main

import (
	"log"
	"net/http"
	"time"

	"healthchecks-go/internal/healthchecks"
)

const sampleConfig = `public_url: http://127.0.0.1:18765
checks:
  - uuid: 11111111-1111-4111-8111-111111111111
    name: Database backup
    description: Encrypted backup of the primary database.
    every: 24h
    grace: 1h
  - uuid: 22222222-2222-4222-8222-222222222222
    name: Kubernetes heartbeat
    every: 5m
    grace: 1m
  - uuid: 33333333-3333-4333-8333-333333333333
    name: Metrics collection
    every: 10m
    grace: 2m
  - uuid: 44444444-4444-4444-8444-444444444444
    name: DNS synchronization
    every: 30m
    grace: 5m
  - uuid: 55555555-5555-4555-8555-555555555555
    name: Package updates
    every: 1h
    grace: 10m
  - uuid: 66666666-6666-4666-8666-666666666666
    name: New integration
    every: 1h
    grace: 10m
  - uuid: 77777777-7777-4777-8777-777777777777
    name: Archive cleanup
    every: 24h
    grace: 1h
    paused: true
  - uuid: 88888888-8888-4888-8888-888888888888
    name: Certificate check
    every: 12h
    grace: 1h
`

func main() {
	config, err := healthchecks.ParseConfig([]byte(sampleConfig), func(string) (string, bool) { return "", false })
	if err != nil {
		log.Fatal(err)
	}
	offset := -20 * time.Minute
	engine := healthchecks.NewEngine(config, func() time.Time { return time.Now().Add(offset) })
	mode, err := healthchecks.ParsePingLogMode("off")
	if err != nil {
		log.Fatal(err)
	}
	engine.SetPingLogMode(mode)
	ping := func(id, kind, body string) {
		found, err := engine.Ping(id, healthchecks.Input{Kind: kind, Body: body})
		if err != nil {
			log.Fatal(err)
		}
		if !found {
			log.Fatalf("sample check %s not found", id)
		}
	}
	ping("11111111-1111-4111-8111-111111111111", "start", "Starting backup")
	offset = -19 * time.Minute
	ping("11111111-1111-4111-8111-111111111111", "success", "Backup completed")
	ping("88888888-8888-4888-8888-888888888888", "success", "Certificate is valid")
	offset = -4 * time.Minute
	ping("55555555-5555-4555-8555-555555555555", "success", "Packages checked")
	offset = -3 * time.Minute
	ping("44444444-4444-4444-8444-444444444444", "fail", "Sample synchronization failure")
	ping("33333333-3333-4333-8333-333333333333", "success", "Metrics collected")
	offset = -time.Minute
	ping("22222222-2222-4222-8222-222222222222", "success", "Heartbeat received")
	ping("55555555-5555-4555-8555-555555555555", "start", "Checking for updates")
	offset = 0

	log.Print("Sample dashboard (in-memory, no database): http://127.0.0.1:18765/")
	log.Fatal(http.ListenAndServe("127.0.0.1:18765", healthchecks.NewHandler(engine, "")))
}
