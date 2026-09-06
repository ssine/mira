// Package miraserver is Mira's native Go control plane. It embeds the Web UI,
// owns the PostgreSQL-backed API, and terminates the Node and App Server
// WebSocket channels without a Node.js runtime.
package miraserver

import (
	"context"
	"errors"
	"log"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/ssine/mira/node/internal/miraserver/accountsampler"
	serverchannel "github.com/ssine/mira/node/internal/miraserver/channel"
	"github.com/ssine/mira/node/internal/miraserver/foundation"
	serverimports "github.com/ssine/mira/node/internal/miraserver/imports"
	miranodes "github.com/ssine/mira/node/internal/miraserver/nodes"
	serverviews "github.com/ssine/mira/node/internal/miraserver/views"
	"github.com/ssine/mira/node/internal/webassets"
)

type Config struct {
	Foundation foundation.Config
	Version    string
	Logger     *log.Logger
}

type Server struct {
	config      Config
	pool        *pgxpool.Pool
	auth        *foundation.AuthService
	nodes       *miranodes.Service
	channel     *serverchannel.Channel
	views       *serverviews.Service
	imports     *serverimports.Service
	sampler     *accountsampler.Sampler
	authState   foundation.AuthState
	http        *http.Server
	listener    net.Listener
	stopErasure func()
	closeOnce   sync.Once
}

func New(ctx context.Context, configuration Config) (*Server, error) {
	if configuration.Version == "" {
		configuration.Version = "development"
	}
	if configuration.Logger == nil {
		configuration.Logger = log.Default()
	}
	pool, err := foundation.OpenPool(ctx, configuration.Foundation)
	if err != nil {
		return nil, err
	}
	if err := foundation.InitializeDatabase(ctx, pool); err != nil {
		pool.Close()
		return nil, err
	}
	auth := foundation.NewAuthService(pool, foundation.AuthOptions{
		SecureCookies: configuration.Foundation.SecureCookies, TrustProxyHeaders: configuration.Foundation.TrustProxyHeaders,
	})
	authState, err := auth.Initialize(ctx)
	if err != nil {
		pool.Close()
		return nil, err
	}
	nodeService := miranodes.New(pool, miranodes.Options{TrustProxyHeaders: configuration.Foundation.TrustProxyHeaders})
	broker, err := serverchannel.New(serverchannel.Options{
		Database: pool, Nodes: nodeService, Auth: auth, TrustProxyHeaders: configuration.Foundation.TrustProxyHeaders,
	})
	if err != nil {
		pool.Close()
		return nil, err
	}
	viewService := serverviews.New(pool)
	importService := serverimports.New(
		&serverimports.PostgresRepository{Pool: pool}, broker.Capabilities(), &importThreadStore{pool: pool},
	)
	importService.ThreadList = func(ctx context.Context, storeID string, limit int, threadID *string, archived *bool) (any, error) {
		return viewService.ListThreads(ctx, storeID, limit, threadID, archived)
	}
	importService.Audit = func(ctx context.Context, event foundation.AuditEvent) error {
		return foundation.AppendAudit(ctx, pool, event, configuration.Foundation.TrustProxyHeaders)
	}
	if count, err := importService.NormalizeImportedThreadHistoryModes(ctx); err != nil {
		_ = broker.Close()
		pool.Close()
		return nil, err
	} else if count > 0 {
		configuration.Logger.Printf("normalized %d imported Codex thread histories", count)
	}
	sampler := accountsampler.New(pool, nodeService, broker, broker.Accounts(), accountsampler.Options{
		Logger: slog.New(slog.NewTextHandler(configuration.Logger.Writer(), nil)),
	})
	server := &Server{
		config: configuration, pool: pool, auth: auth, nodes: nodeService, channel: broker,
		views: viewService, imports: importService, sampler: sampler, authState: authState,
	}
	server.stopErasure = StartThreadErasureWorker(ctx, pool, configuration.Logger)
	server.sampler.Start(ctx)
	server.http = &http.Server{
		Handler:           server,
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    1 << 20,
	}
	return server, nil
}

func (server *Server) Pool() *pgxpool.Pool { return server.pool }

func (server *Server) Serve(listener net.Listener) error {
	server.listener = listener
	err := server.http.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (server *Server) ListenAndServe() error {
	address := net.JoinHostPort(server.config.Foundation.ListenHost, strconv.Itoa(server.config.Foundation.ListenPort))
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return err
	}
	server.config.Logger.Printf("Mira Server %s listening on http://%s", server.config.Version, address)
	if !server.authState.AdminConfigured {
		server.config.Logger.Print("No Mira administrator is configured. Run: mira server admin set-password admin")
	}
	return server.Serve(listener)
}

func (server *Server) Shutdown(ctx context.Context) error {
	var result error
	server.closeOnce.Do(func() {
		result = server.http.Shutdown(ctx)
		if result != nil {
			// Shutdown waits for active HTTP handlers. Once its deadline is
			// exhausted, force their connections closed so a worker update is
			// bounded by the Supervisor grace period rather than pool.Close.
			result = errors.Join(result, server.http.Close())
		}
		if server.stopErasure != nil {
			server.stopErasure()
		}
		server.sampler.Close()
		if err := server.channel.Close(); result == nil {
			result = err
		}
		server.pool.Close()
	})
	return result
}

