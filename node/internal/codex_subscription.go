package node

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	codexAuthLimit             = 1024 * 1024
	codexSubscriptionRecheck   = 30 * time.Second
	maximumSubscriptionInstant = int64(8_640_000_000_000_000) // ECMAScript date range
)

// codexSubscriptionCache remembers the billing period last read from one Codex
// home. Reports are built on every heartbeat, so the file is checked at most
// every codexSubscriptionRecheck and parsed only after it changed.
type codexSubscriptionCache struct {
	home      string
	checkedAt time.Time
	modified  time.Time
	size      int64
	view      map[string]any
}

func (cache *codexSubscriptionCache) read(home string, now time.Time) map[string]any {
	home, err := effectiveAccountHome(home)
	if err != nil {
		return nil
	}
	if cache.home == home && now.Sub(cache.checkedAt) < codexSubscriptionRecheck {
		return cache.view
	}
	path := filepath.Join(home, "auth.json")
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > codexAuthLimit {
		*cache = codexSubscriptionCache{home: home, checkedAt: now}
		return nil
	}
	unchanged := cache.home == home && cache.modified.Equal(info.ModTime()) && cache.size == info.Size()
	if !unchanged {
		cache.view = readCodexSubscription(path)
	}
	cache.home, cache.checkedAt, cache.modified, cache.size = home, now, info.ModTime(), info.Size()
	return cache.view
}

// readCodexSubscription returns only the ChatGPT billing period recorded in the
// local login token. Tokens and identity claims never leave this function. The
// claims are a local observation for display, so no signature is verified.
func readCodexSubscription(path string) map[string]any {
	file, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, codexAuthLimit+1))
	if err != nil || len(data) > codexAuthLimit {
		return nil
	}
	var auth struct {
		Mode   string `json:"auth_mode"`
		Tokens *struct {
			IDToken string `json:"id_token"`
		} `json:"tokens"`
	}
	if json.Unmarshal(data, &auth) != nil || auth.Tokens == nil || auth.Mode != "" && auth.Mode != "chatgpt" {
		return nil
	}
	parts := strings.Split(auth.Tokens.IDToken, ".")
	if len(parts) != 3 {
		return nil
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return nil
	}
	var claims struct {
		Auth struct {
			Plan    string `json:"chatgpt_plan_type"`
			Start   any    `json:"chatgpt_subscription_active_start"`
			End     any    `json:"chatgpt_subscription_active_until"`
			Checked any    `json:"chatgpt_subscription_last_checked"`
		} `json:"https://api.openai.com/auth"`
	}
	if json.Unmarshal(payload, &claims) != nil || claims.Auth.Plan == "free" {
		return nil
	}
	end, ok := subscriptionInstant(claims.Auth.End)
	if !ok {
		return nil
	}
	view := map[string]any{"endsAt": end}
	if start, ok := subscriptionInstant(claims.Auth.Start); ok {
		view["startsAt"] = start
	}
	if checked, ok := subscriptionInstant(claims.Auth.Checked); ok {
		view["checkedAt"] = checked
	}
	return view
}

func subscriptionInstant(value any) (int64, bool) {
	text, ok := value.(string)
	if !ok || len(text) > 64 {
		return 0, false
	}
	parsed, err := time.Parse(time.RFC3339Nano, text)
	if err != nil {
		return 0, false
	}
	milliseconds := parsed.UnixMilli()
	return milliseconds, milliseconds > 0 && milliseconds <= maximumSubscriptionInstant
}
