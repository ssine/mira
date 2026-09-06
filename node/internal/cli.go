package node

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/gorilla/websocket"
)

type cliOptions struct {
	JSON     bool
	Timeout  time.Duration
	Identity string
}

type cliHTTPError struct {
	Status  int
	Code    string
	Message string
}

func (value *cliHTTPError) Error() string { return value.Message }

type cliClient struct {
	options  cliOptions
	identity *persistedNodeState
	http     *http.Client
}

type stringList []string

func (values *stringList) String() string         { return strings.Join(*values, ",") }
func (values *stringList) Set(value string) error { *values = append(*values, value); return nil }

func parseGlobalCLI(args []string) (cliOptions, []string, error) {
	identityFile, err := DefaultIdentityFile()
	if err != nil {
		return cliOptions{}, nil, err
	}
	options := cliOptions{Timeout: 30 * time.Second, Identity: identityFile}
	remaining := make([]string, 0, len(args))
	parsing := true
	for index := 0; index < len(args); index++ {
		arg := args[index]
		if arg == "--" {
			parsing = false
			remaining = append(remaining, arg)
			continue
		}
		if !parsing {
			remaining = append(remaining, arg)
			continue
		}
		switch {
		case arg == "--json":
			options.JSON = true
		case arg == "--version" && len(remaining) == 0:
			remaining = append(remaining, "version")
		case arg == "--timeout":
			if index+1 >= len(args) {
				return options, nil, fmt.Errorf("%s requires a value", arg)
			}
			index++
			if options.Timeout, err = time.ParseDuration(args[index]); err != nil {
				return options, nil, fmt.Errorf("parse --timeout: %w", err)
			}
		case strings.HasPrefix(arg, "--timeout="):
			options.Timeout, err = time.ParseDuration(strings.TrimPrefix(arg, "--timeout="))
			if err != nil {
				return options, nil, fmt.Errorf("parse --timeout: %w", err)
			}
		case arg == "--server" || strings.HasPrefix(arg, "--server="):
			if len(remaining) > 0 && remaining[0] == "setup" {
				remaining = append(remaining, arg)
				continue
			}
			return options, nil, fmt.Errorf("--server is not supported: a Node credential is bound to the Server URL in its identity file")
		default:
			remaining = append(remaining, arg)
		}
	}
	if options.Timeout < 100*time.Millisecond || options.Timeout > 10*time.Minute {
		return options, nil, fmt.Errorf("--timeout must be between 100ms and 10m")
	}
	return options, remaining, nil
}

func newCLIClient(options cliOptions) (*cliClient, error) {
	identity, err := loadIdentity(options.Identity)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, &cliHTTPError{Status: 428, Code: "not_enrolled", Message: "This machine is not enrolled in Mira. Start mira node-worker to submit an enrollment request"}
		}
		return nil, err
	}
	if identity.NodeID == "" || identity.Enrollment.Status != "approved" {
		return nil, &cliHTTPError{Status: 428, Code: "not_enrolled", Message: "This machine is not enrolled in Mira. Start mira node-worker and wait for administrator approval"}
	}
	server := strings.TrimRight(identity.ServerURL, "/")
	parsed, err := url.Parse(server)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, fmt.Errorf("identity Server URL must be an absolute HTTP(S) URL")
	}
	identityCopy := *identity
	identityCopy.ServerURL = strings.TrimRight(server, "/")
	return &cliClient{options: options, identity: &identityCopy, http: &http.Client{Timeout: options.Timeout}}, nil
}

func (client *cliClient) request(ctx context.Context, method, route string, body any, result any) error {
	var input io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		input = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, client.identity.ServerURL+route, input)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+client.identity.Token)
	request.Header.Set("X-Mira-Client-Type", "cli")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := client.http.Do(request)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return &cliHTTPError{Status: 504, Code: "timeout", Message: "Mira request timed out"}
		}
		return err
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, 64*1024*1024))
	if err != nil {
		return err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		value := struct {
			Error string `json:"error"`
			Code  string `json:"code"`
		}{}
		_ = json.Unmarshal(payload, &value)
		if value.Error == "" {
			value.Error = fmt.Sprintf("Mira Server returned HTTP %d", response.StatusCode)
		}
		return &cliHTTPError{Status: response.StatusCode, Code: value.Code, Message: value.Error}
	}
	if result != nil && len(payload) > 0 {
		return json.Unmarshal(payload, result)
	}
	return nil
}

func (client *cliClient) nodes(ctx context.Context) ([]map[string]any, error) {
	return client.filteredNodes(ctx, false, "", "", nil)
}

func (client *cliClient) filteredNodes(ctx context.Context, summary bool, capability, status string, labels []string) ([]map[string]any, error) {
	var response struct {
		Data []map[string]any `json:"data"`
	}
	query := url.Values{}
	if summary {
		query.Set("view", "summary")
	}
	if capability != "" {
		query.Set("capability", capability)
	}
	if status != "" {
		query.Set("status", status)
	}
	for _, label := range labels {
		query.Add("label", label)
	}
	route := "/v1/nodes"
	if encoded := query.Encode(); encoded != "" {
		route += "?" + encoded
	}
	err := client.request(ctx, http.MethodGet, route, nil, &response)
	if err == nil && summary {
		for index, node := range response.Data {
			response.Data[index] = summarizeNode(node)
		}
	}
	if err == nil && (capability != "" || status != "" || len(labels) > 0) {
		filtered := make([]map[string]any, 0, len(response.Data))
		for _, node := range response.Data {
			capabilities, _ := node["capabilities"].(map[string]any)
			enabled, _ := capabilities[capability].(bool)
			if (capability == "" || enabled) && (status == "" || selectorValue(node, "status") == status) && nodeMatchesLabels(node, labels) {
				filtered = append(filtered, node)
			}
		}
		response.Data = filtered
	}
	return response.Data, err
}

