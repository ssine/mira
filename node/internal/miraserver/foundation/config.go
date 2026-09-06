package foundation

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
)

const (
	DefaultListenHost         = "127.0.0.1"
	DefaultListenPort         = 8787
	DefaultDatabaseURL        = "postgresql://mira:mira-local@127.0.0.1:55432/mira"
	DefaultMaxBodyBytes int64 = 64 * 1024 * 1024
	DefaultPoolSize     int32 = 10
)

// Config contains the process-independent Mira Server foundation settings.
// Environment names intentionally stay compatible with the released server.
type Config struct {
	ListenHost         string
	ListenPort         int
	DatabaseURL        string
	CodexStoreEndpoint string
	SecureCookies      bool
	TrustProxyHeaders  bool
	MaxBodyBytes       int64
	PoolSize           int32
}

func LoadConfig() (Config, error) {
	return ConfigFromLookup(os.LookupEnv)
}

func ConfigFromLookup(lookup func(string) (string, bool)) (Config, error) {
	host := envOr(lookup, "LISTEN_HOST", DefaultListenHost)
	portText := envOr(lookup, "LISTEN_PORT", strconv.Itoa(DefaultListenPort))
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return Config{}, fmt.Errorf("LISTEN_PORT must be an integer between 1 and 65535")
	}
	databaseURL := envOr(lookup, "DATABASE_URL", DefaultDatabaseURL)
	if databaseURL == "" {
		return Config{}, fmt.Errorf("DATABASE_URL must not be empty")
	}
	endpoint, present := lookup("MIRA_CODEX_STORE_ENDPOINT")
	if !present {
		endpoint = "http://" + net.JoinHostPort(host, strconv.Itoa(port))
	}
	endpoint = strings.TrimSuffix(endpoint, "/")
	if endpoint == "" {
		return Config{}, fmt.Errorf("MIRA_CODEX_STORE_ENDPOINT must not be empty")
	}
	secureCookies, securePresent := lookup("MIRA_SECURE_COOKIES")
	trustProxy, _ := lookup("MIRA_TRUST_PROXY_HEADERS")
	return Config{
		ListenHost: host, ListenPort: port, DatabaseURL: databaseURL,
		CodexStoreEndpoint: endpoint,
		SecureCookies:      !securePresent || secureCookies != "false",
		TrustProxyHeaders:  trustProxy == "true",
		MaxBodyBytes:       DefaultMaxBodyBytes, PoolSize: DefaultPoolSize,
	}, nil
}

func envOr(lookup func(string) (string, bool), name, fallback string) string {
	if value, ok := lookup(name); ok {
		return value
	}
	return fallback
}
