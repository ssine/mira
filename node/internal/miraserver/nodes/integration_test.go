package nodes

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/ssine/mira/node/internal/miraserver/foundation"
)

func TestPostgresLifecycle(t *testing.T) {
	databaseURL := os.Getenv("MIRA_NODES_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set MIRA_NODES_TEST_DATABASE_URL to run the PostgreSQL Node lifecycle")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := foundation.InitializeDatabase(ctx, pool); err != nil {
		t.Fatal(err)
	}
	var adminID string
	if err := pool.QueryRow(ctx, `INSERT INTO mira_admin_users(username,password_hash)
		VALUES('nodes-test-admin','unused') RETURNING admin_user_id::text`).Scan(&adminID); err != nil {
		t.Fatal(err)
	}
	principal := &Principal{Kind: "admin", ClientType: "admin", SubjectID: adminID}
	service := New(pool, Options{RandomCode: func() (string, error) { return "123456", nil }, Now: func() int64 { return 42 }})

	credentialID := "12345678-1234-4123-8123-123456789abc"
	secret := []byte("01234567890123456789012345678901")
	secretDigest := sha256.Sum256(secret)
	descriptor := enrollmentBody(credentialID, hex.EncodeToString(secretDigest[:]), "integration-node")
	request := httptest.NewRequest(http.MethodPost, "/v1/node-enrollments", nil)
	request.RemoteAddr = "192.0.2.10:1234"
	created, err := service.CreateEnrollment(ctx, request, descriptor)
	if err != nil || created.Status != 202 {
		t.Fatalf("create enrollment = %#v, %v", created, err)
	}
	createdBody := created.Body.(map[string]any)
	enrollmentID := createdBody["enrollmentId"].(string)
	if createdBody["verificationCode"] != "123456" {
		t.Fatalf("enrollment = %#v", createdBody)
	}
	authenticated := httptest.NewRequest(http.MethodGet, "/v1/node-enrollments/"+enrollmentID, nil)
	authenticated.Header.Set("Authorization", "Bearer mira_node_"+credentialID+"_"+base64.RawURLEncoding.EncodeToString(secret))
	read, err := service.GetEnrollment(ctx, authenticated, enrollmentID)
	if err != nil || read.Status != 200 {
		t.Fatalf("get enrollment = %#v, %v", read, err)
	}
	approved, err := service.ApproveEnrollment(ctx, request, principal, enrollmentID, map[string]any{"note": "approved"})
	if err != nil || approved.Status != 200 {
		t.Fatalf("approve enrollment = %#v, %v", approved, err)
	}
	nodeID := approved.Body.(map[string]any)["nodeId"].(string)

	registered, err := service.RegisterNode(ctx, nodeID, map[string]any{
		"nodeKey": "integration-node", "hostname": "host", "platform": "linux", "architecture": "amd64",
		"nodeMode": "linux", "nodeVersion": "test", "nodeBuild": map[string]any{"version": "test"},
		"capabilities": map[string]any{"files": true, "appServer": true}, "codexInstallations": []any{},
	})
	if err != nil || registered.Status != 200 {
		t.Fatalf("register Node = %#v, %v", registered, err)
	}
	metadata, err := service.SetNodeMetadata(ctx, request, principal, nodeID, map[string]any{
		"displayName": "Integration Node", "aliases": []any{"build-box"},
		"labels": map[string]any{"role": "test"}, "expectedRevision": float64(0),
	})
	if err != nil || metadata.Status != 200 {
		t.Fatalf("set metadata = %#v, %v", metadata, err)
	}
	resolved, err := service.ResolveNode(ctx, "BUILD-BOX", false)
	if err != nil || resolved.Status != 200 || resolved.Body.(map[string]any)["matchedBy"] != "alias" {
		t.Fatalf("resolve Node = %#v, %v", resolved, err)
	}
	desired, err := service.SetDesiredAppServer(ctx, nodeID, map[string]any{
		"running": true, "defaultCwd": "/srv/work", "developerInstructionsFile": "/srv/work/AGENTS.md",
	})
	if err != nil || desired.Status != 200 || desired.Body.(map[string]any)["desiredAppServer"].(map[string]any)["revision"] == nil {
		t.Fatalf("set desired App Server = %#v, %v", desired, err)
	}
	if err := service.SetNodeChannelStatus(ctx, nodeID, map[string]any{"connected": true}); err != nil {
		t.Fatal(err)
	}
	node, err := service.GetNode(ctx, nodeID, false)
	if err != nil || node == nil || len(node.Aliases) != 1 || node.ChannelStatus["connected"] != true {
		t.Fatalf("get Node = %#v, %v", node, err)
	}
	revoked, err := service.RevokeNode(ctx, request, principal, nodeID, nil)
	if err != nil || revoked.Status != 200 {
		t.Fatalf("revoke Node = %#v, %v", revoked, err)
	}

	secondID := "22345678-1234-4123-8123-123456789abc"
	secondSecret := []byte("abcdefghijklmnopqrstuvwxyzABCDEF")
	secondDigest := sha256.Sum256(secondSecret)
	fresh, err := service.CreateEnrollment(ctx, request, enrollmentBody(secondID, hex.EncodeToString(secondDigest[:]), "integration-node"))
	if err != nil || fresh.Status != 202 {
		t.Fatalf("fresh enrollment = %#v, %v", fresh, err)
	}
	freshID := fresh.Body.(map[string]any)["enrollmentId"].(string)
	restored, err := service.RestoreNode(ctx, request, principal, nodeID, map[string]any{"enrollmentId": freshID})
	if err != nil || restored.Status != 200 || restored.Body.(map[string]any)["nodeId"] != nodeID {
		t.Fatalf("restore Node = %#v, %v", restored, err)
	}
	var activeCredentials int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*)::integer FROM mira_node_credentials WHERE node_id=$1::uuid AND revoked_at IS NULL`, nodeID).Scan(&activeCredentials); err != nil {
		t.Fatal(err)
	}
	if activeCredentials != 1 {
		t.Fatalf("active credentials = %d, want 1", activeCredentials)
	}
	var auditCount int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*)::integer FROM mira_audit_events WHERE target_node_id=$1::uuid OR metadata->>'nodeKey'=$2`, nodeID, "integration-node").Scan(&auditCount); err != nil {
		t.Fatal(err)
	}
	if auditCount < 6 {
		t.Fatalf("audit count = %d", auditCount)
	}
}

func enrollmentBody(credentialID, secretHash, nodeKey string) map[string]any {
	return map[string]any{
		"credentialId": credentialID, "credentialSecretHash": strings.ToLower(secretHash),
		"nodeKey": nodeKey, "hostname": "host", "platform": "linux", "architecture": "amd64",
		"nodeMode": "linux", "nodeVersion": "test", "nodeBuild": map[string]any{"version": "test"},
		"capabilities": map[string]any{"files": true}, "codexInstallations": []any{},
		"defaultDesiredAppServer": map[string]any{"running": false}, "machineStatus": map[string]any{},
	}
}