func nodeMatchesLabels(node map[string]any, filters []string) bool {
	labels, _ := node["labels"].(map[string]any)
	for _, filter := range filters {
		parts := strings.SplitN(filter, "=", 2)
		if len(parts) != 2 || fmt.Sprint(labels[parts[0]]) != parts[1] {
			return false
		}
	}
	return true
}

func summarizeNode(node map[string]any) map[string]any {
	aliases := node["aliases"]
	if aliases == nil {
		aliases = []string{}
	}
	labels := node["labels"]
	if labels == nil {
		labels = map[string]any{}
	}
	capabilities := map[string]any{}
	if reported, ok := node["capabilities"].(map[string]any); ok {
		for name, value := range reported {
			if enabled, ok := value.(bool); ok && enabled {
				capabilities[name] = true
			}
		}
	}
	appServerStatus := selectorValue(node, "appServerStatus")
	if appServerStatus == "" {
		if enabled, _ := capabilities["appServer"].(bool); enabled {
			appServerStatus = nodeAppServerStatus(node)
		} else {
			appServerStatus = "unsupported"
		}
	}
	return map[string]any{
		"nodeId": node["nodeId"], "nodeKey": node["nodeKey"], "displayName": node["displayName"],
		"aliases": aliases, "labels": labels, "hostname": node["hostname"],
		"platform": node["platform"], "architecture": node["architecture"], "nodeMode": node["nodeMode"],
		"status": node["status"], "capabilities": capabilities,
		"appServerStatus": appServerStatus, "lastSeenAt": node["lastSeenAt"],
	}
}

func selectorValue(node map[string]any, key string) string {
	value, _ := node[key].(string)
	return value
}

func (client *cliClient) resolveNode(ctx context.Context, selector string) (map[string]any, error) {
	if selector == "" {
		return nil, fmt.Errorf("--node is required")
	}
	var resolved struct {
		Node map[string]any `json:"node"`
	}
	err := client.request(ctx, http.MethodGet, "/v1/nodes/resolve?"+url.Values{"selector": []string{selector}}.Encode(), nil, &resolved)
	if err == nil {
		return resolved.Node, nil
	}
	var httpError *cliHTTPError
	if !errors.As(err, &httpError) || httpError.Status != http.StatusNotFound {
		return nil, err
	}
	// Older Servers do not expose the resolver endpoint. Retain the original
	// list-based resolution so a newer CLI can still operate during upgrades.
	nodes, err := client.nodes(ctx)
	if err != nil {
		return nil, err
	}
	matches := []map[string]any{}
	bestRank := 5
	for _, item := range nodes {
		rank := 5
		switch {
		case selector == selectorValue(item, "nodeId"):
			rank = 1
		case selector == selectorValue(item, "nodeKey"):
			rank = 2
		case nodeHasAlias(item, selector):
			rank = 3
		case selector == selectorValue(item, "hostname"):
			rank = 4
		}
		if rank < bestRank {
			bestRank, matches = rank, []map[string]any{item}
		} else if rank == bestRank && rank < 5 {
			matches = append(matches, item)
		}
	}
	if len(matches) == 0 {
		return nil, &cliHTTPError{Status: 404, Code: "not_found", Message: "no Node matches selector " + selector}
	}
	if len(matches) > 1 {
		return nil, &cliHTTPError{Status: 409, Code: "ambiguous_selector", Message: "Node selector is ambiguous: " + selector}
	}
	return matches[0], nil
}

func nodeHasAlias(node map[string]any, selector string) bool {
	switch aliases := node["aliases"].(type) {
	case []any:
		for _, raw := range aliases {
			if alias, ok := raw.(string); ok && strings.EqualFold(alias, selector) {
				return true
			}
		}
	case []string:
		for _, alias := range aliases {
			if strings.EqualFold(alias, selector) {
				return true
			}
		}
	}
	return false
}

func (client *cliClient) invoke(ctx context.Context, selector, capability string, params map[string]any) (any, map[string]any, error) {
	node, err := client.resolveNode(ctx, selector)
	if err != nil {
		return nil, nil, err
	}
	result, err := client.invokeNode(ctx, selectorValue(node, "nodeId"), capability, params)
	return result, node, err
}

func (client *cliClient) invokeNode(ctx context.Context, nodeID, capability string, params map[string]any) (any, error) {
	var response struct {
		Result any `json:"result"`
	}
	body := map[string]any{"capability": capability, "params": params, "timeoutMs": client.options.Timeout.Milliseconds()}
	err := client.request(ctx, http.MethodPost, "/v1/nodes/"+nodeID+"/invoke", body, &response)
	return response.Result, err
}

func flagSet(name string) *flag.FlagSet {
	set := flag.NewFlagSet(name, flag.ContinueOnError)
	set.SetOutput(io.Discard)
	return set
}

func printResult(writer io.Writer, options cliOptions, value any) error {
	if options.JSON {
		return json.NewEncoder(writer).Encode(map[string]any{"schemaVersion": 1, "data": value})
	}
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(writer, string(encoded))
	return err
}