func (server *Server) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	if server.channel.Handles(request) {
		server.channel.ServeHTTP(response, request)
		return
	}
	tracked := &trackingResponseWriter{ResponseWriter: response}
	defer func() {
		if recovered := recover(); recovered != nil {
			server.config.Logger.Printf("request panic: %v", recovered)
			if !tracked.written {
				_ = foundation.WriteErrorJSON(tracked, 500, "internal server error", "internal_error")
			}
		}
	}()
	if webassets.Serve(tracked, request) {
		return
	}
	if err := server.route(request.Context(), tracked, request); err != nil {
		server.writeRouteError(tracked, err)
	}
}

type trackingResponseWriter struct {
	http.ResponseWriter
	written bool
}

func (response *trackingResponseWriter) WriteHeader(status int) {
	if response.written {
		return
	}
	response.written = true
	response.ResponseWriter.WriteHeader(status)
}

func (response *trackingResponseWriter) Write(payload []byte) (int, error) {
	if !response.written {
		response.WriteHeader(http.StatusOK)
	}
	return response.ResponseWriter.Write(payload)
}

func (response *trackingResponseWriter) Flush() {
	if !response.written {
		response.WriteHeader(http.StatusOK)
	}
	if flusher, ok := response.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (server *Server) writeRouteError(response http.ResponseWriter, err error) {
	var own *HTTPError
	if errors.As(err, &own) {
		_ = foundation.WriteErrorJSON(response, own.Status, own.Message, own.Code)
		return
	}
	var base *foundation.HTTPError
	if errors.As(err, &base) {
		_ = foundation.WriteErrorJSON(response, base.Status, base.Message, base.Code)
		return
	}
	var channelError *serverchannel.Error
	if errors.As(err, &channelError) {
		_ = foundation.WriteErrorJSON(response, channelError.Status, channelError.Message, channelError.Code)
		return
	}
	server.config.Logger.Printf("request failed: %v", err)
	_ = foundation.WriteErrorJSON(response, 500, "internal server error", "internal_error")
}

type authOptions struct {
	CSRF       bool
	ClientType string
	NodeID     string
}

func (server *Server) authorize(ctx context.Context, response http.ResponseWriter, request *http.Request, actorType string, options authOptions) (*foundation.Principal, error) {
	principal, err := server.auth.Authenticate(ctx, request, options.ClientType)
	if err != nil {
		return nil, err
	}
	if principal == nil {
		_ = foundation.WriteErrorJSON(response, 401, "authentication required", "authentication_required")
		return nil, nil
	}
	if principal.Revoked {
		_ = foundation.WriteErrorJSON(response, 403, "Node credential is revoked", "node_revoked")
		return nil, nil
	}
	if !server.auth.Permits(principal, actorType) {
		_ = foundation.WriteErrorJSON(response, 403, "permission denied", "permission_denied")
		return nil, nil
	}
	if options.NodeID != "" && (principal.Kind != "node" || principal.NodeID != options.NodeID) {
		_ = foundation.WriteErrorJSON(response, 403, "Node identity does not match route", "node_identity_mismatch")
		return nil, nil
	}
	csrf := options.CSRF
	// Browser administrators authenticate with a cookie, so every mutating
	// request needs CSRF protection even when the route also accepts a
	// non-browser client type (for example CLI or Codex bearer credentials).
	if !csrf && principal.Kind == "admin" {
		csrf = true
	}
	if csrf && request.Method != http.MethodGet && request.Method != http.MethodHead && request.Method != http.MethodOptions && !server.auth.ValidCSRF(request, principal) {
		_ = foundation.WriteErrorJSON(response, 403, "invalid CSRF token", "invalid_csrf")
		return nil, nil
	}
	return principal, nil
}

func writeJSON(response http.ResponseWriter, status int, value any) error {
	return foundation.WriteJSON(response, status, value, nil)
}

func safeStoreID(value string) (string, bool) {
	if len(value) < 1 || len(value) > 128 {
		return "", false
	}
	for _, character := range value {
		if !(character >= 'a' && character <= 'z') && !(character >= 'A' && character <= 'Z') && !(character >= '0' && character <= '9') && character != '.' && character != '_' && character != '-' {
			return "", false
		}
	}
	return value, true
}

var (
	v1StorePattern        = regexp.MustCompile(`^/v1/stores/([^/]+)$`)
	v2StorePattern        = regexp.MustCompile(`^/v2/stores/([^/]+)$`)
	v2HistoryPattern      = regexp.MustCompile(`^/v2/stores/([^/]+)/threads/([^/]+)/history$`)
	v2CommitPattern       = regexp.MustCompile(`^/v2/stores/([^/]+)/commits$`)
	v1StoreEventsPattern  = regexp.MustCompile(`^/v1/stores/([^/]+)/events$`)
	v1ThreadEventsPattern = regexp.MustCompile(`^/v1/stores/([^/]+)/threads/([^/]+)/events$`)
	v1RebuildPattern      = regexp.MustCompile(`^/v1/stores/([^/]+)/rebuild$`)
)

func pathMatch(pattern *regexp.Regexp, path string) []string { return pattern.FindStringSubmatch(path) }

func boundedQueryInteger(value string, fallback, minimum, maximum int64) int64 {
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed < minimum || parsed > maximum {
		return fallback
	}
	return parsed
}

func requireStoreID(value string) (string, error) {
	storeID, ok := safeStoreID(value)
	if !ok {
		return "", &HTTPError{Status: 400, Code: "invalid_request", Message: "invalid store id"}
	}
	return storeID, nil
}
