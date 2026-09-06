package foundation

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const AdminSessionLifetime = 12 * time.Hour

var nodeTokenPattern = regexp.MustCompile(`(?i)^mira_node_([0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12})_([A-Za-z0-9_-]{43})$`)

var clientTypes = map[string]struct{}{
	"node": {}, "cli": {}, "codex": {}, "app-server": {}, "ssh": {},
}

type DBTX interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

type ParsedNodeToken struct {
	CredentialID string
	Secret       string
	SecretHash   string
}

type Principal struct {
	Kind                string
	ClientType          string
	Transport           string
	SubjectID           string
	Username            string
	SessionID           string
	CSRFToken           string
	CSRFTokenHash       string
	LegacyCSRFTokenHash string
	ExpiresAt           time.Time
	CredentialID        string
	NodeID              string
	NodeKey             string
	EnrollmentID        string
	EnrollmentStatus    string
	Revoked             bool
}

type AuthState struct {
	AdminConfigured bool `json:"adminConfigured"`
}

type LoginResult struct {
	Principal *Principal
	CSRFToken string
	Cookie    string
}

type AuthOptions struct {
	SecureCookies     bool
	TrustProxyHeaders bool
}

type loginFailure struct {
	count   int
	resetAt time.Time
}

type AuthService struct {
	db                DBTX
	secureCookies     bool
	trustProxyHeaders bool
	cookieName        string
	loginFailuresMu   sync.Mutex
	loginFailures     map[string]loginFailure
	now               func() time.Time
}

func NewAuthService(db DBTX, options AuthOptions) *AuthService {
	cookieName := "mira_session"
	if options.SecureCookies {
		cookieName = "__Host-mira_session"
	}
	return &AuthService{
		db: db, secureCookies: options.SecureCookies,
		trustProxyHeaders: options.TrustProxyHeaders, cookieName: cookieName,
		loginFailures: make(map[string]loginFailure), now: time.Now,
	}
}

func TokenHash(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

func NodeSecretHash(secret string) (string, bool) {
	decoded, err := base64.RawURLEncoding.DecodeString(secret)
	if err != nil || len(decoded) != 32 || base64.RawURLEncoding.EncodeToString(decoded) != secret {
		return "", false
	}
	digest := sha256.Sum256(decoded)
	return hex.EncodeToString(digest[:]), true
}

func ParseNodeToken(token string) (ParsedNodeToken, bool) {
	match := nodeTokenPattern.FindStringSubmatch(token)
	if match == nil {
		return ParsedNodeToken{}, false
	}
	secretHash, ok := NodeSecretHash(match[2])
	if !ok {
		return ParsedNodeToken{}, false
	}
	return ParsedNodeToken{CredentialID: strings.ToLower(match[1]), Secret: match[2], SecretHash: secretHash}, true
}

func RandomToken(prefix string) (string, error) {
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		return "", fmt.Errorf("generate random token: %w", err)
	}
	return prefix + "_" + base64.RawURLEncoding.EncodeToString(value), nil
}

func SessionCSRFToken(sessionToken string) string {
	digest := sha256.New()
	_, _ = digest.Write([]byte("mira-admin-csrf-v1\x00"))
	_, _ = digest.Write([]byte(sessionToken))
	return "mcsrf_" + base64.RawURLEncoding.EncodeToString(digest.Sum(nil))
}

func RequestAddress(request *http.Request, trustProxyHeaders bool) string {
	if trustProxyHeaders {
		parts := strings.Split(request.Header.Get("X-Forwarded-For"), ",")
		for index := len(parts) - 1; index >= 0; index-- {
			address := strings.TrimSpace(parts[index])
			if address != "" {
				if net.ParseIP(address) != nil {
					return address
				}
				break
			}
		}
	}
	address := request.RemoteAddr
	if host, _, err := net.SplitHostPort(address); err == nil {
		return host
	}
	return address
}

func (service *AuthService) Initialize(ctx context.Context) (AuthState, error) {
	if _, err := service.db.Exec(ctx,
		`UPDATE mira_admin_sessions SET revoked_at = NOW() WHERE revoked_at IS NULL AND expires_at <= NOW()`,
	); err != nil {
		return AuthState{}, fmt.Errorf("expire administrator sessions: %w", err)
	}
	var count int
	if err := service.db.QueryRow(ctx, `SELECT COUNT(*)::integer FROM mira_admin_users`).Scan(&count); err != nil {
		return AuthState{}, fmt.Errorf("read administrator configuration: %w", err)
	}
	return AuthState{AdminConfigured: count == 1}, nil
}

func (service *AuthService) AdminConfigured(ctx context.Context) (bool, error) {
	var count int
	if err := service.db.QueryRow(ctx, `SELECT COUNT(*)::integer FROM mira_admin_users`).Scan(&count); err != nil {
		return false, fmt.Errorf("read administrator configuration: %w", err)
	}
	return count == 1, nil
}