func identityView(state *persistedNodeState, identityFile string) map[string]any {
	return map[string]any{"identityFile": identityFile, "serverUrl": state.ServerURL, "nodeId": state.NodeID,
		"nodeKey": state.NodeKey, "credentialId": state.CredentialID, "credentialFingerprint": state.fingerprint(),
		"approvedAt": state.ApprovedAt, "status": state.Enrollment.Status}
}

func parseNodeOnly(name string, args []string) (string, error) {
	set := flagSet(name)
	node := set.String("node", "", "target Node")
	if err := set.Parse(args); err != nil {
		return "", err
	}
	if set.NArg() != 0 || *node == "" {
		return "", fmt.Errorf("%s requires --node", name)
	}
	return *node, nil
}

func (client *cliClient) runNodes(ctx context.Context, args []string) (any, error) {
	if len(args) == 0 || args[0] == "list" {
		listArgs := args
		if len(args) > 0 {
			listArgs = args[1:]
		}
		set := flagSet("nodes list")
		summary := set.Bool("summary", false, "return only fields used to identify and select Nodes")
		full := set.Bool("full", false, "return the complete Node records")
		capability := set.String("capability", "", "include only Nodes advertising this capability")
		online := set.Bool("online", false, "include only online Nodes")
		var labels stringList
		set.Var(&labels, "label", "include only Nodes with this key=value label; repeatable")
		if err := set.Parse(listArgs); err != nil {
			return nil, err
		}
		if set.NArg() != 0 || (*summary && *full) {
			return nil, fmt.Errorf("nodes list accepts --summary or --full, --capability <name>, repeated --label key=value, and --online")
		}
		useSummary := *summary || (!client.options.JSON && !*full)
		status := ""
		if *online {
			status = "online"
		}
		for _, label := range labels {
			parts := strings.SplitN(label, "=", 2)
			if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
				return nil, fmt.Errorf("--label must be key=value")
			}
		}
		return client.filteredNodes(ctx, useSummary, *capability, status, labels)
	}
	if len(args) >= 1 && args[0] == "get" {
		selector, err := parseNodeOnly("nodes get", args[1:])
		if err != nil {
			return nil, err
		}
		return client.resolveNode(ctx, selector)
	}
	return nil, fmt.Errorf("usage: mira nodes [list] [--summary|--full] [--capability <name>] [--label key=value] [--online] | mira nodes get --node <selector>")
}

func (client *cliClient) runFile(ctx context.Context, args []string, stdin io.Reader) (any, error) {
	if len(args) < 1 {
		return nil, fmt.Errorf("file action is required")
	}
	action := args[0]
	set := flagSet("file " + action)
	node := set.String("node", "", "target Node")
	filePath := set.String("path", "", "remote path")
	destination := set.String("destination", "", "remote destination")
	offset := set.Int64("offset", 0, "read offset")
	length := set.Int64("length", 0, "read length")
	output := set.String("output", "", "local output")
	input := set.String("input", "", "local input")
	useStdin := set.Bool("stdin", false, "read stdin")
	overwrite := set.Bool("overwrite", false, "overwrite")
	recursive := set.Bool("recursive", false, "recursive")
	if err := set.Parse(args[1:]); err != nil {
		return nil, err
	}
	params := map[string]any{"action": action}
	if *filePath != "" {
		params["path"] = *filePath
	}
	if *destination != "" {
		params["destination"] = *destination
	}
	if *offset > 0 {
		params["offset"] = *offset
	}
	if *length > 0 {
		params["length"] = *length
	}
	if *recursive {
		params["recursive"] = true
	}
	if *overwrite {
		params["overwrite"] = true
	}
	if action == "read" && *output != "" {
		params["encoding"] = "base64"
	}
	if action == "write" {
		if (*input == "") == !*useStdin {
			return nil, fmt.Errorf("file write requires exactly one of --input or --stdin")
		}
		var content []byte
		var err error
		if *useStdin {
			content, err = io.ReadAll(io.LimitReader(stdin, 4*1024*1024+1))
		} else {
			content, err = os.ReadFile(*input)
		}
		if err != nil {
			return nil, err
		}
		if len(content) > 4*1024*1024 {
			return nil, fmt.Errorf("input exceeds 4 MiB")
		}
		params["encoding"], params["content"], params["overwrite"] = "base64", base64.StdEncoding.EncodeToString(content), *overwrite
	}
	result, selected, err := client.invoke(ctx, *node, "file", params)
	if err != nil {
		return nil, err
	}
	if action == "read" && *output != "" {
		view, ok := result.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("unexpected file response")
		}
		content, err := base64.StdEncoding.DecodeString(selectorValue(view, "content"))
		if err != nil {
			return nil, err
		}
		absolute, err := filepath.Abs(*output)
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(absolute, content, 0600); err != nil {
			return nil, err
		}
		delete(view, "content")
		view["localPath"] = absolute
		view["sourceNodeId"] = selectorValue(selected, "nodeId")
	}
	return result, nil
}

func commandFlags(set *flag.FlagSet) (*string, *string, *string, *int64) {
	return set.String("node", "", "target Node"), set.String("cwd", "", "working directory"), set.String("process-id", "", "process ID"), set.Int64("cursor", 0, "output cursor")
}

