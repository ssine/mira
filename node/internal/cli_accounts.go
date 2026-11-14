package node

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ssine/mira/node/internal/accountegress"
	"github.com/ssine/mira/node/internal/clauderuntime"
)

// Interactive account launchers. `mira codex` and `mira claude` read only
// Node-local account files, so they keep working while Mira Server is down.
// Codex sessions use the PostgreSQL ThreadStore when the Server is reachable
// and the account home's local rollouts otherwise; Claude sessions always use
// the account's native local transcripts.

const localCodexHomeSelector = "local"

type localAccountChoice struct {
	Engine   string                        `json:"engine"`
	Selector string                        `json:"selector"`
	ID       string                        `json:"nodeAccountId,omitempty"`
	Name     string                        `json:"name,omitempty"`
	Detail   string                        `json:"detail"`
	Egress   *accountegress.PublicSpec     `json:"egress,omitempty"`
	Usable   bool                          `json:"usable"`
	Problem  string                        `json:"problem,omitempty"`
	Home     string                        `json:"home"`
	codex    *localCodexAccount            `json:"-"`
	claude   *clauderuntime.OfflineAccount `json:"-"`
}

func (choice localAccountChoice) label() string {
	if choice.Name != "" {
		return choice.Name
	}
	if choice.claude != nil && choice.claude.Email != "" {
		return choice.claude.Email
	}
	if choice.ID == "" {
		return "Node Codex home"
	}
	return choice.Engine + " " + choice.ID[:8]
}

func identityDirectory(identityFile string) (string, error) {
	if identityFile == "" {
		var err error
		if identityFile, err = DefaultIdentityFile(); err != nil {
			return "", err
		}
	}
	if !filepath.IsAbs(identityFile) {
		return "", fmt.Errorf("Node identity path must be absolute")
	}
	return filepath.Dir(identityFile), nil
}

func egressDetail(spec *accountegress.PublicSpec) string {
	if spec == nil {
		return "direct"
	}
	return fmt.Sprintf("%s %s:%d", spec.Type, spec.Server, spec.Port)
}

func listLocalCodexAccounts(identityFile string, egress *accountegress.Manager) ([]localAccountChoice, error) {
	dir, err := identityDirectory(identityFile)
	if err != nil {
		return nil, err
	}
	index, indexErr := readLocalAccountIndex(identityFile)
	if indexErr != nil && !os.IsNotExist(indexErr) {
		return nil, fmt.Errorf("cannot read local account index: %w", indexErr)
	}
	accounts := append([]localCodexAccount(nil), index.Codex...)
	known := map[string]bool{}
	for _, account := range accounts {
		known[strings.ToLower(account.NodeAccountID)] = true
	}
	// Accounts created before this Node recorded an index still have a home.
	entries, _ := os.ReadDir(filepath.Join(dir, "accounts"))
	for _, entry := range entries {
		id := strings.ToLower(entry.Name())
		if !entry.IsDir() || !nodeUUIDPattern.MatchString(id) || known[id] {
			continue
		}
		home := filepath.Join(dir, "accounts", entry.Name(), "codex")
		if info, err := os.Stat(home); err == nil && info.IsDir() {
			accounts = append(accounts, localCodexAccount{NodeAccountID: id, Enabled: true, CodexHome: home, ConfigOverrides: []string{codexDefaultSubagentOverride}})
		}
	}
	if len(accounts) == 0 {
		// Never enrolled or never reconciled: offer the ambient Codex home.
		home, err := effectiveAccountHome("")
		if err == nil {
			accounts = append(accounts, localCodexAccount{Enabled: true, CodexHome: home, ConfigOverrides: []string{codexDefaultSubagentOverride}, Legacy: true})
		}
	}
	result := make([]localAccountChoice, 0, len(accounts))
	for i := range accounts {
		account := &accounts[i]
		choice := localAccountChoice{Engine: "codex", ID: account.NodeAccountID, Name: account.Name, Home: account.CodexHome, Usable: true, codex: account}
		choice.Selector = account.NodeAccountID
		if choice.Selector == "" {
			choice.Selector = localCodexHomeSelector
		}
		credential := "not logged in"
		profile := managedAccountProfile{}
		if account.NodeAccountID != "" {
			profile, _ = readManagedAccountProfile(config{IdentityFile: identityFile, CodexAccountID: account.NodeAccountID})
		}
		if profile.Provider.ID != "" {
			credential = "API " + firstNonEmpty(profile.Provider.Name, profile.Provider.ID)
		} else if _, err := os.Stat(filepath.Join(account.CodexHome, "auth.json")); err == nil {
			credential = "ChatGPT/API login"
		}
		if account.NodeAccountID != "" {
			if spec, err := egress.PublicConfig(account.NodeAccountID); err == nil {
				choice.Egress = spec
			}
		}
		choice.Detail = credential + ", " + egressDetail(choice.Egress)
		if !account.Enabled {
			choice.Usable, choice.Problem = false, "disabled in Mira"
		}
		result = append(result, choice)
	}
	return sortedChoices(result), nil
}

