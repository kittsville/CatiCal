package httpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	turnstileField     = "cf-turnstile-response"
	turnstileVerifyURL = "https://challenges.cloudflare.com/turnstile/v0/siteverify"
	turnstileScript    = "https://challenges.cloudflare.com/turnstile/v0/api.js"
	turnstileOrigin    = "https://challenges.cloudflare.com"
)

// TurnstileVerifier checks a widget token. Tests inject a fake.
type TurnstileVerifier interface {
	Verify(ctx context.Context, token, remoteIP string) error
}

type siteverifyClient struct {
	http   *http.Client
	url    string
	secret string
}

func newSiteverify(client *http.Client, endpoint, secret string) TurnstileVerifier {
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	if endpoint == "" {
		endpoint = turnstileVerifyURL
	}
	return &siteverifyClient{http: client, url: endpoint, secret: secret}
}

func (c *siteverifyClient) Verify(ctx context.Context, token, remoteIP string) error {
	form := url.Values{
		"secret":   {c.secret},
		"response": {token},
	}
	if remoteIP != "" {
		form.Set("remoteip", remoteIP)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 1<<16))
	if err != nil {
		return err
	}
	var parsed struct {
		Success bool `json:"success"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return err
	}
	if !parsed.Success {
		return fmt.Errorf("turnstile rejected")
	}
	return nil
}

func (c Config) turnstileRequired() bool {
	return c.TurnstileSecret != ""
}

func (c Config) verifier() TurnstileVerifier {
	if c.TurnstileVerifier != nil {
		return c.TurnstileVerifier
	}
	return newSiteverify(nil, "", c.TurnstileSecret)
}

func checkTurnstile(w http.ResponseWriter, r *http.Request, cfg Config, onFail func(string, int)) bool {
	if !cfg.turnstileRequired() {
		return true
	}
	token := strings.TrimSpace(r.FormValue(turnstileField))
	if token == "" {
		onFail("complete the CAPTCHA", http.StatusBadRequest)
		return false
	}
	if err := cfg.verifier().Verify(r.Context(), token, peerIP(r)); err != nil {
		onFail("CAPTCHA failed", http.StatusForbidden)
		return false
	}
	return true
}
