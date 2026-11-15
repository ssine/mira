package clauderuntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// The login keeper is the single refresher of each subscription account's
// claude.ai login on this Node.
//
// OAuth refresh tokens rotate and a renewal revokes the previous tokens. Every
// Claude CLI process on the account (turns, titles, plan-usage reads) renews on
// its own once the access token is within five minutes of expiry, so the
// processes alive at that moment race for the renewal; a process that loses
// can be left holding revoked tokens ("OAuth token revoked"). The keeper renews
// well before that window, under the CLI's own refresh locks, and writes the
// account's credentials file atomically. A running CLI notices the file change
// before its next request and adopts the new tokens, so no CLI process reaches
// its own renewal window while the Node runs. If the keeper cannot renew (the
// Node is down, the network fails), the CLI's own renewal remains the fallback.
const (
	keeperLead     = 45 * time.Minute // renew when the access token expires sooner
	keeperInterval = 5 * time.Minute
	keeperTimeout  = 30 * time.Second // the CLI's own refresh request timeout
	// The pinned CLI's claude.ai OAuth client and token endpoint.
	claudeOAuthTokenURL = "https://platform.claude.com/v1/oauth/token"
	claudeOAuthClientID = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	// The CLI's lock options (proper-lockfile): a directory whose mtime the
	// holder refreshes every 5 s and that counts as stale after 60 s.
	refreshLockStale  = 60 * time.Second
	refreshLockUpdate = 5 * time.Second
)

// LogFunc receives keeper events. Values never include tokens.
type LogFunc func(message string, fields map[string]any)

type loginKeeper struct {
	mu       sync.Mutex
	tokenURL string
	now      func() time.Time
	client   func(proxy func(*http.Request) (*url.URL, error)) *http.Client
}

func newLoginKeeper() *loginKeeper {
	return &loginKeeper{tokenURL: claudeOAuthTokenURL, now: time.Now, client: func(proxy func(*http.Request) (*url.URL, error)) *http.Client {
		return &http.Client{Timeout: keeperTimeout, Transport: &http.Transport{Proxy: proxy, ForceAttemptHTTP2: true, TLSHandshakeTimeout: 10 * time.Second}}
	}}
}

// StartLoginKeeper renews every subscription login on this Node until Close.
// Only the Node calls it; foreground `mira claude` launchers do not.
func (m *Manager) StartLoginKeeper(logf LogFunc) {
	if logf == nil {
		logf = func(string, map[string]any) {}
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.mu.Lock()
	if m.closed || m.keeperCancel != nil {
		m.mu.Unlock()
		cancel()
		return
	}
	m.keeperCancel = cancel
	m.mu.Unlock()
	go func() {
		timer := time.NewTimer(30 * time.Second) // after start-up work
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
			}
			m.keepLogins(logf)
			timer.Reset(keeperInterval)
		}
	}()
}

func (m *Manager) keepLogins(logf LogFunc) {
	entries, err := os.ReadDir(m.accountsRoot)
	if err != nil {
		return
	}
	for _, entry := range entries {
		id := entry.Name()
		if !entry.IsDir() || !accountIDPattern.MatchString(id) {
			continue
		}
		account, err := m.readAccount(id)
		if err != nil || account.AuthType != "subscription" {
			continue
		}
		outcome, err := m.KeepLogin(id)
		switch {
		case err != nil:
			logf("claude login renewal failed", map[string]any{"nodeAccountId": id, "error": err.Error()})
		case outcome == "renewed" || outcome == "renewed_elsewhere":
			logf("claude login renewed", map[string]any{"nodeAccountId": id, "outcome": outcome})
		}
	}
}