func (service *AuthService) Authenticate(ctx context.Context, request *http.Request, defaultClientType string) (*Principal, error) {
	if token, ok := bearerToken(request); ok {
		clientType := request.Header.Get("X-Mira-Client-Type")
		if !validClientType(clientType) {
			clientType = defaultClientType
		}
		return service.AuthenticateNodeToken(ctx, token, clientType)
	}
	cookie, err := request.Cookie(service.cookieName)
	if err != nil {
		if errors.Is(err, http.ErrNoCookie) {
			return nil, nil
		}
		return nil, nil
	}
	var principal Principal
	principal.Kind = "admin"
	principal.ClientType = "admin"
	principal.Transport = "cookie"
	principal.Revoked = false
	err = service.db.QueryRow(ctx,
		`UPDATE mira_admin_sessions AS sessions SET last_seen_at = NOW()
		 FROM mira_admin_users AS users
		 WHERE sessions.token_hash = $1
		   AND sessions.admin_user_id = users.admin_user_id
		   AND sessions.revoked_at IS NULL
		   AND sessions.expires_at > NOW()
		 RETURNING sessions.session_id::text, sessions.csrf_token_hash, sessions.expires_at,
		           users.admin_user_id::text, users.username`,
		TokenHash(cookie.Value),
	).Scan(&principal.SessionID, &principal.LegacyCSRFTokenHash, &principal.ExpiresAt, &principal.SubjectID, &principal.Username)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("authenticate administrator session: %w", err)
	}
	principal.CSRFToken = SessionCSRFToken(cookie.Value)
	principal.CSRFTokenHash = TokenHash(principal.CSRFToken)
	return &principal, nil
}

func (service *AuthService) AuthenticateNodeToken(ctx context.Context, token, clientType string) (*Principal, error) {
	parsed, ok := ParseNodeToken(token)
	if !ok {
		return nil, nil
	}
	var credentialID, secretHash, nodeID, nodeKey, approvalStatus string
	var credentialRevoked bool
	err := service.db.QueryRow(ctx,
		`SELECT credentials.credential_id::text, credentials.secret_hash,
		        credentials.revoked_at IS NOT NULL,
		        nodes.node_id::text, nodes.node_key, nodes.approval_status
		 FROM mira_node_credentials AS credentials
		 JOIN codex_nodes AS nodes ON nodes.node_id = credentials.node_id
		 WHERE credentials.credential_id = $1::uuid`,
		parsed.CredentialID,
	).Scan(&credentialID, &secretHash, &credentialRevoked, &nodeID, &nodeKey, &approvalStatus)
	if errors.Is(err, pgx.ErrNoRows) {
		var enrollmentID, enrollmentSecretHash, status string
		err = service.db.QueryRow(ctx,
			`SELECT enrollment_id::text, credential_secret_hash, status
			 FROM mira_node_enrollment_requests WHERE credential_id = $1::uuid`,
			parsed.CredentialID,
		).Scan(&enrollmentID, &enrollmentSecretHash, &status)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && !constantTimeHashEqual(enrollmentSecretHash, parsed.SecretHash)) {
			return nil, nil
		}
		if err != nil {
			return nil, fmt.Errorf("authenticate pending Node credential: %w", err)
		}
		return &Principal{
			Kind: "node", ClientType: normalizedClientType(clientType), Transport: "bearer",
			SubjectID: parsed.CredentialID, CredentialID: parsed.CredentialID,
			EnrollmentID: enrollmentID, EnrollmentStatus: status, Revoked: true,
		}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("authenticate Node credential: %w", err)
	}
	if !constantTimeHashEqual(secretHash, parsed.SecretHash) {
		return nil, nil
	}
	revoked := credentialRevoked || approvalStatus != "approved"
	if !revoked {
		if _, err := service.db.Exec(ctx,
			`WITH used AS (
			   UPDATE mira_node_credentials SET last_used_at = NOW() WHERE credential_id = $1::uuid
			 )
			 UPDATE codex_nodes SET last_authenticated_at = NOW() WHERE node_id = $2::uuid`,
			parsed.CredentialID, nodeID,
		); err != nil {
			return nil, fmt.Errorf("record Node authentication: %w", err)
		}
	}
	return &Principal{
		Kind: "node", ClientType: normalizedClientType(clientType), Transport: "bearer",
		SubjectID: credentialID, CredentialID: credentialID, NodeID: nodeID, NodeKey: nodeKey, Revoked: revoked,
	}, nil
}

