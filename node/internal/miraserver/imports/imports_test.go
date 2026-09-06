package imports

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/ssine/mira/node/internal/miraserver/channel"
	"github.com/ssine/mira/node/internal/miraserver/foundation"
)

const (
	childID   = "11111111-1111-4111-8111-111111111111"
	parentID  = "22222222-2222-4222-8222-222222222222"
	nodeID    = "33333333-3333-4333-8333-333333333333"
	runtimeID = "44444444-4444-4444-8444-444444444444"
)

type testSource struct {
	path       string
	threadID   string
	modifiedAt string
	codex      string
	content    []byte
}

func (source testSource) summary() map[string]any {
	return map[string]any{
		"path": source.path, "threadId": source.threadID, "modifiedAt": source.modifiedAt,
		"codexVersion": source.codex, "sizeBytes": int64(len(source.content)), "startedAt": "2026-01-02T03:04:05Z",
	}
}

type testCapability struct {
	sources map[string]testSource
	listed  []map[string]any
	resolve map[string]string
}

func (capability *testCapability) Invoke(
	_ context.Context,
	_ *foundation.Principal,
	_ string,
	_ string,
	params map[string]any,
	_ channel.InvokeContext,
) (any, error) {
	switch params["action"] {
	case "list":
		sessions := make([]any, len(capability.listed))
		for index := range capability.listed {
			sessions[index] = capability.listed[index]
		}
		return map[string]any{"sessions": sessions, "unknownScanField": map[string]any{"kept": true}}, nil
	case "resolve":
		path := capability.resolve[stringValue(params["rolloutId"])]
		if path == "" {
			return nil, nil
		}
		return capability.sources[path].summary(), nil
	case "read":
		source, found := capability.sources[stringValue(params["path"])]
		if !found {
			return nil, errors.New("missing source")
		}
		cursor, _ := integer(params["cursor"])
		limit, _ := integer(params["limit"])
		end := cursor + limit
		if end > int64(len(source.content)) {
			end = int64(len(source.content))
		}
		content := source.content[cursor:end]
		return map[string]any{
			"cursor": cursor, "nextCursor": end, "sizeBytes": int64(len(source.content)),
			"content": base64.StdEncoding.EncodeToString(content), "encoding": "base64",
			"eof": end == int64(len(source.content)), "modifiedAt": source.modifiedAt,
		}, nil
	default:
		return nil, errors.New("unknown action")
	}
}

type testImport struct {
	id      string
	input   TransferInput
	final   TransferFinal
	records []RawRecord
}

type testRepository struct {
	nodes     []NodeRuntime
	previous  map[string]PreviousImport
	valid     map[string]bool
	imports   map[string]*testImport
	bySource  map[string]string
	nextID    int
	marks     []string
	bindings  []string
	rollbacks int
	imported  []ThreadKey
	first     map[string]json.RawMessage
}

func newTestRepository() *testRepository {
	return &testRepository{
		previous: map[string]PreviousImport{}, valid: map[string]bool{}, imports: map[string]*testImport{},
		bySource: map[string]string{}, first: map[string]json.RawMessage{},
	}
}

func (repository *testRepository) ApprovedNodes(context.Context) ([]NodeRuntime, error) {
	return repository.nodes, nil
}

func (repository *testRepository) PreviousImports(_ context.Context, _ string, storeID string, paths []string) (map[string]PreviousImport, error) {
	result := map[string]PreviousImport{}
	for _, path := range paths {
		if value, found := repository.previous[path]; found && value.StoreID == storeID {
			result[path] = value
		}
	}
	return result, nil
}

func (repository *testRepository) ValidRuntime(_ context.Context, id string) (string, bool, error) {
	return id, repository.valid[id], nil
}

func (repository *testRepository) BeginTransfer(_ context.Context, input TransferInput) (TransferTransaction, error) {
	return &testTransaction{repository: repository, imported: &testImport{input: input}}, nil
}

func (repository *testRepository) MarkImport(_ context.Context, id, status string, _ *int64, errorCode *string) error {
	value := id + ":" + status
	if errorCode != nil {
		value += ":" + *errorCode
	}
	repository.marks = append(repository.marks, value)
	return nil
}