func listLocalClaudeAccounts(identityFile string) ([]localAccountChoice, error) {
	dir, err := identityDirectory(identityFile)
	if err != nil {
		return nil, err
	}
	accounts, err := clauderuntime.ListOfflineAccounts(dir)
	if err != nil {
		return nil, err
	}
	labels := map[string]localClaudeLabel{}
	if index, err := readLocalAccountIndex(identityFile); err == nil {
		for _, label := range index.Claude {
			labels[label.NodeAccountID] = label
		}
	}
	result := make([]localAccountChoice, 0, len(accounts))
	for i := range accounts {
		account := &accounts[i]
		label, labelled := labels[account.NodeAccountID]
		choice := localAccountChoice{Engine: "claude", Selector: account.NodeAccountID, ID: account.NodeAccountID, Name: label.Name, Home: account.ConfigDir, Egress: account.Egress, Usable: account.Error == "" && account.Configured, claude: account}
		auth := "API key"
		if account.AuthType == "subscription" {
			auth = "subscription"
		}
		if account.Provider.ID == "bedrock" {
			auth = "Bedrock"
		}
		if account.Email != "" && choice.Name != "" {
			auth += " " + account.Email
		}
		choice.Detail = fmt.Sprintf("%s, %s, %s", auth, account.Provider.Model, egressDetail(account.Egress))
		switch {
		case account.Error != "":
			choice.Problem = account.Error
		case !account.Configured && account.AuthType == "subscription":
			choice.Problem = "not logged in"
		case !account.Configured:
			choice.Problem = "no API key"
		case labelled && !label.Enabled:
			choice.Usable, choice.Problem = false, "disabled in Mira"
		}
		result = append(result, choice)
	}
	return sortedChoices(result), nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

// matchAccount resolves an explicit selector: full UUID, unique UUID prefix,
// "local", or a case-insensitive exact name or login email.
func matchAccount(choices []localAccountChoice, selector string) (localAccountChoice, error) {
	selector = strings.TrimSpace(selector)
	lower := strings.ToLower(selector)
	var matches []localAccountChoice
	for _, choice := range choices {
		if choice.Selector == lower || strings.EqualFold(choice.Name, selector) || (choice.claude != nil && choice.claude.Email != "" && strings.EqualFold(choice.claude.Email, selector)) {
			return choice, nil
		}
	}
	if len(lower) >= 4 {
		for _, choice := range choices {
			if choice.ID != "" && strings.HasPrefix(choice.ID, lower) {
				matches = append(matches, choice)
			}
		}
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	if len(matches) > 1 {
		return localAccountChoice{}, fmt.Errorf("account selector %q is ambiguous", selector)
	}
	return localAccountChoice{}, fmt.Errorf("no local account matches %q; run mira accounts", selector)
}

type recentAccounts map[string]string

func recentAccountsPath(identityFile string) (string, error) {
	dir, err := identityDirectory(identityFile)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "accounts", "cli-recent.json"), nil
}

func readRecentAccounts(identityFile string) recentAccounts {
	values := recentAccounts{}
	if path, err := recentAccountsPath(identityFile); err == nil {
		if data, err := readAccountFile(path); err == nil {
			_ = json.Unmarshal(data, &values)
		}
	}
	return values
}

func rememberRecentAccount(identityFile, engine, selector string) {
	values := readRecentAccounts(identityFile)
	if values[engine] == selector {
		return
	}
	values[engine] = selector
	if path, err := recentAccountsPath(identityFile); err == nil {
		if data, err := json.Marshal(values); err == nil {
			_ = writeProtectedAccountFile(path, append(data, '\n'))
		}
	}
}

func isTerminal(value any) bool {
	file, ok := value.(*os.File)
	if !ok {
		return false
	}
	info, err := file.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

func printAccountTable(out io.Writer, choices []localAccountChoice, numbered bool, recent string) {
	for i, choice := range choices {
		marker := " "
		if choice.Selector == recent {
			marker = "*"
		}
		prefix := ""
		if numbered {
			prefix = fmt.Sprintf("%s%2d) ", marker, i+1)
		}
		status := ""
		if choice.Problem != "" {
			status = "  [" + choice.Problem + "]"
		}
		fmt.Fprintf(out, "  %s%-28s %-36s %s%s\n", prefix, choice.label(), choice.Selector, choice.Detail, status)
	}
}

// chooseAccount applies an explicit selector, a single usable account, or an
// interactive menu whose Enter key repeats the most recent choice.
func chooseAccount(engine, selector, identityFile string, choices []localAccountChoice, stdin io.Reader, stderr io.Writer) (localAccountChoice, error) {
	if len(choices) == 0 {
		return localAccountChoice{}, fmt.Errorf("no %s accounts are configured on this Node; add one in the Mira console first", engine)
	}
	if selector != "" {
		choice, err := matchAccount(choices, selector)
		if err != nil {
			return choice, err
		}
		if !choice.Usable {
			return choice, fmt.Errorf("account %s is unusable: %s", choice.label(), choice.Problem)
		}
		return choice, nil
	}
	usable := []localAccountChoice{}
	for _, choice := range choices {
		if choice.Usable {
			usable = append(usable, choice)
		}
	}
	if len(usable) == 1 && len(choices) == 1 {
		return usable[0], nil
	}
	if !isTerminal(stdin) {
		var builder strings.Builder
		printAccountTable(&builder, choices, false, "")
		return localAccountChoice{}, fmt.Errorf("choose a %s account with --account:\n%s", engine, strings.TrimRight(builder.String(), "\n"))
	}
	recent := readRecentAccounts(identityFile)[engine]
	defaultIndex := -1
	for i, choice := range choices {
		if choice.Selector == recent && choice.Usable {
			defaultIndex = i
		}
	}
	if defaultIndex < 0 && len(usable) > 0 {
		for i, choice := range choices {
			if choice.Usable {
				defaultIndex = i
				break
			}
		}
		recent = ""
	}
	fmt.Fprintf(stderr, "Select a %s account:\n", engine)
	printAccountTable(stderr, choices, true, recent)
	reader := bufio.NewReader(stdin)
	for attempt := 0; attempt < 5; attempt++ {
		if defaultIndex >= 0 {
			fmt.Fprintf(stderr, "Account [%d]: ", defaultIndex+1)
		} else {
			fmt.Fprint(stderr, "Account: ")
		}
		line, err := reader.ReadString('\n')
		line = strings.TrimSpace(line)
		if err != nil && line == "" {
			return localAccountChoice{}, errors.New("no account selected")
		}
		var choice localAccountChoice
		if line == "" && defaultIndex >= 0 {
			choice = choices[defaultIndex]
		} else if number, convErr := strconv.Atoi(line); convErr == nil && number >= 1 && number <= len(choices) {
			choice = choices[number-1]
		} else if matched, matchErr := matchAccount(choices, line); matchErr == nil {
			choice = matched
		} else {
			fmt.Fprintln(stderr, matchErr)
			continue
		}
		if !choice.Usable {
			fmt.Fprintf(stderr, "%s is unusable: %s\n", choice.label(), choice.Problem)
			continue
		}
		return choice, nil
	}
	return localAccountChoice{}, errors.New("no account selected")
}

type launcherOptions struct {
	account string
	mode    string // auto, online or offline (Codex only)
	args    []string
}

// parseLauncherArgs consumes Mira options before the first engine argument.
// Everything after "--" or the first unknown argument belongs to the engine.
func parseLauncherArgs(args []string, allowMode bool) (launcherOptions, error) {
	options := launcherOptions{mode: "auto"}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--":
			options.args = args[i+1:]
			return options, nil
		case arg == "--account" || arg == "-a":
			if i+1 >= len(args) || args[i+1] == "" {
				return options, fmt.Errorf("%s requires an account selector", arg)
			}
			i++
			options.account = args[i]
		case strings.HasPrefix(arg, "--account="):
			options.account = strings.TrimPrefix(arg, "--account=")
		case allowMode && (arg == "--offline" || arg == "--online"):
			options.mode = strings.TrimPrefix(arg, "--")
		default:
			options.args = args[i:]
			return options, nil
		}
	}
	return options, nil
}