func (client *cliClient) runProcess(ctx context.Context, args []string, stdout io.Writer) (any, error) {
	if len(args) < 1 {
		return nil, fmt.Errorf("process action is required")
	}
	action := args[0]
	set := flagSet("process " + action)
	node, cwd, processID, cursor := commandFlags(set)
	system := set.Bool("system", false, "list system processes")
	signal := set.String("signal", "", "signal")
	var env stringList
	set.Var(&env, "env", "KEY=VALUE")
	if err := set.Parse(args[1:]); err != nil {
		return nil, err
	}
	params := map[string]any{"action": map[string]string{"run": "start"}[action]}
	if params["action"] == "" {
		params["action"] = action
	}
	if *cwd != "" {
		params["cwd"] = *cwd
	}
	if *processID != "" {
		params["processId"] = *processID
	}
	if *cursor > 0 {
		params["cursor"] = *cursor
	}
	if *system {
		params["system"] = true
	}
	if *signal != "" {
		params["signal"] = *signal
	}
	if action == "start" || action == "run" {
		commandArgs := set.Args()
		if len(commandArgs) == 0 {
			return nil, fmt.Errorf("process %s requires -- <executable> [args...]", action)
		}
		params["command"], params["args"] = commandArgs[0], commandArgs[1:]
		values := map[string]string{}
		for _, item := range env {
			pair := strings.SplitN(item, "=", 2)
			if len(pair) != 2 {
				return nil, fmt.Errorf("--env must be KEY=VALUE")
			}
			values[pair[0]] = pair[1]
		}
		if len(values) > 0 {
			params["env"] = values
		}
	}
	result, selected, err := client.invoke(ctx, *node, "process", params)
	if err != nil || action != "run" {
		return result, err
	}
	view, ok := result.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("unexpected process response")
	}
	id := selectorValue(view, "processId")
	nextCursor := int64(0)
	deadline := time.Now().Add(client.options.Timeout)
	for {
		if output, ok := view["output"].(map[string]any); ok {
			if raw, ok := output["cursor"].(float64); ok {
				nextCursor = int64(raw)
			}
			if !client.options.JSON {
				printChunks(stdout, output)
			}
		}
		running, _ := view["running"].(bool)
		if !running {
			return view, nil
		}
		if time.Now().After(deadline) {
			return nil, &cliHTTPError{Status: 504, Code: "timeout", Message: "process run timed out; process remains managed on target Node"}
		}
		time.Sleep(250 * time.Millisecond)
		poll, err := client.invokeNode(ctx, selectorValue(selected, "nodeId"), "process", map[string]any{"action": "poll", "processId": id, "cursor": nextCursor})
		if err != nil {
			return nil, err
		}
		view, ok = poll.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("unexpected process poll response")
		}
	}
}

func printChunks(writer io.Writer, output map[string]any) {
	chunks, _ := output["chunks"].([]any)
	for _, raw := range chunks {
		if chunk, ok := raw.(map[string]any); ok {
			fmt.Fprint(writer, selectorValue(chunk, "text"))
		}
	}
}

func (client *cliClient) runPTY(ctx context.Context, args []string) (any, error) {
	if len(args) < 1 {
		return nil, fmt.Errorf("PTY action is required")
	}
	action := args[0]
	set := flagSet("pty " + action)
	node := set.String("node", "", "target Node")
	sessionID := set.String("session-id", "", "session ID")
	cwd := set.String("cwd", "", "working directory")
	input := set.String("input", "", "input text")
	cursor := set.Int64("cursor", 0, "output cursor")
	rows := set.Int("rows", 0, "rows")
	cols := set.Int("cols", 0, "columns")
	if err := set.Parse(args[1:]); err != nil {
		return nil, err
	}
	params := map[string]any{"action": action}
	if *sessionID != "" {
		params["sessionId"] = *sessionID
	}
	if *cwd != "" {
		params["cwd"] = *cwd
	}
	if *input != "" {
		params["input"] = *input
	}
	if *cursor > 0 {
		params["cursor"] = *cursor
	}
	if *rows > 0 {
		params["rows"] = *rows
	}
	if *cols > 0 {
		params["cols"] = *cols
	}
	if action == "open" {
		commandArgs := set.Args()
		if len(commandArgs) > 0 {
			params["command"], params["args"] = commandArgs[0], commandArgs[1:]
		}
	}
	result, _, err := client.invoke(ctx, *node, "pty", params)
	return result, err
}

