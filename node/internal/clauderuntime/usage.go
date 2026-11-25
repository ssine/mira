package clauderuntime

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

// usageReads bounds concurrent plan-usage workers on one Node. Each read starts
// a short-lived native Claude process, so extra callers fail fast instead of
// queueing behind it.
const usageReads = 2

const (
	usagePrepareWait  = 8 * time.Second
	usagePrepareRetry = 30 * time.Minute
)

// The native profile also holds per-project state and can grow large.
const claudeProfileLimit = 16 * 1024 * 1024

// UsageWindow is one claude.ai plan rate-limit window. Utilization is the used
// percentage (0-100); ResetsAt is Unix milliseconds.
type UsageWindow struct {
	Utilization *float64 `json:"utilization"`
	ResetsAt    *int64   `json:"resetsAt"`
}

type UsageModelWindow struct {
	Name string `json:"name"`
	UsageWindow
}

type UsageExtra struct {
	Enabled      bool     `json:"enabled"`
	MonthlyLimit *float64 `json:"monthlyLimit"`
	UsedCredits  *float64 `json:"usedCredits"`
	Utilization  *float64 `json:"utilization"`
	Currency     string   `json:"currency,omitempty"`
}

// PlanUsage is the deliberately small projection of the SDK /usage response
// that leaves the Node. Session costs, transcript behaviors and unknown
// upstream fields are dropped.
type PlanUsage struct {
	SubscriptionType string             `json:"subscriptionType,omitempty"`
	Available        bool               `json:"available"`
	FiveHour         *UsageWindow       `json:"fiveHour,omitempty"`
	SevenDay         *UsageWindow       `json:"sevenDay,omitempty"`
	SevenDayOpus     *UsageWindow       `json:"sevenDayOpus,omitempty"`
	SevenDaySonnet   *UsageWindow       `json:"sevenDaySonnet,omitempty"`
	Models           []UsageModelWindow `json:"models,omitempty"`
	Extra            *UsageExtra        `json:"extra,omitempty"`
	// SubscribedAt is when the subscription began (Unix milliseconds). Claude
	// records no billing period end locally; clients label any estimate as such.
	SubscribedAt *int64 `json:"subscribedAt,omitempty"`
	// AccountKey is equal for logins to the same account on different Nodes.
	AccountKey string `json:"accountKey,omitempty"`
}

// CleanPlanUsage projects the worker's raw get_usage response.
func CleanPlanUsage(raw map[string]any) PlanUsage {
	usage := PlanUsage{}
	if text, ok := raw["subscription_type"].(string); ok && len(text) <= 64 && cleanLabel(text) {
		usage.SubscriptionType = text
	}
	limits, _ := raw["rate_limits"].(map[string]any)
	usage.Available = raw["rate_limits_available"] == true && limits != nil
	if limits == nil {
		return usage
	}
	usage.FiveHour = cleanUsageWindow(limits["five_hour"])
	usage.SevenDay = cleanUsageWindow(limits["seven_day"])
	usage.SevenDayOpus = cleanUsageWindow(limits["seven_day_opus"])
	usage.SevenDaySonnet = cleanUsageWindow(limits["seven_day_sonnet"])
	if models, ok := limits["model_scoped"].([]any); ok {
		for _, value := range models {
			if len(usage.Models) >= 16 {
				break
			}
			record, _ := value.(map[string]any)
			name, _ := record["display_name"].(string)
			window := cleanUsageWindow(record)
			if name == "" || len(name) > 64 || !cleanLabel(name) || window == nil {
				continue
			}
			usage.Models = append(usage.Models, UsageModelWindow{Name: name, UsageWindow: *window})
		}
	}
	if extra, ok := limits["extra_usage"].(map[string]any); ok {
		clean := &UsageExtra{Enabled: extra["is_enabled"] == true}
		clean.MonthlyLimit = usageNumber(extra["monthly_limit"], 0, 1e12)
		clean.UsedCredits = usageNumber(extra["used_credits"], 0, 1e12)
		clean.Utilization = usageNumber(extra["utilization"], 0, 1e6)
		if currency, ok := extra["currency"].(string); ok && len(currency) <= 8 && cleanLabel(currency) {
			clean.Currency = currency
		}
		usage.Extra = clean
	}
	return usage
}

func cleanUsageWindow(value any) *UsageWindow {
	record, _ := value.(map[string]any)
	if record == nil {
		return nil
	}
	window := &UsageWindow{Utilization: usageNumber(record["utilization"], 0, 1e6)}
	if text, ok := record["resets_at"].(string); ok && len(text) <= 64 {
		if parsed, err := time.Parse(time.RFC3339Nano, text); err == nil {
			milliseconds := parsed.UnixMilli()
			window.ResetsAt = &milliseconds
		}
	}
	if window.Utilization == nil && window.ResetsAt == nil {
		return nil
	}
	return window
}

func usageNumber(value any, low, high float64) *float64 {
	number, ok := value.(float64)
	if !ok || math.IsNaN(number) || math.IsInf(number, 0) || number < low || number > high {
		return nil
	}
	return &number
}

func cleanLabel(text string) bool {
	if !utf8.ValidString(text) {
		return false
	}
	for _, character := range text {
		if character < 0x20 || character == 0x7f {
			return false
		}
	}
	return true
}