func runLocalAccounts(options cliOptions, args []string) (any, error) {
	if len(args) > 0 && args[0] != "list" || len(args) > 1 {
		return nil, fmt.Errorf("usage: mira accounts [list]")
	}
	dir, err := identityDirectory(options.Identity)
	if err != nil {
		return nil, err
	}
	codex, err := listLocalCodexAccounts(options.Identity, accountegress.New(dir))
	if err != nil {
		return nil, err
	}
	claude, err := listLocalClaudeAccounts(options.Identity)
	if err != nil {
		return nil, err
	}
	return map[string]any{"codex": codex, "claude": claude}, nil
}

func printLocalAccounts(out io.Writer, value any) {
	view, _ := value.(map[string]any)
	for _, engine := range []string{"codex", "claude"} {
		choices, _ := view[engine].([]localAccountChoice)
		fmt.Fprintf(out, "%s accounts:\n", map[string]string{"codex": "Codex", "claude": "Claude"}[engine])
		if len(choices) == 0 {
			fmt.Fprintln(out, "  (none)")
		}
		printAccountTable(out, choices, false, "")
	}
}

// singBoxBinary finds the egress gateway without the Node worker environment:
// MIRA_NODE_SING_BOX, then the Supervisor's environment file, then PATH.
func singBoxBinary() string {
	if value := os.Getenv("MIRA_NODE_SING_BOX"); value != "" {
		return value
	}
	if stateDir, err := defaultSupervisorStateDir(); err == nil {
		if data, err := readAccountFile(filepath.Join(stateDir, "mira.env")); err == nil {
			for _, line := range strings.Split(string(data), "\n") {
				key, value, ok := strings.Cut(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "export ")), "=")
				if ok && strings.TrimSpace(key) == "MIRA_NODE_SING_BOX" {
					value = strings.Trim(strings.TrimSpace(value), `"'`)
					if filepath.IsAbs(value) {
						return value
					}
				}
			}
		}
	}
	return ""
}