func (client *cliClient) runScreen(ctx context.Context, args []string) (any, error) {
	if len(args) < 1 {
		return nil, fmt.Errorf("screen action is required")
	}
	action := args[0]
	set := flagSet("screen " + action)
	node := set.String("node", "", "target Node")
	output := set.String("output", "", "local screenshot path")
	x := set.Int("x", -1, "x")
	y := set.Int("y", -1, "y")
	startX := set.Int("start-x", -1, "start x")
	startY := set.Int("start-y", -1, "start y")
	endX := set.Int("end-x", -1, "end x")
	endY := set.Int("end-y", -1, "end y")
	duration := set.Int("duration-ms", 0, "duration")
	key := set.String("key-code", "", "key code")
	textValue := set.String("text", "", "text")
	if err := set.Parse(args[1:]); err != nil {
		return nil, err
	}
	params := map[string]any{"action": action}
	for name, value := range map[string]int{"x": *x, "y": *y, "startX": *startX, "startY": *startY, "endX": *endX, "endY": *endY} {
		if value >= 0 {
			params[name] = value
		}
	}
	if *duration > 0 {
		params["durationMs"] = *duration
	}
	if *key != "" {
		if number, err := strconv.Atoi(*key); err == nil {
			params["keyCode"] = number
		} else {
			params["keyCode"] = *key
		}
	}
	if *textValue != "" {
		params["text"] = *textValue
	}
	result, selected, err := client.invoke(ctx, *node, "screen", params)
	if err != nil {
		return nil, err
	}
	if action == "screenshot" {
		if *output == "" {
			return nil, fmt.Errorf("screen screenshot requires --output")
		}
		view, ok := result.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("unexpected screenshot response")
		}
		content, err := base64.StdEncoding.DecodeString(selectorValue(view, "content"))
		if err != nil {
			return nil, err
		}
		absolute, err := filepath.Abs(*output)
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(absolute, content, 0600); err != nil {
			return nil, err
		}
		delete(view, "content")
		view["localPath"] = absolute
		view["sourceNodeId"] = selectorValue(selected, "nodeId")
	}
	return result, nil
}

func (client *cliClient) runAppServer(ctx context.Context, args []string, stdin io.Reader, stdout io.Writer) (any, error) {
	if len(args) < 1 {
		return nil, fmt.Errorf("app-server action is required")
	}
	action := args[0]
	selector, err := parseNodeOnly("app-server "+action, args[1:])
	if err != nil {
		return nil, err
	}
	selected, err := client.resolveNode(ctx, selector)
	if err != nil {
		return nil, err
	}
	nodeID := selectorValue(selected, "nodeId")
	if action == "status" {
		return map[string]any{"nodeId": nodeID, "desired": selected["desiredAppServer"], "reported": selected["reportedAppServer"]}, nil
	}
	if action == "start" || action == "stop" {
		var response any
		err = client.request(ctx, http.MethodPut, "/v1/nodes/"+nodeID+"/desired-app-server", map[string]any{"running": action == "start"}, &response)
		return response, err
	}
	if action != "connect" {
		return nil, fmt.Errorf("unknown app-server action %s", action)
	}
	endpoint, err := url.Parse(client.identity.ServerURL)
	if err != nil {
		return nil, err
	}
	if endpoint.Scheme == "https" {
		endpoint.Scheme = "wss"
	} else {
		endpoint.Scheme = "ws"
	}
	endpoint.Path = "/v1/nodes/" + nodeID + "/app-server"
	endpoint.RawQuery = ""
	dialer := *websocket.DefaultDialer
	dialer.HandshakeTimeout = client.options.Timeout
	dialer.Subprotocols = []string{"mira-client-v1", "auth." + base64.RawURLEncoding.EncodeToString([]byte(client.identity.Token))}
	connection, response, err := dialer.DialContext(ctx, endpoint.String(), nil)
	if err != nil {
		if response != nil {
			return nil, &cliHTTPError{Status: response.StatusCode, Message: "App Server connection rejected"}
		}
		return nil, err
	}
	defer connection.Close()
	done := make(chan error, 1)
	go func() {
		for {
			_, payload, err := connection.ReadMessage()
			if err != nil {
				done <- err
				return
			}
			fmt.Fprintln(stdout, string(payload))
		}
	}()
	scanner := bufio.NewScanner(stdin)
	for scanner.Scan() {
		if err := connection.WriteMessage(websocket.TextMessage, scanner.Bytes()); err != nil {
			return nil, err
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	_ = connection.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-done:
		return map[string]any{"status": "closed", "nodeId": nodeID}, nil
	}
}

func (client *cliClient) runCodex(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	}
	candidates := codexCandidatePaths(os.Getenv("CODEX_BINARY"))
	if os.Getenv("CODEX_BINARY") == "" {
		store, err := newCodexRuntimeStore(client.options.Identity)
		if err != nil {
			return err
		}
		binary, err := store.ensure(ctx, stderr)
		if err != nil {
			return err
		}
		candidates = []string{binary}
	}
	codexPath := ""
	for _, candidate := range candidates {
		if supportsRemoteThreadStore(ctx, candidate) {
			codexPath = candidate
			break
		}
	}
	if codexPath == "" {
		return fmt.Errorf("selected Codex does not support Mira's remote ThreadStore; check CODEX_BINARY or mira codex-runtime status")
	}
	storeID := os.Getenv("MIRA_CODEX_STORE_ID")
	if storeID == "" {
		storeID = "personal"
	}
	remoteArgs := []string{
		"-c", `experimental_thread_store.type="remote_http"`,
		"-c", "experimental_thread_store.endpoint=" + strconv.Quote(client.identity.ServerURL),
		"-c", "experimental_thread_store.store_id=" + strconv.Quote(storeID),
		"-c", `approval_policy="never"`,
		"-c", `sandbox_mode="danger-full-access"`,
	}
	stateOverride, err := codexSQLiteOverride(client.options.Identity, codexPath, nil)
	if err != nil {
		return err
	}
	if stateOverride != "" {
		remoteArgs = append(remoteArgs, "-c", stateOverride)
	}
	command := exec.CommandContext(ctx, codexPath, append(remoteArgs, args...)...)
	command.Stdin, command.Stdout, command.Stderr = stdin, stdout, stderr
	command.Env = append(os.Environ(), "MIRA_NODE_TOKEN="+client.identity.Token, "MIRA_SERVER_URL="+client.identity.ServerURL)
	return command.Run()
}

