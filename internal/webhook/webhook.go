package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// Event types emitted by the gateway.
const (
	EventSpendTracked  = "spend_tracked"
	EventBudgetCrossed = "budget_crossed"
	EventKeyCreated    = "key_created"
	EventKeyDeleted    = "key_deleted"
	EventRateLimited   = "rate_limited"
)

// Payload is the webhook delivery body.
type Payload struct {
	Event     string    `json:"event"`
	Timestamp time.Time `json:"timestamp"`
	Data      any       `json:"data"`
}

// Config defines a webhook subscription.
type Config struct {
	URL        string   `yaml:"url" json:"url"`
	Secret     string   `yaml:"secret" json:"secret"`           // HMAC-SHA256 signing secret
	Events     []string `yaml:"events" json:"events"`           // events to subscribe to (empty = all)
	MaxRetries int      `yaml:"max_retries" json:"max_retries"` // retry count on failure (default 3)
}

// Dispatcher sends webhook events to configured endpoints.
type Dispatcher struct {
	configs []Config
	client  *http.Client
	logger  *slog.Logger
	queue   chan Payload
	wg      sync.WaitGroup
}

// NewDispatcher creates a webhook dispatcher.
// If no configs are provided, it's a no-op (Emit returns immediately).
func NewDispatcher(configs []Config, logger *slog.Logger) *Dispatcher {
	d := &Dispatcher{
		configs: configs,
		client: &http.Client{
			Timeout: 10 * time.Second,
		},
		logger: logger,
		queue:  make(chan Payload, 1024),
	}
	if len(configs) > 0 {
		d.wg.Add(1)
		go d.worker()
	}
	return d
}

// Emit enqueues a webhook event for async delivery.
func (d *Dispatcher) Emit(event string, data any) {
	// Nil-safe: a *Dispatcher stored in an interface is not a nil interface, so
	// a caller that has no dispatcher still reaches this method. Panicking there
	// takes down whatever emitted the event — key creation, in the case that
	// found this.
	if d == nil || len(d.configs) == 0 {
		return
	}
	select {
	case d.queue <- Payload{Event: event, Timestamp: time.Now().UTC(), Data: data}:
	default:
		d.logger.Warn("webhook queue full, dropping event", "event", event)
	}
}

// Close drains the queue and shuts down the worker.
func (d *Dispatcher) Close() {
	close(d.queue)
	d.wg.Wait()
}

func (d *Dispatcher) worker() {
	defer d.wg.Done()
	for payload := range d.queue {
		for _, cfg := range d.configs {
			if !d.shouldDeliver(cfg, payload.Event) {
				continue
			}
			maxRetries := cfg.MaxRetries
			if maxRetries <= 0 {
				maxRetries = 3
			}
			var lastErr error
			for attempt := 0; attempt <= maxRetries; attempt++ {
				if attempt > 0 {
					// Exponential backoff: 1s, 2s, 4s...
					backoff := time.Duration(1<<(attempt-1)) * time.Second
					time.Sleep(backoff)
				}
				if err := d.deliver(cfg, payload); err != nil {
					lastErr = err
					d.logger.Warn("webhook delivery attempt failed",
						"url", cfg.URL,
						"event", payload.Event,
						"attempt", attempt+1,
						"max_retries", maxRetries,
						"error", err,
					)
					continue
				}
				lastErr = nil
				break
			}
			if lastErr != nil {
				d.logger.Error("webhook delivery exhausted retries",
					"url", cfg.URL,
					"event", payload.Event,
					"error", lastErr,
				)
			}
		}
	}
}

func (d *Dispatcher) shouldDeliver(cfg Config, event string) bool {
	if len(cfg.Events) == 0 {
		return true
	}
	for _, e := range cfg.Events {
		if e == event {
			return true
		}
	}
	return false
}

func (d *Dispatcher) deliver(cfg Config, payload Payload) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.URL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Ubiquum-Gateway-Webhook/1.0")

	// Sign with HMAC-SHA256 if secret is configured
	if cfg.Secret != "" {
		sig := sign(body, cfg.Secret)
		req.Header.Set("X-Ubiquum-Signature", "sha256="+sig)
	}

	resp, err := d.client.Do(req)
	if err != nil {
		return fmt.Errorf("http: %w", err)
	}
	resp.Body.Close()

	if resp.StatusCode >= 400 {
		return fmt.Errorf("http status %d", resp.StatusCode)
	}
	return nil
}

func sign(payload []byte, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(payload)
	return hex.EncodeToString(mac.Sum(nil))
}