func (service *AuthService) Permits(principal *Principal, actorType string) bool {
	if principal == nil || principal.Revoked {
		return false
	}
	switch actorType {
	case "admin":
		return principal.Kind == "admin"
	case "node":
		return principal.Kind == "node"
	case "trusted":
		return principal.Kind == "admin" || principal.Kind == "node"
	default:
		return false
	}
}

func (service *AuthService) ValidCSRF(request *http.Request, principal *Principal) bool {
	if principal == nil || principal.Kind != "admin" || principal.Transport != "cookie" {
		return true
	}
	value := request.Header.Get("X-Mira-Csrf")
	if value == "" {
		return false
	}
	actual := TokenHash(value)
	return constantTimeHashEqual(actual, principal.CSRFTokenHash) ||
		constantTimeHashEqual(actual, principal.LegacyCSRFTokenHash)
}

func (service *AuthService) RefreshCSRF(ctx context.Context, principal *Principal) (string, error) {
	if principal == nil || principal.Kind != "admin" || principal.SessionID == "" || principal.CSRFToken == "" {
		return "", nil
	}
	csrfToken := principal.CSRFToken
	csrfHash := TokenHash(csrfToken)
	if _, err := service.db.Exec(ctx,
		`UPDATE mira_admin_sessions SET csrf_token_hash = $2, last_seen_at = NOW()
		 WHERE session_id = $1::uuid AND revoked_at IS NULL AND expires_at > NOW()`,
		principal.SessionID, csrfHash,
	); err != nil {
		return "", fmt.Errorf("refresh CSRF token: %w", err)
	}
	principal.CSRFTokenHash = csrfHash
	principal.LegacyCSRFTokenHash = csrfHash
	return csrfToken, nil
}

