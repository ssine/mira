package views

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

const (
	searchMaxTerms      = 8
	searchMaxTermLength = 200
	searchHitScan       = 2000
	searchHitsPerThread = 3
	searchSnippetRunes  = 72
)

type SearchOptions struct {
	Query    string
	Archived string // "include" (default), "exclude" or "only"
	Limit    int
}

type searchHit struct {
	ItemSeq    int64   `json:"itemSeq"`
	Role       string  `json:"role"`
	Snippet    string  `json:"snippet"`
	RecordedAt *string `json:"recordedAt"`
}

type searchResult struct {
	Engine         string      `json:"engine"`
	ThreadID       string      `json:"threadId"`
	ParentThreadID *string     `json:"parentThreadId"`
	RootThreadID   string      `json:"rootThreadId"`
	RootTitle      *string     `json:"rootTitle"`
	Title          *string     `json:"title"`
	Archived       bool        `json:"archived"`
	TitleMatch     bool        `json:"titleMatch"`
	HitCount       int         `json:"hitCount"`
	Hits           []searchHit `json:"hits"`
	LatestAt       *string     `json:"latestAt"`
	latest         time.Time
	generation     int64
}

// SearchTerms splits a query into at most eight case-insensitive substrings.
func SearchTerms(query string) []string {
	terms := []string{}
	seen := map[string]bool{}
	for _, field := range strings.FieldsFunc(query, unicode.IsSpace) {
		if utf8.RuneCountInString(field) > searchMaxTermLength {
			field = string([]rune(field)[:searchMaxTermLength])
		}
		key := strings.ToLower(field)
		if field == "" || seen[key] {
			continue
		}
		seen[key] = true
		terms = append(terms, field)
		if len(terms) == searchMaxTerms {
			break
		}
	}
	return terms
}

func likePattern(term string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return "%" + replacer.Replace(term) + "%"
}

// searchSnippet returns a single-line excerpt around the first matched term.
func searchSnippet(body string, terms []string) string {
	text := strings.Join(strings.Fields(body), " ")
	runes := []rune(text)
	lower := []rune(strings.ToLower(text))
	position := -1
	for _, term := range terms {
		needle := []rune(strings.ToLower(term))
		for index := 0; index+len(needle) <= len(lower); index++ {
			if string(lower[index:index+len(needle)]) == string(needle) {
				if position < 0 || index < position {
					position = index
				}
				break
			}
		}
	}
	if position < 0 {
		position = 0
	}
	start := max(0, position-searchSnippetRunes/3)
	end := min(len(runes), start+searchSnippetRunes)
	snippet := string(runes[start:end])
	if start > 0 {
		snippet = "…" + snippet
	}
	if end < len(runes) {
		snippet += "…"
	}
	return snippet
}