func runChild(command *exec.Cmd) error {
	// The terminal delivers Ctrl-C to the whole foreground process group. Mira
	// ignores it through its signal context and waits for the engine to decide.
	return command.Run()
}

func runClaudeLauncher(ctx context.Context, options cliOptions, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if runtime.GOOS == "android" {
		return errors.New("Claude Code is unavailable on Android Nodes")
	}
	launch, err := parseLauncherArgs(args, false)
	if err != nil {
		return err
	}
	dir, err := identityDirectory(options.Identity)
	if err != nil {
		return err
	}
	choices, err := listLocalClaudeAccounts(options.Identity)
	if err != nil {
		return err
	}
	choice, err := chooseAccount("Claude", launch.account, options.Identity, choices, stdin, stderr)
	if err != nil {
		return err
	}
	binary, err := clauderuntime.LocalExecutable(dir)
	if err != nil {
		return err
	}
	manager := clauderuntime.NewWithEgress(dir, accountegress.NewOffline(dir, singBoxBinary()))
	session, err := manager.OfflineSession(choice.ID, launch.args)
	if err != nil {
		return err
	}
	defer session.Release()
	rememberRecentAccount(options.Identity, "Claude", choice.Selector)
	fmt.Fprintf(stderr, "mira: Claude account %s (%s); local transcripts only\n", choice.label(), choice.Detail)
	command := exec.Command(binary, append(session.Arguments, launch.args...)...)
	command.Env = session.Environment
	command.Stdin, command.Stdout, command.Stderr = stdin, stdout, stderr
	return runChild(command)
}

// serverReachable bounds the online probe so a dead Server never delays the
// offline fallback by more than a few seconds.
func serverReachable(ctx context.Context, client *cliClient) bool {
	probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(probeCtx, http.MethodGet, client.identity.ServerURL+"/healthz", nil)
	if err != nil {
		return false
	}
	response, err := (&http.Client{Timeout: 3 * time.Second}).Do(request)
	if err != nil {
		return false
	}
	response.Body.Close()
	return response.StatusCode == http.StatusOK
}

type codexLaunch struct {
	account localCodexAccount
	// client is nil for an offline session using the account's local rollouts.
	client       *cliClient
	identityFile string
	binary       string
	egress       *accountegress.Manager
}

func runCodexLauncher(ctx context.Context, options cliOptions, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	launch, err := parseLauncherArgs(args, true)
	if err != nil {
		return err
	}
	dir, err := identityDirectory(options.Identity)
	if err != nil {
		return err
	}
	egress := accountegress.NewOffline(dir, singBoxBinary())
	choices, err := listLocalCodexAccounts(options.Identity, egress)
	if err != nil {
		return err
	}
	choice, err := chooseAccount("Codex", launch.account, options.Identity, choices, stdin, stderr)
	if err != nil {
		return err
	}
	var client *cliClient
	if launch.mode != "offline" {
		client, err = newCLIClient(options)
		if err == nil && !serverReachable(ctx, client) {
			err = errors.New("Mira Server is unreachable")
		}
		if err != nil {
			if launch.mode == "online" {
				return err
			}
			client = nil
			fmt.Fprintf(stderr, "mira: %v; starting offline, conversations stay in %s\n", err, filepath.Join(choice.Home, "sessions"))
		}
	}
	rememberRecentAccount(options.Identity, "Codex", choice.Selector)
	mode := "Mira ThreadStore"
	if client == nil {
		mode = "offline"
	}
	fmt.Fprintf(stderr, "mira: Codex account %s (%s); %s\n", choice.label(), choice.Detail, mode)
	return runCodexAccount(ctx, codexLaunch{account: *choice.codex, client: client, identityFile: options.Identity, binary: choice.codex.CodexPath, egress: egress}, launch.args, stdin, stdout, stderr)
}