// KeepLogin renews one account's login when it expires within keeperLead.
// Outcomes: not_needed, no_login, renewed, renewed_elsewhere, lock_busy.
func (m *Manager) KeepLogin(id string) (string, error) {
	home, err := m.accountHome(id)
	if err != nil {
		return "", err
	}
	m.keeper.mu.Lock()
	defer m.keeper.mu.Unlock()
	path := filepath.Join(home, ".credentials.json")
	stored, err := readOAuthStore(path)
	if err != nil {
		return "no_login", nil
	}
	if !m.keeper.due(stored) {
		return "not_needed", nil
	}
	unlock, err := acquireRefreshLocks(home)
	if errors.Is(err, errRefreshLockBusy) {
		return "lock_busy", nil
	}
	if err != nil {
		return "", err
	}
	defer unlock()
	// Like the CLI, re-read under the lock: another process may have renewed.
	locked, err := readOAuthStore(path)
	if err != nil {
		return "no_login", nil
	}
	if locked.refreshToken() != stored.refreshToken() && !m.keeper.due(locked) {
		return "renewed_elsewhere", nil
	}
	if !m.keeper.due(locked) {
		return "not_needed", nil
	}
	env, release, err := m.egress.Acquire(id)
	if err != nil {
		return "", fmt.Errorf("account egress unavailable: %w", err)
	}
	defer release()
	proxy := http.ProxyFromEnvironment
	if value := env["HTTPS_PROXY"]; value != "" {
		if parsed, err := url.Parse(value); err == nil {
			proxy = http.ProxyURL(parsed)
		}
	}
	renewed, err := m.keeper.refresh(locked, proxy)
	if err != nil {
		return "", err
	}
	if err := writeOAuthStoreAtomic(path, locked.with(renewed, m.keeper.now())); err != nil {
		// The provider already rotated; the old refresh token is spent. This
		// is the one unrecoverable case, so report it loudly.
		return "", fmt.Errorf("renewed login could not be saved: %w", err)
	}
	return "renewed", nil
}

// oauthStore keeps the credentials document as read, so unknown fields of
// the CLI's store survive a renewal.
type oauthStore struct {
	document map[string]json.RawMessage
	oauth    map[string]json.RawMessage
}