// SearchConversations finds Codex and Claude conversations whose titles or
// user/assistant prose contain every term. Archived conversations and
// subagents are included; each result names its root conversation.
func (service *Service) SearchConversations(ctx context.Context, storeID string, options SearchOptions) (Result, error) {
	terms := SearchTerms(options.Query)
	if len(terms) == 0 {
		return Result{Status: 200, Body: map[string]any{"query": options.Query, "terms": terms, "results": []any{}}}, nil
	}
	limit := options.Limit
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	archived := options.Archived
	if archived != "exclude" && archived != "only" {
		archived = "include"
	}
	patterns := make([]any, len(terms))
	bodyClauses := make([]string, len(terms))
	titleClauses := make([]string, len(terms))
	for index, term := range terms {
		patterns[index] = likePattern(term)
		bodyClauses[index] = fmt.Sprintf("m.body ILIKE $%d", index+3)
		titleClauses[index] = fmt.Sprintf("t.title ILIKE $%d", index+3)
	}
	results := map[string]*searchResult{}
	resultKey := func(engine, threadID string) string { return engine + ":" + threadID }
	truncated := false
	err := pgx.BeginTxFunc(ctx, service.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		// One- and two-character terms cannot use trigrams; bound the scan.
		if _, err := tx.Exec(ctx, `SET LOCAL statement_timeout = '15s'`); err != nil {
			return err
		}
		args := append([]any{storeID, searchHitScan}, patterns...)
		rows, err := tx.Query(ctx, `SELECT m.engine,m.thread_id,m.generation,m.item_seq,m.role,m.body,m.recorded_at
		  FROM mira_conversation_messages m
		  WHERE (m.engine='claude' OR m.store_id=$1) AND `+strings.Join(bodyClauses, " AND ")+`
		  ORDER BY m.recorded_at DESC NULLS LAST LIMIT $2`, args...)
		if err != nil {
			return err
		}
		scanned := 0
		for rows.Next() {
			var engine, threadID, role, body string
			var generation, sequence int64
			var recordedAt *time.Time
			if err := rows.Scan(&engine, &threadID, &generation, &sequence, &role, &body, &recordedAt); err != nil {
				rows.Close()
				return err
			}
			scanned++
			key := resultKey(engine, threadID)
			result := results[key]
			if result == nil {
				result = &searchResult{Engine: engine, ThreadID: threadID, generation: generation, Hits: []searchHit{}}
				results[key] = result
			}
			if result.generation != generation {
				continue // Stale generations are filtered against the projection below.
			}
			result.HitCount++
			if recordedAt != nil && recordedAt.After(result.latest) {
				result.latest = *recordedAt
			}
			if len(result.Hits) < searchHitsPerThread {
				result.Hits = append(result.Hits, searchHit{ItemSeq: sequence, Role: role, Snippet: searchSnippet(body, terms), RecordedAt: optionalTime(recordedAt)})
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		truncated = scanned == searchHitScan
		// Titles: Codex renames live in state entries; Claude sessions carry theirs.
		titleRows, err := tx.Query(ctx, `SELECT 'codex',t.thread_id,t.active_generation FROM (
		    SELECT p.thread_id,p.active_generation,COALESCE(NULLIF(n.value#>>'{}',''),p.title) AS title FROM codex_thread_projections p
		    LEFT JOIN codex_store_state_entries n ON n.store_id=p.store_id AND n.field='names' AND NOT n.is_root AND n.entry_key=p.thread_id
		    WHERE p.store_id=$1) t WHERE `+strings.Join(titleClauses, " AND ")+`
		  UNION ALL
		  SELECT 'claude',t.session_id::text,1 FROM mira_claude_sessions t WHERE `+strings.Join(titleClauses, " AND ")+`
		  LIMIT $2`, args...)
		if err != nil {
			return err
		}
		for titleRows.Next() {
			var engine, threadID string
			var generation int64
			if err := titleRows.Scan(&engine, &threadID, &generation); err != nil {
				titleRows.Close()
				return err
			}
			key := resultKey(engine, threadID)
			if results[key] == nil {
				results[key] = &searchResult{Engine: engine, ThreadID: threadID, generation: generation, Hits: []searchHit{}}
			}
			results[key].TitleMatch = true
		}
		titleRows.Close()
		if err := titleRows.Err(); err != nil {
			return err
		}
		return service.describeSearchResults(ctx, tx, storeID, results)
	})
	if err != nil {
		return Result{}, err
	}
	list := []*searchResult{}
	for _, result := range results {
		if result.RootThreadID == "" {
			continue // Deleted, or a stale generation that is still being re-indexed.
		}
		if archived == "exclude" && result.Archived || archived == "only" && !result.Archived {
			continue
		}
		if result.HitCount == 0 && !result.TitleMatch {
			continue
		}
		result.LatestAt = optionalTime(&result.latest)
		if result.latest.IsZero() {
			result.LatestAt = nil
		}
		list = append(list, result)
	}
	sort.SliceStable(list, func(i, j int) bool {
		if list[i].TitleMatch != list[j].TitleMatch {
			return list[i].TitleMatch
		}
		if !list[i].latest.Equal(list[j].latest) {
			return list[i].latest.After(list[j].latest)
		}
		return list[i].ThreadID > list[j].ThreadID
	})
	if len(list) > limit {
		list, truncated = list[:limit], true
	}
	return Result{Status: 200, Body: map[string]any{"query": options.Query, "terms": terms, "results": list, "truncated": truncated}}, nil
}

// describeSearchResults fills titles, archive state and root conversations,
// and leaves RootThreadID empty for deleted threads or stale generations.
func (service *Service) describeSearchResults(ctx context.Context, tx pgx.Tx, storeID string, results map[string]*searchResult) error {
	codexIDs, claudeIDs := []string{}, []string{}
	for _, result := range results {
		if result.Engine == "codex" {
			codexIDs = append(codexIDs, result.ThreadID)
		} else {
			claudeIDs = append(claudeIDs, result.ThreadID)
		}
	}
	if len(codexIDs) > 0 {
		rows, err := tx.Query(ctx, `WITH RECURSIVE chain AS (
		    SELECT p.thread_id AS origin,p.thread_id,p.parent_thread_id,0 AS depth FROM codex_thread_projections p
		    WHERE p.store_id=$1 AND p.thread_id=ANY($2::text[])
		    UNION ALL
		    SELECT c.origin,p.thread_id,p.parent_thread_id,c.depth+1 FROM chain c JOIN codex_thread_projections p
		      ON p.store_id=$1 AND p.thread_id=c.parent_thread_id AND p.thread_id<>c.thread_id WHERE c.depth<32
		  ), roots AS (SELECT DISTINCT ON (origin) origin,thread_id AS root FROM chain ORDER BY origin,depth DESC)
		  SELECT p.thread_id,p.parent_thread_id,p.active_generation,r.root,
		    COALESCE(NULLIF(n.value#>>'{}',''),p.title),COALESCE(NULLIF(rn.value#>>'{}',''),rp.title),
		    COALESCE((SELECT action='archive' FROM mira_thread_actions a WHERE a.store_id=p.store_id AND a.thread_id=r.root ORDER BY action_seq DESC LIMIT 1),false)
		  FROM codex_thread_projections p JOIN roots r ON r.origin=p.thread_id
		  JOIN codex_thread_projections rp ON rp.store_id=p.store_id AND rp.thread_id=r.root
		  LEFT JOIN codex_store_state_entries n ON n.store_id=p.store_id AND n.field='names' AND NOT n.is_root AND n.entry_key=p.thread_id
		  LEFT JOIN codex_store_state_entries rn ON rn.store_id=p.store_id AND rn.field='names' AND NOT rn.is_root AND rn.entry_key=r.root
		  WHERE p.store_id=$1 AND NOT EXISTS(SELECT 1 FROM mira_thread_actions d WHERE d.store_id=p.store_id AND d.thread_id=p.thread_id AND d.action='delete')`, storeID, codexIDs)
		if err != nil {
			return err
		}
		for rows.Next() {
			var threadID, root string
			var parent, title, rootTitle *string
			var generation int64
			var archived bool
			if err := rows.Scan(&threadID, &parent, &generation, &root, &title, &rootTitle, &archived); err != nil {
				rows.Close()
				return err
			}
			result := results["codex:"+threadID]
			if result == nil || result.generation != generation {
				continue
			}
			result.ParentThreadID, result.RootThreadID, result.Title, result.RootTitle, result.Archived = parent, root, title, rootTitle, archived
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
	}
	if len(claudeIDs) > 0 {
		rows, err := tx.Query(ctx, `SELECT s.session_id::text,NULL::text,s.session_id::text,s.title,s.title,s.archived FROM mira_claude_sessions s WHERE s.session_id::text=ANY($1::text[])
		  UNION ALL
		  SELECT t.thread_id::text,t.session_id::text,t.session_id::text,'子 Agent · '||regexp_replace(regexp_replace(t.subpath,'^subagents/agent-',''),'\.jsonl$',''),s.title,s.archived
		  FROM mira_claude_transcripts t JOIN mira_claude_sessions s USING(session_id) WHERE t.subpath<>'' AND t.thread_id::text=ANY($1::text[])`, claudeIDs)
		if err != nil {
			return err
		}
		for rows.Next() {
			var threadID, root string
			var parent, title, rootTitle *string
			var archived bool
			if err := rows.Scan(&threadID, &parent, &root, &title, &rootTitle, &archived); err != nil {
				rows.Close()
				return err
			}
			if result := results["claude:"+threadID]; result != nil {
				result.ParentThreadID, result.RootThreadID, result.Title, result.RootTitle, result.Archived = parent, root, title, rootTitle, archived
			}
		}
		rows.Close()
		return rows.Err()
	}
	return nil
}