func (repository *testRepository) BindRuntime(_ context.Context, storeID, threadID, id string) error {
	repository.bindings = append(repository.bindings, storeID+":"+threadID+":"+id)
	return nil
}

func (repository *testRepository) ImportedThreadKeys(context.Context) ([]ThreadKey, error) {
	return repository.imported, nil
}

func (repository *testRepository) FirstHistoryItem(_ context.Context, storeID, threadID string, _, _ int64) (json.RawMessage, bool, error) {
	value, found := repository.first[storeID+":"+threadID]
	return value, found, nil
}

type testTransaction struct {
	repository *testRepository
	imported   *testImport
	closed     bool
}

func (transaction *testTransaction) Add(_ context.Context, records []RawRecord) error {
	for _, record := range records {
		copy := record
		copy.Raw = append(json.RawMessage(nil), record.Raw...)
		transaction.imported.records = append(transaction.imported.records, copy)
	}
	return nil
}

func (transaction *testTransaction) Finish(_ context.Context, final TransferFinal) (string, bool, error) {
	transaction.closed = true
	key := transaction.imported.input.StoreID + "\x00" + transaction.imported.input.NodeID + "\x00" + transaction.imported.input.Path + "\x00" + final.SHA256
	if id := transaction.repository.bySource[key]; id != "" {
		return id, true, nil
	}
	transaction.repository.nextID++
	id := "import-" + string(rune('0'+transaction.repository.nextID))
	transaction.imported.id = id
	transaction.imported.final = final
	transaction.repository.imports[id] = transaction.imported
	transaction.repository.bySource[key] = id
	return id, false, nil
}

func (transaction *testTransaction) Rollback(context.Context) error {
	if !transaction.closed {
		transaction.repository.rollbacks++
		transaction.closed = true
	}
	return nil
}

type testThreadStore struct {
	repository *testRepository
	canonical  []json.RawMessage
	commits    int
	failCommit error
	head       Head
	history    map[string][]json.RawMessage
	deltas     []Delta
	headers    []http.Header
}

func (store *testThreadStore) AssertNotDeleted(context.Context, string, []string) error { return nil }

func (store *testThreadStore) CommitImported(_ context.Context, _ string, commit ImportCommit, _ Context) (CommitResult, error) {
	if store.failCommit != nil {
		return CommitResult{}, store.failCommit
	}
	items := []json.RawMessage{}
	for _, segment := range commit.Segments {
		source := store.repository.imports[segment.ImportID]
		if source == nil {
			return CommitResult{}, errors.New("missing staged import")
		}
		from := int(segment.FirstLine - 1)
		to := from + int(segment.Count)
		for _, record := range source.records[from:to] {
			item, err := commit.Normalize(record.Raw)
			if err != nil {
				return CommitResult{}, err
			}
			items = append(items, item)
		}
	}
	noChange := rawSlicesEqual(store.canonical, items)
	store.canonical = items
	store.commits++
	if commit.RuntimeNodeID != nil {
		store.repository.bindings = append(store.repository.bindings, commit.ThreadID+":"+*commit.RuntimeNodeID)
	}
	return CommitResult{Version: int64(store.commits), NoChange: noChange}, nil
}

func (store *testThreadStore) Head(context.Context, string) (Head, error) { return store.head, nil }

func (store *testThreadStore) History(_ context.Context, _, threadID string, _, _ int64) ([]json.RawMessage, error) {
	return store.history[threadID], nil
}

func (store *testThreadStore) CommitDelta(_ context.Context, _ string, delta Delta, headers http.Header) (CommitResult, error) {
	store.deltas = append(store.deltas, delta)
	store.headers = append(store.headers, headers.Clone())
	return CommitResult{Version: delta.ExpectedVersion + 1}, nil
}

func rawSlicesEqual(left, right []json.RawMessage) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		equal, _ := sameRawJSON(left[index], right[index])
		if !equal {
			return false
		}
	}
	return true
}

