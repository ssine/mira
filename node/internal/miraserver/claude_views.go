package miraserver

// Claude read projections share the Web conversation contract, never Codex storage.
import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

func claudeEstimate(amount *float64, partial bool) map[string]any {
	status := "complete"
	if partial {
		status = "partial"
	}
	if amount == nil {
		status = "unavailable"
	}
	return map[string]any{"amount": amount, "status": status, "currency": "USD", "basis": "claude_sdk", "includesSubagents": true,
		"note": "Claude SDK 价格估算，包含子 Agent；API 网关实际扣费可能不同。"}
}

func (server *Server) claudeUsage(ctx context.Context, id string) (map[string]any, error) {
	var amount *float64
	var input, output, cached, selfInput, selfOutput, selfCached *int64
	var partial, usagePartial bool
	err := server.pool.QueryRow(ctx, `SELECT sum(cost_delta)::float8,sum(input_delta)::bigint,sum(output_delta)::bigint,sum(cached_delta)::bigint,
 sum(self_input)::bigint,sum(self_output)::bigint,sum(self_cached)::bigint,coalesce(bool_or(cost_partial),false),coalesce(bool_or(usage_partial),false)
 FROM mira_claude_usage_deltas WHERE session_id=$1`, id).Scan(&amount, &input, &output, &cached, &selfInput, &selfOutput, &selfCached, &partial, &usagePartial)
	status := "complete"
	if usagePartial {
		status = "partial"
	}
	if input == nil || output == nil {
		status = "unavailable"
	}
	total := map[string]any{"inputTokens": input, "outputTokens": output, "cachedInputTokens": cached, "status": status}
	self := map[string]any{"inputTokens": selfInput, "outputTokens": selfOutput, "cachedInputTokens": selfCached}
	return map[string]any{"costEstimate": claudeEstimate(amount, partial), "tokenUsage": total, "tokenUsageSummary": map[string]any{"total": total, "self": self, "includesSubagents": true, "scope": "claude_session"}}, err
}