// usage reads claude.ai plan rate limits for one subscription account through
// the pinned SDK. Mira normally sets CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC,
// which also disables the CLI's usage endpoint; only this read-only worker
// clears it. Telemetry, error reporting, feature flags and claude.ai MCP
// connectors stay disabled, and traffic uses the account's managed egress.
func (m *Manager) usage(accountID string) (any, error) {
	if accountID == "" {
		return nil, errors.New("Choose a Claude subscription account")
	}
	account, err := m.readAccount(accountID)
	if err != nil {
		return nil, err
	}
	if account.AuthType != "subscription" {
		return nil, errors.New("Claude account does not use subscription authentication")
	}
	dir, err := m.usageRuntime(usagePrepareWait)
	if err != nil {
		return nil, err
	}
	select {
	case m.usageSlots <- struct{}{}:
		defer func() { <-m.usageSlots }()
	default:
		return nil, errors.New("Claude plan usage reads are busy; retry shortly")
	}
	env, release, err := m.accountEnvironmentAcquired(accountID, false)
	if err != nil {
		return nil, err
	}
	defer release()
	filtered := env[:0]
	for _, entry := range env {
		if !strings.HasPrefix(entry, "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=") && !strings.HasPrefix(entry, "ENABLE_CLAUDEAI_MCP_SERVERS=") {
			filtered = append(filtered, entry)
		}
	}
	env = append(filtered, "ENABLE_CLAUDEAI_MCP_SERVERS=false", "DISABLE_AUTOUPDATER=1")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, nodeBinary(), filepath.Join(dir, "worker.mjs"))
	cmd.Env = append(env, m.debugLogEnv("usage", accountID, rand.Text())...)
	cmd.Dir = dir
	configureCommand(cmd)
	cmd.Stdin = bytes.NewReader([]byte("{\"action\":\"usage\"}\n"))
	cmd.Cancel = func() error { killCommand(cmd); return nil }
	cmd.WaitDelay = 5 * time.Second
	output := &boundedOutput{limit: 1024 * 1024}
	cmd.Stdout = output
	err = cmd.Run()
	settleProcessGroup(cmd, processSettleGrace) // Before the deferred egress release.
	if err != nil {
		return nil, errors.New("Could not read Claude plan usage")
	}
	var raw map[string]any
	if err = json.Unmarshal(output.Bytes(), &raw); err != nil {
		return nil, errors.New("Claude returned an invalid plan usage response")
	}
	usage := CleanPlanUsage(raw)
	if home, err := m.accountHome(accountID); err == nil {
		usage.SubscribedAt, usage.AccountKey = readProfile(filepath.Join(home, ".claude.json"))
	}
	return usage, nil
}

// usageRuntime returns the prepared SDK directory. A Node restart leaves the
// runtime stopped until someone uses Claude there, so plan usage starts the
// preparation itself and waits briefly; a first download outlives the wait and
// serves a later read. A failed preparation is retried at most every
// usagePrepareRetry, because the Server samples every five minutes.
func (m *Manager) usageRuntime(wait time.Duration) (string, error) {
	deadline := time.Now().Add(wait)
	for {
		m.mu.Lock()
		if m.state == "stopped" || m.state == "error" && time.Since(m.prepareAt) >= usagePrepareRetry {
			m.prepare()
		}
		closed, state, dir, lastError := m.closed, m.state, m.dir, m.lastError
		m.mu.Unlock()
		switch {
		case closed:
			return "", errors.New("Claude manager is shutting down")
		case state == "ready":
			return dir, nil
		case state == "error":
			return "", errors.New("Claude runtime is not ready: " + lastError)
		case !time.Now().Before(deadline):
			return "", errors.New("Claude runtime is being prepared; retry shortly")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// readProfile reads two facts from the profile that the native CLI
// keeps beside its login: when the subscription began and a one-way key that is
// equal for the same Claude account and organization on different Nodes. The
// identifiers themselves and everything else in that file stay on the Node.
func readProfile(path string) (subscribedAt *int64, accountKey string) {
	file, err := os.Open(path)
	if err != nil {
		return nil, ""
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, claudeProfileLimit+1))
	if err != nil || len(data) > claudeProfileLimit {
		return nil, ""
	}
	var profile struct {
		Account struct {
			SubscribedAt   any `json:"subscriptionCreatedAt"`
			AccountID      any `json:"accountUuid"`
			OrganizationID any `json:"organizationUuid"`
		} `json:"oauthAccount"`
	}
	if json.Unmarshal(data, &profile) != nil {
		return nil, ""
	}
	account, _ := profile.Account.AccountID.(string)
	organization, _ := profile.Account.OrganizationID.(string)
	if accountIDPattern.MatchString(account) && accountIDPattern.MatchString(organization) {
		sum := sha256.Sum256([]byte("mira-claude-account-v1\x00" + strings.ToLower(account) + "\x00" + strings.ToLower(organization)))
		accountKey = hex.EncodeToString(sum[:16])
	}
	if text, ok := profile.Account.SubscribedAt.(string); ok && len(text) <= 64 {
		if parsed, err := time.Parse(time.RFC3339Nano, text); err == nil && parsed.UnixMilli() > 0 && parsed.Year() <= 9999 {
			milliseconds := parsed.UnixMilli()
			subscribedAt = &milliseconds
		}
	}
	return subscribedAt, accountKey
}
