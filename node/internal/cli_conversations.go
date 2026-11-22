package node

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var conversationIDPattern = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

const (
	conversationToolLimitDefault = 8 * 1024
	conversationToolLimitMax     = 1024 * 1024
)

// conversationRead is the combined result of every page that was read.
type conversationRead struct {
	Engine         string              `json:"engine"`
	ThreadID       string              `json:"threadId"`
	SessionID      string              `json:"sessionId,omitempty"`
	Title          *string             `json:"title"`
	ParentThreadID *string             `json:"parentThreadId"`
	Cwd            *string             `json:"cwd"`
	Archived       bool                `json:"archived"`
	UpdatedAt      *string             `json:"updatedAt"`
	Complete       bool                `json:"complete"`
	Tools          bool                `json:"tools"`
	Children       []conversationChild `json:"children"`
	Items          []conversationItem  `json:"items"`
	NextCursor     *int64              `json:"nextCursor"`
}

type conversationChild struct {
	ThreadID string  `json:"threadId"`
	Title    *string `json:"title"`
}

type conversationItem struct {
	Seq        int64             `json:"seq"`
	Kind       string            `json:"kind"`
	Text       string            `json:"text,omitempty"`
	Summary    string            `json:"summary,omitempty"`
	RecordedAt *string           `json:"recordedAt"`
	Tool       *conversationTool `json:"tool,omitempty"`
}

type conversationTool struct {
	Key             string `json:"key"`
	Name            string `json:"name"`
	Status          string `json:"status,omitempty"`
	Input           string `json:"input,omitempty"`
	Output          string `json:"output,omitempty"`
	InputBytes      int    `json:"inputBytes"`
	OutputBytes     int    `json:"outputBytes"`
	InputTruncated  bool   `json:"inputTruncated,omitempty"`
	OutputTruncated bool   `json:"outputTruncated,omitempty"`
	HasOutput       bool   `json:"hasOutput"`
}

// conversationText marks a result that RunCLI prints as a readable transcript.
type conversationText struct{ conversationRead }

var conversationKinds = map[string]bool{"user": true, "assistant": true, "tool": true, "compaction": true, "error": true, "notice": true}