func (server *Server) claudeSummary(ctx context.Context, s claudeSession) (map[string]any, error) {
	result, err := server.claudeUsage(ctx, s.ID)
	if err != nil {
		return nil, err
	}
	result["threadId"] = s.ID
	result["sessionId"] = s.ID
	result["engine"] = "claude"
	result["title"] = s.Title
	result["name"] = s.Title
	result["cwd"] = s.Cwd
	result["model"] = s.Model
	result["reasoningEffort"] = s.Effort
	result["nodeAccountId"] = s.AccountID
	result["runtimeNodeId"] = s.NodeID
	result["sourceNodeId"] = s.NodeID
	result["archived"] = s.Archived
	result["generation"] = int64(1)
	result["updatedAt"] = s.UpdatedAt
	result["createdAt"] = s.CreatedAt
	result["persistence"] = s.Persistence
	result["listRoot"] = true
	var count, seq int64
	var lastTurn *string
	var status string
	err = server.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM mira_claude_transcripts WHERE session_id=$1 AND subpath<>''),
 coalesce((SELECT max(seq) FROM mira_claude_events WHERE session_id=$1),0),
 (SELECT turn_id::text FROM mira_claude_turns WHERE session_id=$1 ORDER BY revision DESC LIMIT 1),
 coalesce((SELECT status FROM mira_claude_turns WHERE session_id=$1 ORDER BY revision DESC LIMIT 1),'idle')`, s.ID).Scan(&count, &seq, &lastTurn, &status)
	if err != nil {
		return nil, err
	}
	result["tokenUsageSummary"].(map[string]any)["subagentCount"] = count
	result["costEstimate"].(map[string]any)["subagentCount"] = count
	result["childCount"] = count
	result["descendantCount"] = count
	result["itemCount"] = seq
	activity := "idle"
	if s.ActiveTurn != nil {
		activity = "running"
		lastTurn = s.ActiveTurn
	} else if status == "failed" || status == "interrupted" {
		activity = status
	}
	result["activity"] = map[string]any{"state": activity, "turnId": lastTurn, "generation": 1, "itemCount": seq}
	return result, nil
}

func (server *Server) claudeConversation(ctx context.Context, id string) (map[string]any, error) {
	s, err := scanClaude(server.pool.QueryRow(ctx, `SELECT `+claudeColumns+` FROM mira_claude_sessions WHERE session_id=$1`, id))
	if err == nil {
		return server.claudeSummary(ctx, s)
	}
	var sessionID, subpath string
	err = server.pool.QueryRow(ctx, `SELECT session_id::text,subpath FROM mira_claude_transcripts WHERE thread_id=$1`, id).Scan(&sessionID, &subpath)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, claudeError(404, "Claude conversation not found")
	}
	if err != nil {
		return nil, err
	}
	s, err = scanClaude(server.pool.QueryRow(ctx, `SELECT `+claudeColumns+` FROM mira_claude_sessions WHERE session_id=$1`, sessionID))
	if err != nil {
		return nil, err
	}
	value, err := server.claudeSummary(ctx, s)
	if err != nil {
		return nil, err
	}
	value["threadId"] = id
	value["subpath"] = subpath
	// Group native subpaths beneath their owning session without asserting native parentage.
	value["parentThreadId"] = sessionID
	value["nativeParentUnknown"] = true
	value["listRoot"] = false
	value["childCount"] = 0
	value["descendantCount"] = 0
	value["title"] = "子 Agent · " + strings.TrimSuffix(strings.TrimPrefix(subpath, "subagents/agent-"), ".jsonl")
	value["costEstimate"] = claudeEstimate(nil, false)
	delete(value, "tokenUsage")
	delete(value, "tokenUsageSummary")
	value["activity"] = map[string]any{"state": "idle", "generation": 1, "itemCount": 0}
	return value, nil
}

const claudeProjectPath = `CASE WHEN cwd ~ '^[A-Za-z]:' OR left(cwd,2)=E'\\\\' THEN lower(replace(cwd,E'\\','/')) ELSE cwd END`
const claudeProjectNormalized = `coalesce(nullif(regexp_replace(` + claudeProjectPath + `,'/+$',''),''),'/')`

type claudeListCursor struct {
	At       time.Time
	ID       string
	Project  string
	Archived bool
	Parent   string
}

func (server *Server) claudeConversationList(ctx context.Context, r *http.Request) (map[string]any, error) {
	q := r.URL.Query()
	archived := q.Get("archived") == "1"
	project := q.Get("projectKey")
	parent := q.Get("parentThreadId")
	cursor := claudeListCursor{At: time.Now().Add(24 * time.Hour), ID: "ffffffff-ffff-ffff-ffff-ffffffffffff", Project: project, Archived: archived, Parent: parent}
	if raw := q.Get("cursor"); raw != "" {
		data, e := base64.RawURLEncoding.DecodeString(raw)
		if e != nil || json.Unmarshal(data, &cursor) != nil || !claudeUUID(cursor.ID) || cursor.Project != project || cursor.Archived != archived || cursor.Parent != parent {
			return nil, claudeError(400, "Invalid Claude cursor")
		}
	}
	data := []any{}
	projects := []any{}
	var next any
	if q.Get("view") == "children" {
		if !claudeUUID(parent) {
			return nil, claudeError(400, "Invalid parent")
		}
		rows, err := server.pool.Query(ctx, `SELECT thread_id::text FROM mira_claude_transcripts WHERE session_id=$1 AND subpath<>'' AND thread_id<$2::uuid ORDER BY thread_id DESC LIMIT 51`, parent, cursor.ID)
		if err != nil {
			return nil, err
		}
		ids := []string{}
		for rows.Next() {
			var id string
			if err = rows.Scan(&id); err != nil {
				rows.Close()
				return nil, err
			}
			ids = append(ids, id)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
		if len(ids) > 50 {
			ids = ids[:50]
			cursor.ID = ids[len(ids)-1]
			raw, _ := json.Marshal(cursor)
			next = base64.RawURLEncoding.EncodeToString(raw)
		}
		for _, id := range ids {
			value, err := server.claudeConversation(ctx, id)
			if err != nil {
				return nil, err
			}
			data = append(data, value)
		}
	} else {
		var filterNode, filterCwd string
		if project != "" {
			var pair []string
			if json.Unmarshal([]byte(project), &pair) != nil || len(pair) != 2 || pair[0] != "" && !claudeUUID(pair[0]) {
				return nil, claudeError(400, "Invalid project")
			}
			filterNode, filterCwd = pair[0], pair[1]
		}
		rows, err := server.pool.Query(ctx, `SELECT `+claudeColumns+` FROM mira_claude_sessions WHERE archived=$1 AND ($6='' OR node_id::text=$2 AND `+claudeProjectNormalized+`=$3)
 AND (updated_at,session_id)<($4,$5::uuid) ORDER BY updated_at DESC,session_id DESC LIMIT 51`, archived, filterNode, filterCwd, cursor.At, cursor.ID, project)
		if err != nil {
			return nil, err
		}
		sessions := []claudeSession{}
		for rows.Next() {
			s, e := scanClaude(rows)
			if e != nil {
				rows.Close()
				return nil, e
			}
			sessions = append(sessions, s)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
		if len(sessions) > 50 {
			sessions = sessions[:50]
			last := sessions[len(sessions)-1]
			cursor.At = last.UpdatedAt
			cursor.ID = last.ID
			raw, _ := json.Marshal(cursor)
			next = base64.RawURLEncoding.EncodeToString(raw)
		}
		for _, s := range sessions {
			value, err := server.claudeSummary(ctx, s)
			if err != nil {
				return nil, err
			}
			data = append(data, value)
		}
		rows, err = server.pool.Query(ctx, `SELECT node_id::text,`+claudeProjectNormalized+`,min(cwd),count(*) FROM mira_claude_sessions WHERE archived=$1 GROUP BY node_id,`+claudeProjectNormalized, archived)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var node, path, cwd string
			var count int64
			if err = rows.Scan(&node, &path, &cwd, &count); err != nil {
				return nil, err
			}
			key, _ := json.Marshal([]string{node, path})
			projects = append(projects, map[string]any{"key": string(key), "nodeId": node, "cwd": cwd, "count": count})
		}
		if err = rows.Err(); err != nil {
			return nil, err
		}
	}
	result := map[string]any{"data": data, "paged": true, "nextCursor": next}
	if q.Get("view") != "children" {
		result["projects"] = projects
	}
	return result, nil
}

func (server *Server) claudeAccountCosts(ctx context.Context, r *http.Request) (map[string]any, error) {
	q := r.URL.Query()
	name := q.Get("name")
	zone := q.Get("timezone")
	if zone == "" {
		zone = "UTC"
	}
	span := q.Get("range")
	if span == "" {
		span = "7d"
	}
	count := map[string]int{"24h": 1, "7d": 7, "30d": 30}[span]
	loc, err := time.LoadLocation(zone)
	if name == "" || len(name) > 128 || count == 0 || err != nil {
		return nil, claudeError(400, "Invalid cost range, name or timezone")
	}
	now := time.Now().In(loc)
	from := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc).AddDate(0, 0, 1-count)
	rows, err := server.pool.Query(ctx, `SELECT to_char(happened_at AT TIME ZONE $4,'YYYY-MM-DD'),date_trunc('hour',happened_at),sum(cost_delta)::float8,bool_or(cost_partial)
 FROM mira_claude_usage_deltas WHERE account_name=$1 AND happened_at>=$2 AND happened_at<=$3 GROUP BY 1,2 ORDER BY 2`, name, from, now, zone)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	days := []map[string]any{}
	points := []any{}
	byDate := map[string]map[string]any{}
	var total *float64
	partial := false
	for i := 0; i < count; i++ {
		start := from.AddDate(0, 0, i)
		day := map[string]any{"date": start.Format("2006-01-02"), "at": start.UnixMilli(), "end": start.AddDate(0, 0, 1).UnixMilli(), "amount": nil, "status": "unavailable"}
		days = append(days, day)
		byDate[day["date"].(string)] = day
	}
	for rows.Next() {
		var date string
		var hour time.Time
		var amount *float64
		var p bool
		if err = rows.Scan(&date, &hour, &amount, &p); err != nil {
			return nil, err
		}
		partial = partial || p
		day := byDate[date]
		if day == nil {
			continue
		}
		if amount != nil {
			if total == nil {
				v := 0.0
				total = &v
			}
			*total += *amount
			prior, _ := day["amount"].(float64)
			day["amount"] = prior + *amount
			if day["status"] != "partial" {
				day["status"] = "complete"
			}
		}
		if p {
			day["status"] = "partial"
		}
		at := hour.Add(time.Hour)
		end := time.UnixMilli(day["end"].(int64))
		if at.After(end) {
			at = end
		}
		if at.After(now) {
			at = now
		}
		points = append(points, map[string]any{"date": date, "at": at.UnixMilli(), "amount": amount, "status": claudeEstimate(amount, p)["status"]})
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return map[string]any{"name": name, "engine": "claude", "range": span, "timezone": zone, "from": from.UnixMilli(), "to": now.UnixMilli(), "days": days, "points": points, "estimate": claudeEstimate(total, partial), "basis": "claude_sdk", "currency": "USD"}, nil
}
