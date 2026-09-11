package miraserver

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ssine/mira/node/internal/miraserver/accountsampler"
	"github.com/ssine/mira/node/internal/miraserver/channel"
	"github.com/ssine/mira/node/internal/miraserver/nodes"
)

type quotaFixture struct {
	node   nodes.Node
	values map[string]float64
	calls  int
}

func (f *quotaFixture) IsConnected(id string) bool { return id == f.node.NodeID }
func (f *quotaFixture) List(context.Context, bool) ([]nodes.Node, error) {
	return []nodes.Node{f.node}, nil
}
func (f *quotaFixture) Get(context.Context, string, bool) (*nodes.Node, error) { return &f.node, nil }
func (f *quotaFixture) Read(context.Context, string) (channel.AccountSnapshot, error) {
	return channel.AccountSnapshot{}, errors.New("account selector required")
}
func (f *quotaFixture) ReadAccount(_ context.Context, node, binding, runtime string) (channel.AccountSnapshot, error) {
	used, ok := f.values[binding]
	if !ok || node != f.node.NodeID || runtime == "" {
		return channel.AccountSnapshot{}, errors.New("invalid scoped quota read")
	}
	f.calls++
	return channel.AccountSnapshot{Account: map[string]any{"type": "chatgpt", "email": "synthetic@example.test", "accessToken": "must-not-persist"}, Limits: map[string]any{"rateLimits": map[string]any{"primary": map[string]any{"usedPercent": used, "windowDurationMins": 10080, "resetsAt": 2000000000}}}}, nil
}

func TestAccountQuotaSlotsAndIdentityIsolation(t *testing.T) {
	pool := accountTestDatabase(t)
	ctx := context.Background()
	uuid := func() string {
		id, err := randomUUID()
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	nodeID, a, b := uuid(), uuid(), uuid()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO codex_nodes(node_id,node_key,hostname,platform,architecture,node_mode,node_version,capabilities,codex_installations)
 VALUES($1::uuid,$1::text,'fixture','linux','amd64','linux','test','{"appServer":true,"codexAccountsV1":true}','[]')`, nodeID)
	f := &quotaFixture{node: nodes.Node{NodeID: nodeID, ApprovalStatus: "approved", Status: "online", Capabilities: map[string]any{"appServer": true, "codexAccountsV1": true}}, values: map[string]float64{a: 12, b: 67}}
	for _, id := range []string{a, b} {
		exec(`INSERT INTO mira_codex_accounts(account_id,name) VALUES($1::uuid,'fixture')`, id)
		exec(`INSERT INTO mira_node_codex_accounts(node_account_id,node_id,account_id) VALUES($1::uuid,$2::uuid,$1::uuid)`, id, nodeID)
		f.node.CodexAccounts = append(f.node.CodexAccounts, nodes.CodexAccount{NodeAccountID: id, AccountID: id, Enabled: true, Revision: 1, CredentialRevision: 1, Reported: map[string]any{"status": "running", "runtimeId": "same-runtime", "codexHome": "/fixture", "codexPath": "/fixture/codex"}})
	}
	now := time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC)
	sampler := accountsampler.New(pool, f, f, f, accountsampler.Options{Now: func() time.Time { return now }})
	sample := func(index int, want bool) {
		t.Helper()
		ok, err := sampler.SampleAccount(ctx, nodes.AccountNode(&f.node, f.node.CodexAccounts[index]))
		if err != nil || ok != want {
			t.Fatalf("sample: %v %v", ok, err)
		}
	}
	sample(0, true)
	sample(1, true)
	sample(0, false)
	sample(1, false)
	if f.calls != 2 {
		t.Fatalf("duplicate quota reads: %d", f.calls)
	}
	var aIdentity, bIdentity string
	var aUsed, bUsed float64
	var leaked bool
	err := pool.QueryRow(ctx, `SELECT a.identity_key,b.identity_key,(a.limits#>>'{rateLimits,primary,usedPercent}')::float8,
 (b.limits#>>'{rateLimits,primary,usedPercent}')::float8,(a.account ? 'accessToken' OR b.account ? 'accessToken')
 FROM mira_codex_account_samples a CROSS JOIN mira_codex_account_samples b WHERE a.node_account_id=$1::uuid AND b.node_account_id=$2::uuid`, a, b).Scan(&aIdentity, &bIdentity, &aUsed, &bUsed, &leaked)
	if err != nil {
		t.Fatal(err)
	}
	if aIdentity == bIdentity || aUsed != 12 || bUsed != 67 || leaked {
		t.Fatalf("quota or identity isolation failed: %v %v %v", aUsed, bUsed, leaked)
	}
	now = now.Add(accountsampler.SampleInterval)
	f.node.CodexAccounts[0].Reported["runtimeId"] = "restarted-runtime"
	sample(0, true)
	var identity string
	err = pool.QueryRow(ctx, `SELECT identity_key FROM mira_codex_account_samples WHERE node_account_id=$1::uuid ORDER BY sample_slot DESC LIMIT 1`, a).Scan(&identity)
	if err != nil || identity != aIdentity {
		t.Fatalf("restart split identity: %v", err)
	}
	now = now.Add(accountsampler.SampleInterval)
	f.node.CodexAccounts[0].CredentialRevision++
	exec(`UPDATE mira_node_codex_accounts SET credential_revision=2 WHERE node_account_id=$1::uuid`, a)
	sample(0, true)
	err = pool.QueryRow(ctx, `SELECT identity_key FROM mira_codex_account_samples WHERE node_account_id=$1::uuid ORDER BY sample_slot DESC LIMIT 1`, a).Scan(&identity)
	if err != nil || identity == aIdentity {
		t.Fatalf("credential replacement reused identity: %v", err)
	}
}