func requestedCLIHelp(args []string) ([]string, bool) {
	if len(args) > 0 && args[0] == "help" {
		return args[1:], true
	}
	for index, arg := range args {
		if arg == "--help" || arg == "-h" {
			return args[:index], true
		}
	}
	return nil, false
}

func cliHelpText(topic []string) (string, bool) {
	if len(topic) == 0 {
		return `Mira connects trusted personal devices and runs Codex close to their workspaces.

Usage:
  mira [global options] <command> [arguments]

Commands:
  install        Install Supervisor and choose system-service ownership
  doctor         Check installation ownership, release and service drift
  repair         Repair only the declared service owner's configuration
  uninstall      Remove a Mira-owned service or show Nix removal guidance
  setup          Enroll or install this Mira Node
  status         Show this Node's local and Server connection state
  version        Show build information
  update         Check for or install a Mira update
  identity       Inspect this Node's non-secret identity metadata
  nodes          Discover Nodes and resolve selectors
  file           Inspect or change files through a Node capability
  process        Count, list, start, poll or signal managed processes
  pty            Open and operate managed terminal sessions
  screen         Inspect or control an Android display
  app-server     Inspect, start, stop or connect to Codex App Server
  codex          Run the Mira-managed Codex CLI
  codex-runtime  Inspect or prepare the pinned Codex runtime
  ssh/scp/sftp   Use native OpenSSH over Mira's relay

Global options:
  --json          Emit one stable JSON result envelope
  --timeout 30s   Bound requests between 100ms and 10m
  -h, --help      Show local help without contacting Mira Server
  --version       Show version information

Run "mira <command> --help" for command-specific help.`, true
	}
	key := topic[0]
	if len(topic) > 1 && !strings.HasPrefix(topic[1], "-") {
		key += " " + topic[1]
	}
	help := map[string]string{
		"nodes": `Discover trusted Nodes and use stable selectors.

Usage:
  mira nodes [list] [--summary|--full] [--capability <name>] [--label key=value] [--online] [--json]
  mira nodes get --node <UUID|nodeKey|alias|hostname> [--json]

The list command prints a compact table for people. --json retains the full
versioned record for compatibility; combine --summary --json for Agent use.
Aliases are exact, user-defined selectors. Hostnames must resolve uniquely.`,
		"nodes list": `Usage: mira nodes [list] [options]

Options:
  --summary             Return identification, aliases, status and capabilities
  --full                Return complete registry records
  --capability <name>   Include only Nodes advertising a capability such as ssh
  --label key=value     Include only Nodes with this label; may be repeated
  --online              Include only currently online Nodes
  --json                Emit a versioned JSON envelope`,
		"nodes get": `Usage: mira nodes get --node <selector> [--json]

The selector may be a Node UUID, exact nodeKey, user-defined alias, or a unique hostname.`,
		"file": `Usage: mira file <roots|stat|list|read|write|mkdir|move|remove> --node <selector> [options]

Common options: --path, --destination, --offset, --length, --recursive, --overwrite.
Use --output for local file reads. Writes require exactly one of --input or --stdin.`,
		"process": `Usage: mira process <count|list|start|run|poll|signal> --node <selector> [options] [-- executable args...]

Options include --cwd, --process-id, --cursor, --signal, --system and repeated --env KEY=VALUE.
run waits and streams output; start returns a managed process ID.`,
		"pty": `Usage: mira pty <list|open|poll|write|resize|close> --node <selector> [options] [-- executable args...]

Options include --session-id, --cwd, --input, --cursor, --rows and --cols.`,
		"screen": `Usage: mira screen <display|screenshot|hierarchy|tap|swipe|key|text> --node <selector> [options]

Screenshots require --output. Input actions use --x/--y, --start-x/--start-y,
--end-x/--end-y, --duration-ms, --key-code or --text.`,
		"app-server": `Usage: mira app-server <status|start|stop|connect> --node <selector>`,
		"identity": `Usage: mira identity show [--json]

Shows non-secret identity metadata. The Node credential itself is never printed.`,
		"setup": `Usage: mira setup [options]

Configure or enroll this machine. Run this command's platform-specific setup before starting mira node-worker.`,
		"install": `Usage: mira install [--role node|server] [--service-owner nix|mira] [--service-manager auto|systemd|procd] [--state-dir DIR] [--server-url URL] [--dry-run]

On NixOS, an interactive install asks who owns the system service. Non-interactive
installs must pass --service-owner explicitly. Nix and Mira ownership are mutually exclusive.`,
		"doctor":    `Usage: mira doctor [--state-dir DIR]`,
		"repair":    `Usage: mira repair [--state-dir DIR] [--dry-run]`,
		"uninstall": `Usage: mira uninstall [--state-dir DIR] [--dry-run]`,
		"status":    `Usage: mira status [--json]`,
		"version":   `Usage: mira version [--json]`,
		"update": `Usage: mira update [--check] [--version VERSION] [--state-dir DIR] [--no-wait] [--json]

The installed executable discovers its own state directory. --state-dir is
available for diagnostics or nonstandard launchers. The old local Supervisor
continues an accepted update if this CLI, SSH session, Node, or Server exits.`,
		"codex": `Usage: mira codex [-- Codex arguments...]

Runs the compatible pinned Codex runtime with Mira's PostgreSQL ThreadStore.`,
		"codex-runtime": `Usage: mira codex-runtime <status|prepare> [--json]`,
		"ssh":           `Usage: mira ssh [OpenSSH options] <node-id|nodeKey|alias> [-- command...]`,
		"scp": `Usage: mira scp [OpenSSH options] <source> <destination>

Remote operands use <node-selector>::<absolute-path>.`,
		"sftp": `Usage: mira sftp [OpenSSH options] <node-id|nodeKey|alias>`,
	}
	value, ok := help[key]
	if !ok && len(topic) > 1 {
		value, ok = help[topic[0]]
	}
	return value, ok
}