func (client *cliClient) runConversation(ctx context.Context, args []string, json bool) (any, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("conversation action is required: read")
	}
	if args[0] != "read" {
		return nil, fmt.Errorf("unknown conversation action %s", args[0])
	}
	flags := flag.NewFlagSet("conversation read", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	after := flags.Int64("after", 0, "only items after this sequence")
	maximum := flags.Int("max", 0, "stop after this many items (0 = all)")
	tail := flags.Int("tail", 0, "keep only the last N items")
	kinds := flags.String("kind", "", "comma-separated kinds: user,assistant,tool,compaction,error,notice")
	noTools := flags.Bool("no-tools", false, "read only user/assistant prose from the conversation index")
	toolLimit := flags.Int("tool-output-limit", conversationToolLimitDefault, "bytes kept per tool input/output (0 = 1 MiB maximum)")
	// Accept the ID before or after options.
	id := ""
	rest := args[1:]
	if len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
		id, rest = rest[0], rest[1:]
	}
	if err := flags.Parse(rest); err != nil {
		return nil, err
	}
	if id == "" && flags.NArg() > 0 {
		id = flags.Arg(0)
		if err := flags.Parse(flags.Args()[1:]); err != nil {
			return nil, err
		}
	}
	if flags.NArg() > 0 {
		return nil, fmt.Errorf("unexpected conversation arguments")
	}
	id = strings.ToLower(strings.TrimSpace(id))
	if !conversationIDPattern.MatchString(id) {
		return nil, fmt.Errorf("conversation ID (UUID) is required")
	}
	selected := map[string]bool{}
	for _, kind := range strings.Split(*kinds, ",") {
		if kind = strings.TrimSpace(kind); kind == "" {
			continue
		}
		if !conversationKinds[kind] {
			return nil, fmt.Errorf("--kind accepts user, assistant, tool, compaction, error and notice")
		}
		selected[kind] = true
	}
	if *after < 0 || *maximum < 0 || *tail < 0 {
		return nil, fmt.Errorf("--after, --max and --tail must not be negative")
	}
	if *toolLimit < 0 || *toolLimit > conversationToolLimitMax {
		return nil, fmt.Errorf("--tool-output-limit must be between 0 and %d bytes", conversationToolLimitMax)
	}
	query := url.Values{}
	if !*noTools {
		query.Set("tools", "1")
		query.Set("toolOutputLimit", strconv.Itoa(*toolLimit))
	}
	var combined conversationRead
	tools := map[string]int{}
	cursor := *after
	for page := 0; ; page++ {
		query.Set("after", strconv.FormatInt(cursor, 10))
		var result conversationRead
		if err := client.request(ctx, http.MethodGet, "/v1/conversations/"+id+"/messages?"+query.Encode(), nil, &result); err != nil {
			return nil, client.explainMissingRoute(ctx, err, "conversation read", "1.0.131")
		}
		if page == 0 {
			combined = result
			combined.Items = nil
		}
		for _, item := range result.Items {
			// A compaction summary whose boundary ended the previous page.
			if item.Kind == "compaction" && item.Text == "" && item.Summary != "" && len(combined.Items) > 0 {
				if last := &combined.Items[len(combined.Items)-1]; last.Kind == "compaction" && last.Summary == "" {
					last.Summary = item.Summary
					continue
				}
			}
			// A tool call whose output landed on the next page shares its key.
			if item.Tool != nil && item.Tool.Key != "" {
				if index, ok := tools[item.Tool.Key]; ok {
					mergeConversationTool(combined.Items[index].Tool, item.Tool)
					continue
				}
				tools[item.Tool.Key] = len(combined.Items)
			}
			combined.Items = append(combined.Items, item)
		}
		combined.NextCursor = result.NextCursor
		if result.NextCursor == nil || *result.NextCursor <= cursor {
			combined.NextCursor = nil
			break
		}
		cursor = *result.NextCursor
		if *maximum > 0 && *tail == 0 && len(selected) == 0 && len(combined.Items) >= *maximum {
			break
		}
	}
	if len(selected) > 0 {
		filtered := combined.Items[:0]
		for _, item := range combined.Items {
			if selected[item.Kind] {
				filtered = append(filtered, item)
			}
		}
		combined.Items = filtered
	}
	if *tail > 0 && len(combined.Items) > *tail {
		combined.Items = combined.Items[len(combined.Items)-*tail:]
	}
	if *maximum > 0 && len(combined.Items) > *maximum {
		// One record can yield several items; never skip the rest of it.
		next := combined.Items[*maximum-1].Seq
		if combined.Items[*maximum].Seq == next {
			next--
		}
		combined.Items = combined.Items[:*maximum]
		combined.NextCursor = &next
	}
	if combined.Items == nil {
		combined.Items = []conversationItem{}
	}
	if combined.Children == nil {
		combined.Children = []conversationChild{}
	}
	if json {
		return combined, nil
	}
	return conversationText{combined}, nil
}

func mergeConversationTool(target, later *conversationTool) {
	if target.Name == "" {
		target.Name = later.Name
	}
	if later.Status != "" {
		target.Status = later.Status
	}
	if target.Input == "" && later.Input != "" {
		target.Input, target.InputBytes, target.InputTruncated = later.Input, later.InputBytes, later.InputTruncated
	}
	if later.HasOutput {
		target.Output, target.OutputBytes, target.OutputTruncated, target.HasOutput = later.Output, later.OutputBytes, later.OutputTruncated, true
	}
}

