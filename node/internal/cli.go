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
	"strconv"
	"strings"
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
			return nil, &cliHTTPError{Status: 428, Code: "not_enrolled", Message: "This machine is not enrolled in Mira. Start mira-node to submit an enrollment request"}
		}
		return nil, err
	}
	if identity.NodeID == "" || identity.Enrollment.Status != "approved" {
		return nil, &cliHTTPError{Status: 428, Code: "not_enrolled", Message: "This machine is not enrolled in Mira. Start mira-node and wait for administrator approval"}
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
	var response struct {
		Data []map[string]any `json:"data"`
	}
	err := client.request(ctx, http.MethodGet, "/v1/nodes", nil, &response)
	return response.Data, err
}

func selectorValue(node map[string]any, key string) string {
	value, _ := node[key].(string)
	return value
}

func (client *cliClient) resolveNode(ctx context.Context, selector string) (map[string]any, error) {
	if selector == "" {
		return nil, fmt.Errorf("--node is required")
	}
	nodes, err := client.nodes(ctx)
	if err != nil {
		return nil, err
	}
	matches := []map[string]any{}
	for _, item := range nodes {
		if selector == selectorValue(item, "nodeId") || selector == selectorValue(item, "nodeKey") || selector == selectorValue(item, "hostname") {
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

func (client *cliClient) invoke(ctx context.Context, selector, capability string, params map[string]any) (any, map[string]any, error) {
	node, err := client.resolveNode(ctx, selector)
	if err != nil {
		return nil, nil, err
	}
	var response struct {
		Result any `json:"result"`
	}
	body := map[string]any{"capability": capability, "params": params, "timeoutMs": client.options.Timeout.Milliseconds()}
	err = client.request(ctx, http.MethodPost, "/v1/nodes/"+selectorValue(node, "nodeId")+"/invoke", body, &response)
	return response.Result, node, err
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
	if len(args) == 1 && args[0] == "list" {
		return client.nodes(ctx)
	}
	if len(args) >= 1 && args[0] == "get" {
		selector, err := parseNodeOnly("nodes get", args[1:])
		if err != nil {
			return nil, err
		}
		return client.resolveNode(ctx, selector)
	}
	return nil, fmt.Errorf("usage: mira nodes list | mira nodes get --node <selector>")
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
		poll, _, err := client.invoke(ctx, selectorValue(selected, "nodeId"), "process", map[string]any{"action": "poll", "processId": id, "cursor": nextCursor})
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

func cliUsage() string {
	return "usage: mira [--json] [--timeout 30s] <setup|status|version|update|identity|nodes|file|process|pty|screen|app-server|codex|codex-runtime|ssh|scp|sftp> ..."
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
	if remaining[0] == "setup" || remaining[0] == "status" || remaining[0] == "update" || remaining[0] == "codex-runtime" {
		var value any
		var localErr error
		switch remaining[0] {
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
				err = &cliHTTPError{Status: 428, Code: "not_enrolled", Message: "This machine is not enrolled in Mira. Start mira-node to submit an enrollment request"}
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
		if err := printResult(stdout, options, result); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
	}
	return 0
}
