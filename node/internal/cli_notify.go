package node

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// runNotify queues a system alert for the administrator's Android devices that
// have native notifications enabled. It authenticates with this Node's credential.
func (client *cliClient) runNotify(ctx context.Context, args []string) (any, error) {
	flags := flag.NewFlagSet("notify", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	title := flags.String("title", "", "notification title (1-120 characters)")
	body := flags.String("body", "", "notification text (up to 1000 characters)")
	level := flags.String("level", "warning", "critical, warning or info")
	link := flags.String("url", "", "https URL opened when the notification is tapped")
	if err := flags.Parse(args); err != nil {
		return nil, err
	}
	if flags.NArg() != 0 {
		return nil, fmt.Errorf("unexpected notify arguments; quote the title and body")
	}
	if strings.TrimSpace(*title) == "" {
		return nil, fmt.Errorf("--title is required")
	}
	var result map[string]any
	err := client.request(ctx, http.MethodPost, "/v1/notifications/alerts", map[string]any{
		"title": *title, "body": *body, "level": *level, "url": *link,
	}, &result)
	return result, err
}