func nodeStringValues(node map[string]any, name string) []string {
	result := []string{}
	switch values := node[name].(type) {
	case []any:
		for _, raw := range values {
			if value, ok := raw.(string); ok {
				result = append(result, value)
			}
		}
	case []string:
		result = append(result, values...)
	}
	return result
}

func nodeDisplayName(node map[string]any) string {
	if value := selectorValue(node, "displayName"); value != "" {
		return value
	}
	return selectorValue(node, "hostname")
}

func nodeAppServerStatus(node map[string]any) string {
	if value := selectorValue(node, "appServerStatus"); value != "" {
		return value
	}
	if reported, ok := node["reportedAppServer"].(map[string]any); ok {
		if value := selectorValue(reported, "status"); value != "" {
			return value
		}
	}
	return "unsupported"
}

func printNodesHuman(writer io.Writer, value any, get bool) error {
	if get {
		node, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("unexpected Node response")
		}
		fmt.Fprintf(writer, "Name:       %s\nHostname:   %s\nAliases:    %s\nNode key:   %s\nNode ID:    %s\nStatus:     %s\nPlatform:   %s/%s (%s)\nApp Server: %s\n",
			nodeDisplayName(node), selectorValue(node, "hostname"), strings.Join(nodeStringValues(node, "aliases"), ", "),
			selectorValue(node, "nodeKey"), selectorValue(node, "nodeId"), selectorValue(node, "status"),
			selectorValue(node, "platform"), selectorValue(node, "architecture"), selectorValue(node, "nodeMode"), nodeAppServerStatus(node))
		if labels, ok := node["labels"].(map[string]any); ok && len(labels) > 0 {
			keys := make([]string, 0, len(labels))
			for key := range labels {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			parts := make([]string, 0, len(keys))
			for _, key := range keys {
				parts = append(parts, key+"="+fmt.Sprint(labels[key]))
			}
			fmt.Fprintf(writer, "Labels:     %s\n", strings.Join(parts, ", "))
		}
		return nil
	}
	nodes, ok := value.([]map[string]any)
	if !ok {
		return fmt.Errorf("unexpected Node list response")
	}
	table := tabwriter.NewWriter(writer, 0, 4, 2, ' ', 0)
	fmt.Fprintln(table, "NAME\tALIASES\tSTATUS\tPLATFORM\tMODE\tAPP SERVER\tNODE KEY")
	for _, node := range nodes {
		fmt.Fprintf(table, "%s\t%s\t%s\t%s/%s\t%s\t%s\t%s\n",
			nodeDisplayName(node), strings.Join(nodeStringValues(node, "aliases"), ","), selectorValue(node, "status"),
			selectorValue(node, "platform"), selectorValue(node, "architecture"), selectorValue(node, "nodeMode"),
			nodeAppServerStatus(node), selectorValue(node, "nodeKey"))
	}
	return table.Flush()
}

func cliUsage() string {
	return "usage: mira [--json] [--timeout 30s] <install|doctor|repair|uninstall|setup|status|version|update|identity|nodes|file|process|pty|screen|app-server|codex|codex-runtime|ssh|scp|sftp> ..."
}

func cliExitCode(err error) int {
	var httpError *cliHTTPError
	if errors.As(err, &httpError) {
		switch httpError.Status {
		case 428:
			return 2
		case 401, 403:
			return 3
		case 404:
			return 4
		case 409:
			return 5
		case 503:
			return 6
		case 504:
			return 7
		}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return 7
	}
	return 1
}

func printCLIError(writer io.Writer, options cliOptions, err error) {
	if !options.JSON {
		fmt.Fprintln(writer, err)
		return
	}
	code, status := "command_failed", 0
	var httpError *cliHTTPError
	if errors.As(err, &httpError) {
		code, status = httpError.Code, httpError.Status
	}
	_ = json.NewEncoder(writer).Encode(map[string]any{"schemaVersion": 1, "error": map[string]any{"code": code, "message": err.Error(), "httpStatus": status}})
}

