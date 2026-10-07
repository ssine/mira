package node

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
)

func (client *cliClient) runSite(ctx context.Context, args []string) (any, error) {
	action := "list"
	if len(args) > 0 {
		action = args[0]
		args = args[1:]
	}
	flags := flag.NewFlagSet("site "+action, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	name := flags.String("name", "", "site name")
	selector := flags.String("node", "", "target Node")
	port := flags.Int("port", 0, "loopback port")
	scheme := flags.String("scheme", "", "http or https")
	enabled := flags.Bool("enabled", true, "enable route")
	revision := flags.Int64("expected-revision", 0, "revision from site get")
	limit := flags.Int("limit", 50, "page size")
	after := flags.String("after", "", "page cursor")
	if err := flags.Parse(args); err != nil {
		return nil, err
	}
	present := map[string]bool{}
	flags.Visit(func(f *flag.Flag) { present[f.Name] = true })
	key := ""
	if flags.NArg() > 0 {
		key = flags.Arg(0)
	}
	if flags.NArg() > 1 {
		return nil, fmt.Errorf("unexpected site arguments")
	}
	route := "/v1/sites"
	method := http.MethodGet
	body := map[string]any{}
	switch action {
	case "list":
		if key != "" {
			return nil, fmt.Errorf("list does not accept a site")
		}
		route += "?" + url.Values{"limit": {strconv.Itoa(*limit)}, "after": {*after}}.Encode()
	case "get", "update", "delete":
		if key == "" {
			return nil, fmt.Errorf("site UUID or name is required before options")
		}
		route += "/" + url.PathEscape(key)
		if action != "get" {
			body["expectedRevision"] = *revision
			method = http.MethodPatch
			if action == "delete" {
				method = http.MethodDelete
			}
		}
	case "create":
		if key != "" {
			return nil, fmt.Errorf("create uses --name")
		}
		method = http.MethodPost
		body["name"] = *name
		body["port"] = *port
	default:
		return nil, fmt.Errorf("unknown site action %s", action)
	}
	if action == "create" || action == "update" {
		if *selector != "" {
			n, err := client.resolveNode(ctx, *selector)
			if err != nil {
				return nil, err
			}
			body["nodeId"] = n["nodeId"]
		}
		if present["port"] {
			body["port"] = *port
		}
		if present["scheme"] {
			body["scheme"] = *scheme
		}
		if present["enabled"] {
			body["enabled"] = *enabled
		}
	}
	var result any
	var input any
	if method != http.MethodGet {
		input = body
	}
	err := client.request(ctx, method, route, input, &result)
	return result, err
}