func TestImportCodexSessionLineagePreservesRawAndIsIdempotent(t *testing.T) {
	parentMetaPrefix := `{"type":"session_meta","ordinal":0,"payload":{"id":"` + parentID + `","history_mode":"paginated"}}` + "\n"
	parentBody := `{"ordinal":1,"futureEnvelope":true,"type":"response_item","payload":{"text":"ancestor"}}` + "\n"
	parentContent := []byte(parentMetaPrefix + parentBody)
	childMeta := `{"topUnknown":7,"type":"session_meta","payload":{"id":"` + childID + `","timestamp":"2026-01-02T03:04:05Z","cwd":"/work","base_instructions":"keep\\u0020raw","history_mode":"paginated","future":{"kept":true},"source":{"subagent":{"thread_spawn":{"parent_thread_id":"` + parentID + `"}}},"history_base":{"thread_id":"` + parentID + `","end_ordinal_exclusive":2,"end_byte_offset":` + integerText(int64(len(parentContent))) + `},"forked_from_ordinal_exclusive":9,"subagent_history_start_ordinal":4}}` + "\n"
	childBody := `{"type":"response_item","payload":{"text":"child","unknown":"\\ud800","nul":"a\\u0000b"}}` + "\n"
	childContent := []byte(childMeta + childBody)
	modified := "2026-01-03T04:05:06.000Z"
	parent := testSource{path: "/sessions/parent.jsonl", threadID: parentID, modifiedAt: modified, codex: "1.2.3", content: parentContent}
	child := testSource{path: "/sessions/child.jsonl", threadID: childID, modifiedAt: modified, codex: "1.2.3", content: childContent}
	childSummary := child.summary()
	childSummary["cwd"] = "/work"
	childSummary["title"] = "Imported thread"
	repository := newTestRepository()
	repository.nodes = []NodeRuntime{
		{NodeID: nodeID, Hostname: "Laptop", Platform: "win32", NodeMode: "windows"},
		{NodeID: runtimeID, Hostname: "laptop", Platform: "linux", NodeMode: "wsl", Capabilities: map[string]any{"appServer": true}},
	}
	repository.valid[runtimeID] = true
	capability := &testCapability{
		sources: map[string]testSource{child.path: child, parent.path: parent},
		listed:  []map[string]any{childSummary}, resolve: map[string]string{parentID: parent.path},
	}
	threadStore := &testThreadStore{repository: repository, history: map[string][]json.RawMessage{}}
	service := New(repository, capability, threadStore)
	audits := 0
	service.Audit = func(_ context.Context, event foundation.AuditEvent) error {
		audits++
		if event.Action != "codex_session.imported" || event.ThreadID != childID {
			t.Fatalf("unexpected audit: %#v", event)
		}
		return nil
	}

	for attempt := 0; attempt < 2; attempt++ {
		result, err := service.ImportCodexSession(context.Background(), Request{
			NodeID: nodeID, Body: map[string]any{"path": child.path}, Progress: Context{},
		})
		if err != nil {
			t.Fatalf("attempt %d: %v", attempt+1, err)
		}
		if result.Status != http.StatusOK {
			t.Fatalf("unexpected result: %#v", result)
		}
		body := result.Body.(map[string]any)
		if got := body["itemCount"]; got != int64(3) {
			t.Fatalf("itemCount = %#v", got)
		}
		if got := body["ancestorCount"]; got != 1 {
			t.Fatalf("ancestorCount = %#v", got)
		}
		if got := body["runtimeNodeId"]; got != runtimeID {
			t.Fatalf("runtimeNodeId = %#v", got)
		}
		if got := body["duplicate"]; got != (attempt == 1) {
			t.Fatalf("duplicate attempt %d = %#v", attempt+1, got)
		}
	}
	if len(repository.imports) != 2 { // child and ancestor; retry reuses both.
		t.Fatalf("immutable imports = %d, want 2", len(repository.imports))
	}
	childImport := findImportByPath(repository, child.path)
	if childImport == nil || string(childImport.records[1].Raw) != strings.TrimSuffix(childBody, "\n") {
		t.Fatalf("raw child provenance changed: %#v", childImport)
	}
	parentImport := findImportByPath(repository, parent.path)
	if parentImport == nil || !strings.Contains(string(parentImport.records[1].Raw), `"futureEnvelope":true`) {
		t.Fatalf("raw ancestor provenance changed: %#v", parentImport)
	}
	if len(threadStore.canonical) != 3 {
		t.Fatalf("canonical history length = %d", len(threadStore.canonical))
	}
	first, err := decodeObject(threadStore.canonical[0])
	if err != nil {
		t.Fatal(err)
	}
	payload := object(first["payload"])
	if stringValue(payload["history_mode"]) != "legacy" || payload["history_base"] != nil || payload["forked_from_ordinal_exclusive"] != nil {
		t.Fatalf("child meta was not canonicalized: %s", threadStore.canonical[0])
	}
	if !reflect.DeepEqual(object(payload["future"]), map[string]any{"kept": true}) {
		t.Fatalf("unknown payload was lost: %s", threadStore.canonical[0])
	}
	if got := string(threadStore.canonical[1]); !strings.Contains(got, `"ancestor"`) {
		t.Fatalf("ancestor ordering/content mismatch: %s", got)
	}
	if got := string(threadStore.canonical[2]); !strings.Contains(got, `"\\ud800"`) || !strings.Contains(got, `"a\\u0000b"`) {
		t.Fatalf("unknown JSON escapes were not retained: %s", got)
	}
	if audits != 2 || len(repository.bindings) != 2 {
		t.Fatalf("audits=%d bindings=%v", audits, repository.bindings)
	}
}