func RunCLI(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	options, remaining, err := parseGlobalCLI(args)
	if err != nil {
		fmt.Fprintln(stderr, err)
		fmt.Fprintln(stderr, cliUsage())
		return 64
	}
	if topic, requested := requestedCLIHelp(remaining); requested {
		help, ok := cliHelpText(topic)
		if !ok {
			fmt.Fprintf(stderr, "unknown help topic %s\n", strings.Join(topic, " "))
			return 64
		}
		fmt.Fprintln(stdout, help)
		return 0
	}
	if len(remaining) == 0 {
		printCLIError(stderr, options, fmt.Errorf("command is required"))
		fmt.Fprintln(stderr, cliUsage())
		return 64
	}
	if remaining[0] == "version" && len(remaining) == 1 {
		if !options.JSON {
			fmt.Fprintf(stdout, "mira %s (%s, %s/%s)\n", Version, Commit, CurrentBuild().Platform, CurrentBuild().Architecture)
			return 0
		}
		value := map[string]any{"program": "mira", "build": CurrentBuild()}
		if err := printResult(stdout, options, value); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		return 0
	}
	if remaining[0] == "install" || remaining[0] == "doctor" || remaining[0] == "repair" || remaining[0] == "uninstall" || remaining[0] == "setup" || remaining[0] == "status" || remaining[0] == "update" || remaining[0] == "codex-runtime" {
		var value any
		var localErr error
		switch remaining[0] {
		case "install":
			value, localErr = runSystemInstall(ctx, remaining[1:], stdin, stdout)
		case "doctor":
			value, localErr = runDoctor(remaining[1:])
		case "repair":
			value, localErr = runRepair(ctx, remaining[1:])
		case "uninstall":
			value, localErr = runUninstall(ctx, remaining[1:])
		case "setup":
			value, localErr = runSetup(remaining[1:])
		case "status":
			value, localErr = localStatus(ctx, options)
		case "update":
			value, localErr = runUpdate(ctx, options, remaining[1:], stdin, stdout, stderr)
		case "codex-runtime":
			value, localErr = runCodexRuntime(ctx, options, remaining[1:], stderr)
		}
		if localErr != nil {
			printCLIError(stderr, options, localErr)
			return cliExitCode(localErr)
		}
		if !options.JSON && remaining[0] == "status" {
			view, _ := value.(map[string]any)
			fmt.Fprintf(stdout, "Mira %s\nServer: %v\nEnrollment: %v\n", Version, view["serverUrl"], view["status"])
			if code, ok := view["verificationCode"].(string); ok && code != "" {
				fmt.Fprintf(stdout, "Verification code: %s\n", code)
			}
			if state, ok := view["connectionStatus"]; ok {
				fmt.Fprintf(stdout, "Connection: %v\n", state)
			}
			if hint, ok := view["hint"]; ok {
				fmt.Fprintln(stdout, hint)
			}
			if connectionError, ok := view["connectionError"]; ok {
				fmt.Fprintf(stdout, "Server error: %v\n", connectionError)
			}
			return 0
		}
		if value != nil {
			if err := printResult(stdout, options, value); err != nil {
				fmt.Fprintln(stderr, err)
				return 1
			}
		}
		return 0
	}
	if remaining[0] == "identity" && len(remaining) == 2 && remaining[1] == "show" {
		state, err := loadIdentity(options.Identity)
		if err != nil {
			if os.IsNotExist(err) {
				err = &cliHTTPError{Status: 428, Code: "not_enrolled", Message: "This machine is not enrolled in Mira. Start mira node-worker to submit an enrollment request"}
			}
			printCLIError(stderr, options, err)
			return cliExitCode(err)
		}
		if err := printResult(stdout, options, identityView(state, options.Identity)); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		return 0
	}
	client, err := newCLIClient(options)
	if err != nil {
		printCLIError(stderr, options, err)
		return cliExitCode(err)
	}
	if remaining[0] == "codex" {
		if options.JSON {
			printCLIError(stderr, options, fmt.Errorf("--json is not supported by the interactive Codex wrapper"))
			return 64
		}
		if err := client.runCodex(ctx, remaining[1:], stdin, stdout, stderr); err != nil {
			var exitError *exec.ExitError
			if errors.As(err, &exitError) {
				return exitError.ExitCode()
			}
			fmt.Fprintln(stderr, err)
			return 1
		}
		return 0
	}
	if remaining[0] == "ssh-proxy" {
		if err := client.runSSHProxy(ctx, remaining[1:], stdin, stdout); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		return 0
	}
	if remaining[0] == "ssh" || remaining[0] == "scp" || remaining[0] == "sftp" {
		if options.JSON {
			fmt.Fprintln(stderr, "SSH commands use native streams, not --json")
			return 64
		}
		if err := client.runSSHCommands(ctx, remaining[0], remaining[1:], stdin, stdout, stderr); err != nil {
			var localExit *exec.ExitError
			if errors.As(err, &localExit) {
				return localExit.ExitCode()
			}
			fmt.Fprintln(stderr, err)
			return 1
		}
		return 0
	}
	commandCtx, cancel := context.WithTimeout(ctx, options.Timeout)
	defer cancel()
	var result any
	switch remaining[0] {
	case "nodes":
		result, err = client.runNodes(commandCtx, remaining[1:])
	case "file":
		result, err = client.runFile(commandCtx, remaining[1:], stdin)
	case "process":
		result, err = client.runProcess(commandCtx, remaining[1:], stdout)
	case "pty":
		result, err = client.runPTY(commandCtx, remaining[1:])
	case "screen":
		result, err = client.runScreen(commandCtx, remaining[1:])
	case "app-server":
		result, err = client.runAppServer(commandCtx, remaining[1:], stdin, stdout)
	default:
		err = fmt.Errorf("unknown command %s", remaining[0])
	}
	if err != nil {
		printCLIError(stderr, options, err)
		return cliExitCode(err)
	}
	if result != nil {
		if remaining[0] == "nodes" && !options.JSON {
			if err := printNodesHuman(stdout, result, len(remaining) > 1 && remaining[1] == "get"); err != nil {
				fmt.Fprintln(stderr, err)
				return 1
			}
			return 0
		}
		if err := printResult(stdout, options, result); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
	}
	return 0
}