func runCodexAccount(ctx context.Context, launch codexLaunch, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	account := launch.account
	binary := launch.binary
	if binary == "" {
		binary = os.Getenv("CODEX_BINARY")
	}
	candidates := codexCandidatePaths(binary)
	if binary == "" {
		store, err := newCodexRuntimeStore(launch.identityFile)
		if err != nil {
			return err
		}
		prepared, err := store.ensure(ctx, stderr)
		if err != nil {
			return err
		}
		candidates = []string{prepared}
	}
	codexPath, compatible := "", false
	for _, candidate := range candidates {
		if supportsRemoteThreadStore(ctx, candidate) {
			codexPath, compatible = candidate, true
			break
		}
	}
	if codexPath == "" {
		if launch.client != nil || len(candidates) == 0 {
			return fmt.Errorf("selected Codex does not support Mira's remote ThreadStore; check CODEX_BINARY or mira codex-runtime status")
		}
		codexPath = candidates[0] // Offline sessions accept an ordinary Codex.
	}
	configuration := config{IdentityFile: launch.identityFile, CodexAccountID: account.NodeAccountID}
	if account.Legacy {
		configuration.CodexAccountID = ""
	}
	managed, err := managedAccountOverrides(configuration)
	if err != nil {
		return err
	}
	overrides := append(append([]string(nil), account.ConfigOverrides...), managed...)
	environment, _, err := prepareAccountEnvironment(configuration, desiredAppServer{
		CodexHome: account.CodexHome, ConfigOverrides: overrides,
		EnvironmentFiles: account.EnvironmentFiles, InheritEnv: account.InheritEnv,
	})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(account.CodexHome, 0700); err != nil {
		return fmt.Errorf("create Codex home: %w", err)
	}
	if account.NodeAccountID != "" && launch.egress != nil {
		values, release, err := launch.egress.Acquire(account.NodeAccountID)
		if err != nil {
			return fmt.Errorf("Codex account egress unavailable: %w", err)
		}
		defer release()
		environment = append(environment, accountegress.EnvList(values)...)
	}
	// Launcher defaults come first so explicit user arguments still win.
	arguments := []string{"-c", codexDefaultSubagentOverride}
	if launch.client != nil {
		storeID := os.Getenv("MIRA_CODEX_STORE_ID")
		if storeID == "" {
			storeID = "personal"
		}
		arguments = append(arguments,
			"-c", `experimental_thread_store.type="remote_http"`,
			"-c", "experimental_thread_store.endpoint="+strconv.Quote(launch.client.identity.ServerURL),
			"-c", "experimental_thread_store.store_id="+strconv.Quote(storeID),
		)
		environment = append(environment, "MIRA_NODE_TOKEN="+launch.client.identity.Token, "MIRA_SERVER_URL="+launch.client.identity.ServerURL)
		if account.NodeAccountID != "" {
			environment = append(environment, "MIRA_NODE_CODEX_ACCOUNT_ID="+account.NodeAccountID)
		}
	}
	arguments = append(arguments, "-c", `approval_policy="never"`, "-c", `sandbox_mode="danger-full-access"`)
	if compatible {
		override, err := codexAccountSQLiteOverride(launch.identityFile, codexPath, configuration.CodexAccountID, overrides)
		if err != nil {
			return err
		}
		if override != "" {
			arguments = append(arguments, "-c", override)
		}
	}
	for _, override := range overrides {
		if override != codexDefaultSubagentOverride {
			arguments = append(arguments, "-c", override)
		}
	}
	command := exec.Command(codexPath, append(arguments, args...)...)
	command.Env = environment
	command.Stdin, command.Stdout, command.Stderr = stdin, stdout, stderr
	return runChild(command)
}

// sortedChoices keeps menus stable: named accounts first, then by selector.
func sortedChoices(choices []localAccountChoice) []localAccountChoice {
	sort.SliceStable(choices, func(i, j int) bool {
		if (choices[i].Name == "") != (choices[j].Name == "") {
			return choices[i].Name != ""
		}
		return choices[i].Selector < choices[j].Selector
	})
	return choices
}