func TestStageSessionTransferInvalidUTF8RollsBack(t *testing.T) {
	content := append([]byte(`{"type":"session_meta","payload":{"id":"`+childID+`"}}`+"\n"), 0xff, '\n')
	source := testSource{path: "/bad.jsonl", threadID: childID, modifiedAt: "2026-01-03T04:05:06Z", content: content}
	repository := newTestRepository()
	service := New(repository, &testCapability{sources: map[string]testSource{source.path: source}}, nil)
	_, err := service.StageSessionTransfer(context.Background(), nil, nodeID, source.summary(), DefaultStoreID, nil, nil, Context{})
	if err == nil || errorCode(err) != "invalid_session_jsonl" {
		t.Fatalf("error = %v", err)
	}
	if repository.rollbacks != 1 || len(repository.imports) != 0 {
		t.Fatalf("rollbacks=%d imports=%d", repository.rollbacks, len(repository.imports))
	}
}

func TestImportCodexSessionMarksDivergedStageFailed(t *testing.T) {
	content := []byte(`{"type":"session_meta","payload":{"id":"` + childID + `"}}` + "\n")
	source := testSource{path: "/diverged.jsonl", threadID: childID, modifiedAt: "2026-01-03T04:05:06Z", content: content}
	repository := newTestRepository()
	capability := &testCapability{sources: map[string]testSource{source.path: source}, listed: []map[string]any{source.summary()}}
	store := &testThreadStore{repository: repository, history: map[string][]json.RawMessage{}, failCommit: importError(409, "history_diverged", "diverged")}
	service := New(repository, capability, store)
	_, err := service.ImportCodexSession(context.Background(), Request{NodeID: nodeID, Body: map[string]any{"path": source.path, "storeId": nil}})
	var failure *foundation.HTTPError
	if !errors.As(err, &failure) || failure.Status != 409 || failure.Code != "history_diverged" {
		t.Fatalf("error = %#v", err)
	}
	if !reflect.DeepEqual(repository.marks, []string{"import-1:failed:history_diverged"}) {
		t.Fatalf("marks = %v", repository.marks)
	}
}

func TestImportCodexSessionDoesNotReportCommittedImportAsFailedWhenAuditFails(t *testing.T) {
	content := []byte(`{"type":"session_meta","payload":{"id":"` + childID + `"}}` + "\n")
	source := testSource{path: "/audit-outage.jsonl", threadID: childID, modifiedAt: "2026-01-03T04:05:06Z", content: content}
	repository := newTestRepository()
	capability := &testCapability{sources: map[string]testSource{source.path: source}, listed: []map[string]any{source.summary()}}
	store := &testThreadStore{repository: repository, history: map[string][]json.RawMessage{}}
	service := New(repository, capability, store)
	service.Audit = func(context.Context, foundation.AuditEvent) error { return errors.New("audit unavailable") }
	result, err := service.ImportCodexSession(context.Background(), Request{NodeID: nodeID, Body: map[string]any{"path": source.path}})
	if err != nil || result.Status != http.StatusOK || store.commits != 1 {
		t.Fatalf("committed import was reported as failed: result=%#v err=%v commits=%d", result, err, store.commits)
	}
	if len(repository.marks) != 0 {
		t.Fatalf("committed import was marked failed: %v", repository.marks)
	}
}