// fenced wraps text in a Markdown code fence longer than any backtick run in it.
func fenced(text string) string {
	longest, run := 0, 0
	for _, character := range text {
		if character == '`' {
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
	}
	fence := strings.Repeat("`", max(3, longest+1))
	return fence + "\n" + strings.TrimRight(text, "\n") + "\n" + fence
}

func printConversation(writer io.Writer, conversation conversationRead) error {
	title := "(untitled)"
	if conversation.Title != nil && strings.TrimSpace(*conversation.Title) != "" {
		title = *conversation.Title
	}
	var out strings.Builder
	fmt.Fprintf(&out, "# %s\n\n", title)
	fmt.Fprintf(&out, "- engine: %s\n- thread: %s\n", conversation.Engine, conversation.ThreadID)
	if conversation.ParentThreadID != nil {
		fmt.Fprintf(&out, "- parent: %s\n", *conversation.ParentThreadID)
	}
	if conversation.Cwd != nil && *conversation.Cwd != "" {
		fmt.Fprintf(&out, "- cwd: %s\n", *conversation.Cwd)
	}
	if conversation.Archived {
		out.WriteString("- archived: true\n")
	}
	for _, child := range conversation.Children {
		label := ""
		if child.Title != nil {
			label = " " + *child.Title
		}
		fmt.Fprintf(&out, "- subagent: %s%s\n", child.ThreadID, label)
	}
	if !conversation.Complete {
		out.WriteString("- note: the conversation index is still catching up; later messages may be missing\n")
	}
	if conversation.NextCursor != nil {
		fmt.Fprintf(&out, "- more: continue with --after %d\n", *conversation.NextCursor)
	}
	if _, err := io.WriteString(writer, out.String()); err != nil {
		return err
	}
	for _, item := range conversation.Items {
		out.Reset()
		heading := []string{item.Kind}
		if item.Tool != nil {
			if item.Tool.Name != "" {
				heading = append(heading, item.Tool.Name)
			}
			if item.Tool.Status != "" {
				heading = append(heading, item.Tool.Status)
			}
		}
		if item.RecordedAt != nil {
			if parsed, err := time.Parse(time.RFC3339Nano, *item.RecordedAt); err == nil {
				heading = append(heading, parsed.Local().Format("2006-01-02 15:04:05"))
			}
		}
		heading = append(heading, "#"+strconv.FormatInt(item.Seq, 10))
		fmt.Fprintf(&out, "\n## %s\n\n", strings.Join(heading, " · "))
		if tool := item.Tool; tool != nil {
			if tool.Input != "" {
				fmt.Fprintf(&out, "Input%s:\n%s\n", truncationNote(tool.InputBytes, len(tool.Input), tool.InputTruncated), fenced(tool.Input))
			}
			if tool.HasOutput {
				if tool.Input != "" {
					out.WriteString("\n")
				}
				fmt.Fprintf(&out, "Output%s:\n%s\n", truncationNote(tool.OutputBytes, len(tool.Output), tool.OutputTruncated), fenced(tool.Output))
			} else {
				out.WriteString("(no output recorded)\n")
			}
		} else {
			out.WriteString(strings.TrimRight(item.Text, "\n") + "\n")
			if item.Summary != "" {
				fmt.Fprintf(&out, "\nSummary:\n%s\n", fenced(item.Summary))
			}
		}
		if _, err := io.WriteString(writer, out.String()); err != nil {
			return err
		}
	}
	return nil
}

func truncationNote(total, kept int, truncated bool) string {
	if !truncated {
		return ""
	}
	return fmt.Sprintf(" (first %d of %d bytes; raise --tool-output-limit for more)", kept, total)
}

// explainMissingRoute turns the Server's generic 404 for an unknown route into
// a version message. A missing resource keeps its own, more specific error.
func (client *cliClient) explainMissingRoute(ctx context.Context, err error, feature, since string) error {
	var httpError *cliHTTPError
	if !errors.As(err, &httpError) || httpError.Status != http.StatusNotFound || httpError.Message != "not found" {
		return err
	}
	var health struct {
		Version string `json:"version"`
	}
	if client.request(ctx, http.MethodGet, "/healthz", nil, &health) != nil || health.Version == "" {
		health.Version = "(unknown version)"
	}
	return &cliHTTPError{Status: http.StatusNotFound, Code: "unsupported_by_server",
		Message: fmt.Sprintf("Mira Server %s at %s does not support `mira %s`; it needs %s or newer", health.Version, client.identity.ServerURL, feature, since)}
}
