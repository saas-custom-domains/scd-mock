package mock

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"time"
)

const webhookAttempts = 4 // first try plus 3 retries

// event is one webhook delivery: Name is the `event` field, Body the payload.
type event struct {
	Name string
	Body any
}

// webhooks delivers events in order from a single background worker so API
// handlers never wait on the receiver.
type webhooks struct {
	target  string // URL including the token query param
	display string // URL without the query, for logs
	token   string
	client  *http.Client
	backoff time.Duration
	log     *log.Logger
	queue   chan event
}

func newWebhooks(cfg Config, logger *log.Logger) *webhooks {
	w := &webhooks{log: logger}
	if cfg.WebhookURL == "" {
		return w
	}
	u, err := url.Parse(cfg.WebhookURL)
	if err != nil {
		logger.Printf("[webhook] disabled: %v", err)
		return w
	}
	q := u.Query()
	q.Set("token", cfg.WebhookToken)
	u.RawQuery = q.Encode()
	w.target = u.String()
	u.RawQuery = ""
	w.display = u.String()
	w.token = cfg.WebhookToken
	w.client = &http.Client{Timeout: 5 * time.Second}
	w.backoff = cfg.WebhookBackoff
	w.queue = make(chan event, 1000)
	go w.run()
	return w
}

// send queues events without blocking; it drops events if the queue is full.
func (w *webhooks) send(events ...event) {
	if w.queue == nil {
		return
	}
	for _, e := range events {
		select {
		case w.queue <- e:
		default:
			w.log.Printf("[webhook] %s dropped: queue full", e.Name)
		}
	}
}

func (w *webhooks) run() {
	for e := range w.queue {
		w.deliver(e)
	}
}

func (w *webhooks) deliver(e event) {
	body, err := json.Marshal(e.Body)
	if err != nil {
		w.log.Printf("[webhook] %s not sent: %v", e.Name, err)
		return
	}
	for attempt := 1; attempt <= webhookAttempts; attempt++ {
		err := w.post(body)
		if err == nil {
			w.log.Printf("[webhook] %s -> %s ok (attempt %d)", e.Name, w.display, attempt)
			return
		}
		if attempt == webhookAttempts {
			w.log.Printf("[webhook] %s -> %s failed, giving up after %d attempts: %v", e.Name, w.display, attempt, err)
			return
		}
		wait := w.backoff << (attempt - 1)
		w.log.Printf("[webhook] %s -> %s failed (attempt %d): %v; retrying in %s", e.Name, w.display, attempt, err, wait)
		time.Sleep(wait)
	}
}

func (w *webhooks) post(body []byte) error {
	req, err := http.NewRequest(http.MethodPost, w.target, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "SaasCustomDomains.com")
	req.Header.Set("X-SCD-Webhook-Token", w.token)
	resp, err := w.client.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			return ue.Err // url.Error would print the token
		}
		return err
	}
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}