func readOAuthStore(path string) (*oauthStore, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > 1024*1024 {
		return nil, errors.New("credentials file is not a small regular file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	store := &oauthStore{}
	if err := json.Unmarshal(data, &store.document); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(store.document["claudeAiOauth"], &store.oauth); err != nil || store.oauth == nil {
		return nil, errors.New("no claude.ai login")
	}
	if store.refreshToken() == "" {
		return nil, errors.New("login has no refresh token")
	}
	return store, nil
}

func (store *oauthStore) text(key string) string {
	var value string
	_ = json.Unmarshal(store.oauth[key], &value)
	return value
}

func (store *oauthStore) refreshToken() string { return store.text("refreshToken") }

func (store *oauthStore) expiresAt() (time.Time, bool) {
	var value int64
	if json.Unmarshal(store.oauth["expiresAt"], &value) != nil || value <= 0 {
		return time.Time{}, false
	}
	return time.UnixMilli(value), true
}

func (store *oauthStore) scopes() []string {
	var value []string
	_ = json.Unmarshal(store.oauth["scopes"], &value)
	return value
}

func (keeper *loginKeeper) due(store *oauthStore) bool {
	expires, ok := store.expiresAt()
	return ok && expires.Sub(keeper.now()) < keeperLead
}

type renewedTokens struct {
	AccessToken           string `json:"access_token"`
	RefreshToken          string `json:"refresh_token"`
	ExpiresIn             int64  `json:"expires_in"`
	RefreshTokenExpiresIn *int64 `json:"refresh_token_expires_in"`
	Scope                 string `json:"scope"`
}

// refresh sends the CLI's refresh request. Its context is never cancelled by
// Node shutdown: abandoning a request the provider accepted would spend the
// refresh token without saving its replacement.
func (keeper *loginKeeper) refresh(store *oauthStore, proxy func(*http.Request) (*url.URL, error)) (*renewedTokens, error) {
	clientID := store.text("clientId")
	if clientID == "" {
		clientID = claudeOAuthClientID
	}
	body := map[string]any{"grant_type": "refresh_token", "refresh_token": store.refreshToken(), "client_id": clientID}
	if scopes := store.scopes(); len(scopes) > 0 {
		body["scope"] = strings.Join(scopes, " ")
	}
	encoded, _ := json.Marshal(body)
	request, _ := http.NewRequest(http.MethodPost, keeper.tokenURL, bytes.NewReader(encoded))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	response, err := keeper.client(proxy).Do(request)
	if err != nil {
		return nil, fmt.Errorf("token refresh request failed: %w", err)
	}
	defer response.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(response.Body, 64*1024))
	if response.StatusCode != http.StatusOK {
		var failure struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(data, &failure)
		if failure.Error == "" {
			failure.Error = "unexpected response"
		}
		return nil, fmt.Errorf("token refresh rejected: HTTP %d %s", response.StatusCode, failure.Error)
	}
	var tokens renewedTokens
	if err := json.Unmarshal(data, &tokens); err != nil || tokens.AccessToken == "" || tokens.ExpiresIn <= 0 {
		return nil, errors.New("token refresh returned an invalid response")
	}
	if tokens.RefreshToken == "" {
		tokens.RefreshToken = store.refreshToken()
	}
	return &tokens, nil
}

// with returns the document with the renewed tokens, as the CLI stores them.
func (store *oauthStore) with(tokens *renewedTokens, now time.Time) []byte {
	oauth := map[string]json.RawMessage{}
	for key, value := range store.oauth {
		oauth[key] = value
	}
	set := func(key string, value any) {
		encoded, _ := json.Marshal(value)
		oauth[key] = encoded
	}
	set("accessToken", tokens.AccessToken)
	set("refreshToken", tokens.RefreshToken)
	set("expiresAt", now.Add(time.Duration(tokens.ExpiresIn)*time.Second).UnixMilli())
	if tokens.RefreshTokenExpiresIn != nil && *tokens.RefreshTokenExpiresIn > 0 {
		set("refreshTokenExpiresAt", now.Add(time.Duration(*tokens.RefreshTokenExpiresIn)*time.Second).UnixMilli())
	}
	if scopes := strings.Fields(tokens.Scope); len(scopes) > 0 {
		set("scopes", scopes)
	}
	document := map[string]json.RawMessage{}
	for key, value := range store.document {
		document[key] = value
	}
	encoded, _ := json.Marshal(oauth)
	document["claudeAiOauth"] = encoded
	result, _ := json.Marshal(document)
	return result
}

func writeOAuthStoreAtomic(path string, data []byte) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".credentials-*.tmp")
	if err != nil {
		return err
	}
	name := file.Name()
	defer os.Remove(name)
	if err := file.Chmod(0600); err != nil {
		file.Close()
		return err
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	if dir, err := os.Open(filepath.Dir(path)); err == nil {
		_ = dir.Sync()
		dir.Close()
	}
	return nil
}

var errRefreshLockBusy = errors.New("another process is renewing the login")

// Lock waits mirror the CLI's: a few one-second retries, then give up until the
// next keeper pass. Tests shorten them.
var (
	lockAttempts = 10
	lockWait     = time.Second
)

// acquireRefreshLocks takes the CLI's two refresh locks in its order: the
// current `<config>/.oauth_refresh.lock` and the legacy `<realpath>.lock`,
// both proper-lockfile directories. A busy lock is retried briefly; a lock
// whose mtime is older than the CLI's stale threshold is taken over.
func acquireRefreshLocks(home string) (func(), error) {
	real, err := filepath.EvalSymlinks(home)
	if err != nil {
		real = home
	}
	current, err := acquireDirLock(filepath.Join(home, ".oauth_refresh.lock"))
	if err != nil {
		return nil, err
	}
	legacy, err := acquireDirLock(real + ".lock")
	if err != nil {
		current()
		return nil, err
	}
	return func() { legacy(); current() }, nil
}

func acquireDirLock(path string) (func(), error) {
	for attempt := 0; ; attempt++ {
		err := os.Mkdir(path, 0700)
		if err == nil {
			break
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		if info, statErr := os.Stat(path); statErr == nil && time.Since(info.ModTime()) > refreshLockStale {
			_ = os.Remove(path)
			continue
		}
		if attempt >= lockAttempts {
			return nil, errRefreshLockBusy
		}
		time.Sleep(lockWait)
	}
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(refreshLockUpdate)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				now := time.Now()
				_ = os.Chtimes(path, now, now)
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			close(done)
			_ = os.Remove(path)
		})
	}, nil
}