func (service *AuthService) Login(ctx context.Context, request *http.Request, username, password string) (*LoginResult, error) {
	if !ValidAdminUsername(username) {
		return nil, nil
	}
	if delay := service.loginDelay(request, username); delay > 0 {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	var adminUserID, storedUsername, passwordHash string
	err := service.db.QueryRow(ctx,
		`SELECT admin_user_id::text, username, password_hash FROM mira_admin_users WHERE LOWER(username) = LOWER($1)`,
		username,
	).Scan(&adminUserID, &storedUsername, &passwordHash)
	if (errors.Is(err, pgx.ErrNoRows)) || (err == nil && !VerifyPassword(password, passwordHash)) {
		service.recordLoginFailure(request, username)
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read administrator credentials: %w", err)
	}
	service.clearLoginFailures(request, username)
	sessionToken, err := RandomToken("mas")
	if err != nil {
		return nil, err
	}
	csrfToken := SessionCSRFToken(sessionToken)
	expiresAt := service.now().Add(AdminSessionLifetime)
	var sessionID string
	if err := service.db.QueryRow(ctx,
		`INSERT INTO mira_admin_sessions (admin_user_id, token_hash, csrf_token_hash, expires_at)
		 VALUES ($1::uuid, $2, $3, $4) RETURNING session_id::text`,
		adminUserID, TokenHash(sessionToken), TokenHash(csrfToken), expiresAt,
	).Scan(&sessionID); err != nil {
		return nil, fmt.Errorf("create administrator session: %w", err)
	}
	principal := &Principal{
		Kind: "admin", ClientType: "admin", Transport: "cookie", SubjectID: adminUserID,
		Username: storedUsername, SessionID: sessionID, CSRFTokenHash: TokenHash(csrfToken),
		ExpiresAt: expiresAt, Revoked: false,
	}
	secure := ""
	if service.secureCookies {
		secure = "; Secure"
	}
	cookie := fmt.Sprintf("%s=%s; Path=/; HttpOnly; SameSite=Strict%s; Max-Age=%d",
		service.cookieName, sessionToken, secure, int(AdminSessionLifetime/time.Second))
	return &LoginResult{Principal: principal, CSRFToken: csrfToken, Cookie: cookie}, nil
}

func (service *AuthService) Logout(ctx context.Context, principal *Principal) (string, error) {
	if principal != nil && principal.Kind == "admin" && principal.SessionID != "" {
		if _, err := service.db.Exec(ctx,
			`UPDATE mira_admin_sessions SET revoked_at = NOW() WHERE session_id = $1::uuid`, principal.SessionID,
		); err != nil {
			return "", fmt.Errorf("revoke administrator session: %w", err)
		}
	}
	secure := ""
	if service.secureCookies {
		secure = "; Secure"
	}
	return fmt.Sprintf("%s=; Path=/; HttpOnly; SameSite=Strict%s; Max-Age=0", service.cookieName, secure), nil
}

type AuditEvent struct {
	Action       string
	Principal    *Principal
	TargetNodeID string
	ThreadID     string
	RequestID    string
	Request      *http.Request
	// Failed is false by default so ordinary audit calls preserve the legacy
	// appendAudit success=true default without requiring boilerplate.
	Failed    bool
	ErrorCode string
	Metadata  any
}

func AppendAudit(ctx context.Context, db DBTX, event AuditEvent, trustProxyHeaders bool) error {
	metadata := event.Metadata
	if metadata == nil {
		metadata = map[string]any{}
	}
	encodedMetadata, err := json.Marshal(metadata)
	if err != nil {
		return fmt.Errorf("serialize audit metadata: %w", err)
	}
	var actorType, actorAdminID, actorNodeID, clientType any
	if event.Principal != nil {
		actorType = nullable(event.Principal.Kind)
		clientType = nullable(event.Principal.ClientType)
		if event.Principal.Kind == "admin" {
			actorAdminID = nullable(event.Principal.SubjectID)
		}
		if event.Principal.Kind == "node" {
			actorNodeID = nullable(event.Principal.NodeID)
		}
	}
	var address any
	if event.Request != nil {
		address = nullable(RequestAddress(event.Request, trustProxyHeaders))
	}
	_, err = db.Exec(ctx,
		`INSERT INTO mira_audit_events (
		   action, actor_type, actor_admin_id, actor_node_id, client_type,
		   target_node_id, thread_id, request_id, success, error_code,
		   request_address, metadata
		 ) VALUES ($1, $2, $3::uuid, $4::uuid, $5, $6::uuid, $7, $8, $9, $10, $11, $12::jsonb)`,
		event.Action, actorType, actorAdminID, actorNodeID, clientType,
		nullable(event.TargetNodeID), nullable(event.ThreadID), nullable(event.RequestID),
		!event.Failed, nullable(event.ErrorCode), address, string(encodedMetadata),
	)
	if err != nil {
		return fmt.Errorf("append audit event: %w", err)
	}
	return nil
}

func bearerToken(request *http.Request) (string, bool) {
	value := request.Header.Get("Authorization")
	if !strings.HasPrefix(value, "Bearer ") {
		return "", false
	}
	token := strings.TrimPrefix(value, "Bearer ")
	if token == "" || strings.IndexFunc(token, func(character rune) bool {
		return character == ' ' || character == '\t' || character == '\n' || character == '\r'
	}) >= 0 {
		return "", false
	}
	return token, true
}

func validClientType(clientType string) bool {
	_, ok := clientTypes[clientType]
	return ok
}

func normalizedClientType(clientType string) string {
	if validClientType(clientType) {
		return clientType
	}
	return "cli"
}

func constantTimeHashEqual(left, right string) bool {
	leftBytes, leftErr := hex.DecodeString(left)
	rightBytes, rightErr := hex.DecodeString(right)
	if leftErr != nil || rightErr != nil || len(leftBytes) != 32 || len(rightBytes) != 32 {
		return false
	}
	return subtle.ConstantTimeCompare(leftBytes, rightBytes) == 1
}

func (service *AuthService) loginFailureKeys(request *http.Request, username string) [2]string {
	address := RequestAddress(request, service.trustProxyHeaders)
	if address == "" {
		address = "unknown"
	}
	return [2]string{"address:" + address, "login:" + address + "\n" + strings.ToLower(username)}
}

func (service *AuthService) loginDelay(request *http.Request, username string) time.Duration {
	service.loginFailuresMu.Lock()
	defer service.loginFailuresMu.Unlock()
	now := service.now()
	var delay time.Duration
	for _, key := range service.loginFailureKeys(request, username) {
		entry, ok := service.loginFailures[key]
		if !ok || !entry.resetAt.After(now) {
			delete(service.loginFailures, key)
			continue
		}
		exponent := entry.count
		if exponent > 7 {
			exponent = 7
		}
		candidate := 250 * time.Millisecond * time.Duration(1<<exponent)
		if candidate > 30*time.Second {
			candidate = 30 * time.Second
		}
		if candidate > delay {
			delay = candidate
		}
	}
	return delay
}

func (service *AuthService) recordLoginFailure(request *http.Request, username string) {
	service.loginFailuresMu.Lock()
	defer service.loginFailuresMu.Unlock()
	if len(service.loginFailures) > 10_000 {
		clear(service.loginFailures)
	}
	now := service.now()
	for _, key := range service.loginFailureKeys(request, username) {
		previous := service.loginFailures[key]
		count := 1
		if previous.resetAt.After(now) {
			count = previous.count + 1
		}
		service.loginFailures[key] = loginFailure{count: count, resetAt: now.Add(15 * time.Minute)}
	}
}

func (service *AuthService) clearLoginFailures(request *http.Request, username string) {
	service.loginFailuresMu.Lock()
	defer service.loginFailuresMu.Unlock()
	for _, key := range service.loginFailureKeys(request, username) {
		delete(service.loginFailures, key)
	}
}

func nullable(value string) any {
	if value == "" {
		return nil
	}
	return value
}
