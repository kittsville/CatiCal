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
	turnstileMaxToken  = 2048
	turnstileVerifyURL = "https://challenges.cloudflare.com/turnstile/v0/siteverify"
	turnstileScript    = "https://challenges.cloudflare.com/turnstile/v0/api.js"
	turnstileOrigin    = "https://challenges.cloudflare.com"
	turnstileCreate    = "create"
	turnstileManage    = "manage"
)

// TurnstileVerifier checks a widget token. Tests inject a fake.
type TurnstileVerifier interface {
	Verify(ctx context.Context, token, remoteIP, action string) error
}

type siteverifyClient struct {
	http   *http.Client
	url    string
	secret string
	hosts  map[string]struct{}
}

func newSiteverify(client *http.Client, endpoint, secret string, hosts map[string]struct{}) TurnstileVerifier {
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	if endpoint == "" {
		endpoint = turnstileVerifyURL
	}
	return &siteverifyClient{http: client, url: endpoint, secret: secret, hosts: hosts}
}

func (c *siteverifyClient) Verify(ctx context.Context, token, remoteIP, action string) error {
	if len(c.hosts) == 0 {
		return fmt.Errorf("turnstile hostnames not configured")
	}
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
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return fmt.Errorf("siteverify %d", res.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, 1<<16))
	if err != nil {
		return err
	}
	var parsed struct {
		Success  bool   `json:"success"`
		Action   string `json:"action"`
		Hostname string `json:"hostname"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return err
	}
	if !parsed.Success || parsed.Action != action {
		return fmt.Errorf("turnstile rejected")
	}
	host := strings.ToLower(strings.TrimSpace(parsed.Hostname))
	if _, ok := c.hosts[host]; !ok {
		return fmt.Errorf("turnstile hostname rejected")
	}
	return nil
}

func (c Config) turnstileRequired() bool {
	return c.TurnstileSecret != ""
}

func (c Config) expectedHostnames() map[string]struct{} {
	out := make(map[string]struct{})
	for _, p := range strings.Split(c.TurnstileHostnames, ",") {
		p = strings.ToLower(strings.TrimSpace(p))
		if p != "" {
			out[p] = struct{}{}
		}
	}
	if len(out) > 0 {
		return out
	}
	u, err := url.Parse(c.baseURL())
	if err != nil {
		return out
	}
	h := strings.ToLower(u.Hostname())
	if h == "" || h == "localhost" || h == "127.0.0.1" || h == "::1" {
		return out
	}
	out[h] = struct{}{}
	return out
}

func (c Config) verifier() TurnstileVerifier {
	if c.TurnstileVerifier != nil {
		return c.TurnstileVerifier
	}
	return newSiteverify(nil, "", c.TurnstileSecret, c.expectedHostnames())
}

func turnstileClientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		return strings.TrimSpace(strings.Split(xff, ",")[0])
	}
	return peerIP(r)
}

func checkTurnstile(w http.ResponseWriter, r *http.Request, cfg Config, action string, onFail func(string, int)) bool {
	if !cfg.turnstileRequired() {
		return true
	}
	token := strings.TrimSpace(r.FormValue(turnstileField))
	if token == "" || len(token) > turnstileMaxToken {
		onFail("complete the CAPTCHA", http.StatusBadRequest)
		return false
	}
	if err := cfg.verifier().Verify(r.Context(), token, turnstileClientIP(r), action); err != nil {
		onFail("CAPTCHA failed", http.StatusForbidden)
		return false
	}
	return true
}
