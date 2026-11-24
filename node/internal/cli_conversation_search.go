package node

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type conversationSearch struct {
	Query     string                     `json:"query"`
	Terms     []string                   `json:"terms"`
	Match     string                     `json:"match"`
	Results   []conversationSearchResult `json:"results"`
	Truncated bool                       `json:"truncated"`
}

type conversationSearchResult struct {
	Engine         string                  `json:"engine"`
	ThreadID       string                  `json:"threadId"`
	ParentThreadID *string                 `json:"parentThreadId"`
	RootThreadID   string                  `json:"rootThreadId"`
	RootTitle      *string                 `json:"rootTitle"`
	Title          *string                 `json:"title"`
	Archived       bool                    `json:"archived"`
	TitleMatch     bool                    `json:"titleMatch"`
	HitCount       int                     `json:"hitCount"`
	Hits           []conversationSearchHit `json:"hits"`
	LatestAt       *string                 `json:"latestAt"`
}

type conversationSearchHit struct {
	ItemSeq    int64   `json:"itemSeq"`
	Role       string  `json:"role"`
	Snippet    string  `json:"snippet"`
	RecordedAt *string `json:"recordedAt"`
}

// conversationSearchText marks a result that RunCLI prints for people.
type conversationSearchText struct{ conversationSearch }

// runConversationSearch searches the user/assistant prose and titles of every
// Codex and Claude conversation (subagents and archived ones included) in the
// Server's conversation index.
func (client *cliClient) runConversationSearch(ctx context.Context, args []string, json bool) (any, error) {
	flags := flag.NewFlagSet("conversation search", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	anyTerm := flags.Bool("any", false, "match conversations containing any term instead of all terms")
	engine := flags.String("engine", "", "codex or claude")
	archived := flags.String("archived", "include", "include, exclude or only")
	since := flags.String("since", "", "only messages since YYYY-MM-DD or an RFC 3339 time")
	limit := flags.Int("limit", 20, "conversations to return (1-100)")
	terms := []string{}
	// Terms and options may be mixed; everything that is not an option is a term.
	rest := args
	for len(rest) > 0 {
		if err := flags.Parse(rest); err != nil {
			return nil, err
		}
		rest = flags.Args()
		if len(rest) > 0 {
			terms, rest = append(terms, rest[0]), rest[1:]
		}
	}
	query := strings.TrimSpace(strings.Join(terms, " "))
	if query == "" {
		return nil, fmt.Errorf("search terms are required")
	}
	if *limit < 1 || *limit > 100 {
		return nil, fmt.Errorf("--limit must be between 1 and 100")
	}
	if *engine != "" && *engine != "codex" && *engine != "claude" {
		return nil, fmt.Errorf("--engine must be codex or claude")
	}
	if *archived != "include" && *archived != "exclude" && *archived != "only" {
		return nil, fmt.Errorf("--archived must be include, exclude or only")
	}
	values := url.Values{"q": {query}, "limit": {strconv.Itoa(*limit)}, "archived": {*archived}}
	if *anyTerm {
		values.Set("match", "any")
	}
	if *engine != "" {
		values.Set("engine", *engine)
	}
	if *since != "" {
		parsed, err := time.ParseInLocation("2006-01-02", *since, time.Local)
		if err != nil {
			if parsed, err = time.Parse(time.RFC3339, *since); err != nil {
				return nil, fmt.Errorf("--since must be YYYY-MM-DD or an RFC 3339 time")
			}
		}
		values.Set("since", parsed.Format(time.RFC3339))
	}
	var result conversationSearch
	if err := client.request(ctx, http.MethodGet, "/v1/conversations/search?"+values.Encode(), nil, &result); err != nil {
		return nil, client.explainMissingRoute(ctx, err, "conversation search", "1.0.134")
	}
	if result.Results == nil {
		result.Results = []conversationSearchResult{}
	}
	if json {
		return result, nil
	}
	return conversationSearchText{result}, nil
}

func printConversationSearch(writer io.Writer, search conversationSearch) error {
	var out strings.Builder
	match := "all terms"
	if search.Match == "any" {
		match = "any term"
	}
	fmt.Fprintf(&out, "%d conversation(s) matching %s of: %s\n", len(search.Results), match, strings.Join(search.Terms, ", "))
	if search.Truncated {
		out.WriteString("(more matches exist; narrow the terms, add --since or raise --limit)\n")
	}
	for _, result := range search.Results {
		title := "(untitled)"
		if result.Title != nil && strings.TrimSpace(*result.Title) != "" {
			title = *result.Title
		}
		fmt.Fprintf(&out, "\n## %s\n\n- thread: %s (%s)", title, result.ThreadID, result.Engine)
		if result.ThreadID != result.RootThreadID {
			root := result.RootThreadID
			if result.RootTitle != nil && *result.RootTitle != "" {
				root += " " + *result.RootTitle
			}
			fmt.Fprintf(&out, "\n- root: %s", root)
		}
		fmt.Fprintf(&out, "\n- hits: %d", result.HitCount)
		if result.TitleMatch {
			out.WriteString(" (title matches)")
		}
		if result.LatestAt != nil {
			if parsed, err := time.Parse(time.RFC3339Nano, *result.LatestAt); err == nil {
				fmt.Fprintf(&out, ", latest %s", parsed.Local().Format("2006-01-02 15:04"))
			}
		}
		if result.Archived {
			out.WriteString(", archived")
		}
		out.WriteString("\n")
		for _, hit := range result.Hits {
			// --after seq-1 starts a read at this message.
			fmt.Fprintf(&out, "  - #%d %s: %s\n", hit.ItemSeq, hit.Role, hit.Snippet)
		}
	}
	if len(search.Results) > 0 {
		out.WriteString("\nRead one with: mira conversation read <thread> [--after SEQ-1]\n")
	}
	_, err := io.WriteString(writer, out.String())
	return err
}