func TestNormalizeImportedThreadHistoryModes(t *testing.T) {
	meta := json.RawMessage(`{"type":"session_meta","payload":{"id":"` + childID + `","base_instructions":"old","history_mode":"paginated","unknown":17}}`)
	body := json.RawMessage(`{"type":"response_item","payload":{"text":"same"},"unknownEnvelope":true}`)
	repository := newTestRepository()
	repository.imported = []ThreadKey{{StoreID: DefaultStoreID, ThreadID: childID}}
	repository.first[DefaultStoreID+":"+childID] = meta
	store := &testThreadStore{
		repository: repository,
		head: Head{Version: 7, State: map[string]any{"created_threads": map[string]any{childID: map[string]any{"history_mode": "paginated"}}},
			HistoryManifest: map[string]HistoryEntry{childID: {Generation: 2, ItemCount: 2}}},
		history: map[string][]json.RawMessage{childID: {meta, body}},
	}
	service := New(repository, nil, store)
	count, err := service.NormalizeImportedThreadHistoryModes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 || len(store.deltas) != 1 {
		t.Fatalf("count=%d deltas=%d", count, len(store.deltas))
	}
	delta := store.deltas[0]
	if delta.ExpectedVersion != 7 || len(delta.StateChanges) != 1 || len(delta.HistoryChanges) != 1 {
		t.Fatalf("unexpected delta: %#v", delta)
	}
	items := delta.HistoryChanges[0]["items"].([]json.RawMessage)
	if rolloutHistoryMode(items[0]) != "legacy" || !strings.Contains(string(items[0]), `"unknown":17`) {
		t.Fatalf("metadata normalization lost fields: %s", items[0])
	}
	if string(items[1]) != string(body) {
		t.Fatalf("non-meta item changed: %s", items[1])
	}
	if store.headers[0].Get("x-codex-version") != "mira-import-compatibility" || !validUUID(store.headers[0].Get("x-codex-operation-id")) {
		t.Fatalf("invalid compatibility headers: %v", store.headers[0])
	}
}

func TestScanCodexSessionsPreservesUnknownFieldsAndAddsHints(t *testing.T) {
	modified, _ := time.Parse(time.RFC3339Nano, "2026-01-03T04:05:06Z")
	summary := map[string]any{"path": "/x", "threadId": childID, "sizeBytes": int64(12), "modifiedAt": "2026-01-03T04:05:06Z", "cwd": "/home/x", "newField": "kept"}
	repository := newTestRepository()
	repository.nodes = []NodeRuntime{{NodeID: nodeID, Hostname: "Host", NodeMode: "windows"}, {NodeID: runtimeID, Hostname: "host", NodeMode: "wsl", Capabilities: map[string]any{"appServer": true}}}
	repository.previous["/x"] = PreviousImport{ImportID: "i", SourcePath: "/x", ThreadID: childID, StoreID: DefaultStoreID, SourceSizeBytes: 12, SourceModifiedAt: &modified, CreatedAt: modified}
	service := New(repository, &testCapability{listed: []map[string]any{summary}}, nil)
	result, err := service.ScanCodexSessions(context.Background(), nil, nodeID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !object(result["unknownScanField"])["kept"].(bool) {
		t.Fatal("unknown scan result field was lost")
	}
	session := array(result["sessions"])[0].(map[string]any)
	if session["newField"] != "kept" || session["executionMode"] != "wsl" || session["suggestedRuntimeNodeId"] != runtimeID {
		t.Fatalf("unexpected hints: %#v", session)
	}
	if object(session["import"])["unchanged"] != true {
		t.Fatalf("unchanged import not detected: %#v", session["import"])
	}
}

func findImportByPath(repository *testRepository, path string) *testImport {
	ids := make([]string, 0, len(repository.imports))
	for id := range repository.imports {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if repository.imports[id].input.Path == path {
			return repository.imports[id]
		}
	}
	return nil
}

func integerText(value int64) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}
