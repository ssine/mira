package foundation

import (
	"context"
	"encoding/base64"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestNodeAuthenticationReadOnlyAndRevocation(t *testing.T) {
	endpoint := os.Getenv("MIRA_TEST_DATABASE_URL")
	if endpoint == "" {
		t.Skip("MIRA_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, endpoint)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := InitializeDatabase(ctx, pool); err != nil {
		t.Fatal(err)
	}
	const id = "10000000-0000-4000-8000-000000009901"
	secret := base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat("x", 32)))
	token := "mira_node_" + id + "_" + secret
	hash, _ := NodeSecretHash(secret)
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO codex_nodes(node_id,node_key,hostname,platform,architecture,node_mode,node_version,capabilities,codex_installations,approval_status)
 VALUES($1::uuid,'auth-read-only-fixture','fixture','linux','amd64','linux','test','{}','[]','approved')`, id)
	defer func() { _, _ = pool.Exec(ctx, `DELETE FROM codex_nodes WHERE node_id=$1::uuid`, id) }()
	exec(`INSERT INTO mira_node_credentials(credential_id,node_id,secret_hash) VALUES($1::uuid,$1::uuid,$2)`, id, hash)

	config := pool.Config()
	config.ConnConfig.RuntimeParams["default_transaction_read_only"] = "on"
	readPool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer readPool.Close()
	service := NewAuthService(readPool, AuthOptions{})
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			principal, err := service.AuthenticateNodeToken(ctx, token, "codex")
			if err != nil || !service.Permits(principal, "node") || principal.NodeID != id {
				t.Errorf("read-only authentication: principal=%v err=%v", principal, err)
			}
		}()
	}
	wg.Wait()
	if principal, err := service.AuthenticateNodeToken(ctx, token[:len(token)-43]+base64.RawURLEncoding.EncodeToString(make([]byte, 32)), "node"); err != nil || principal != nil {
		t.Fatalf("wrong secret accepted: %v %v", principal, err)
	}
	for _, query := range []string{
		`UPDATE mira_node_credentials SET revoked_at=now() WHERE credential_id=$1::uuid`,
		`UPDATE mira_node_credentials SET revoked_at=NULL WHERE credential_id=$1::uuid`,
		`UPDATE codex_nodes SET approval_status='revoked' WHERE node_id=$1::uuid`,
	} {
		exec(query, id)
		principal, err := service.AuthenticateNodeToken(ctx, token, "node")
		wantPermitted := strings.Contains(query, "revoked_at=NULL")
		if err != nil || service.Permits(principal, "node") != wantPermitted {
			t.Fatalf("authorization did not reflect committed revocation/approval: %v %v", principal, err)
		}
	}
}
