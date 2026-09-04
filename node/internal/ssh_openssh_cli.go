package node

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"

	"golang.org/x/crypto/ssh"
)

// The standard OpenSSH ProxyCommand owns this process for the lifetime of its
// transport, including ControlPersist. Node credentials never enter argv.
func (client *cliClient) runSSHProxy(ctx context.Context, args []string, in io.Reader, out io.Writer) error {
	client.configureSSHHTTP()
	if len(args) != 1 {
		return fmt.Errorf("SSH proxy requires one Node selector")
	}
	conn, _, _, err := client.sshTransport(ctx, args[0])
	if err != nil {
		return err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	var localEOF atomic.Bool
	go func() {
		if _, copyErr := io.Copy(conn, in); copyErr == nil {
			localEOF.Store(true)
		}
		conn.Close()
	}()
	_, err = io.Copy(out, conn)
	if localEOF.Load() || ctx.Err() != nil {
		return nil
	}
	return err
}

func validateOpenSSHOption(value string) error {
	if strings.ContainsAny(value, "\x00\r\n") {
		return fmt.Errorf("invalid SSH option")
	}
	fields := strings.FieldsFunc(value, func(r rune) bool { return r == '=' || r == ' ' || r == '\t' })
	if len(fields) == 0 {
		return fmt.Errorf("invalid SSH option")
	}
	name := strings.ToLower(fields[0])
	for _, reserved := range []string{"hostname", "user", "port", "proxycommand", "proxyjump", "proxyusefdpass", "identityfile", "identityagent", "certificatefile", "identitiesonly", "hostkeyalias", "hostkeyalgorithms", "userknownhostsfile", "globalknownhostsfile", "knownhostscommand", "stricthostkeychecking", "verifyhostkeydns", "canonicalizehostname", "preferredauthentications", "pubkeyauthentication", "passwordauthentication", "kbdinteractiveauthentication", "updatehostkeys", "include", "match"} {
		if name == reserved {
			return fmt.Errorf("%s is managed by Mira's Node identity and relay", name)
		}
	}
	return nil
}

func splitOpenSSHArgs(command string, args []string) ([]string, []string, error) {
	options := []string{}
	withValue := "BbcDEeFIiJLlmOoPpQRSWw"
	if command == "scp" {
		withValue = "cDFiJloPSX"
	}
	if command == "sftp" {
		withValue = "BbDFiJloPRSX"
	}
	for len(args) > 0 {
		a := args[0]
		args = args[1:]
		if a == "--" {
			break
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			args = append([]string{a}, args...)
			break
		}
		if a == "--help" {
			return nil, nil, fmt.Errorf("usage: mira %s [OpenSSH options] <node%s>; identity, account and transport are managed by Mira", command, map[bool]string{true: ":/path", false: ""}[command == "scp"])
		}
		if strings.HasPrefix(a, "--") {
			return nil, nil, fmt.Errorf("unknown OpenSSH option: %s", a)
		}
		if len(a) < 2 {
			return nil, nil, fmt.Errorf("invalid SSH option")
		}
		flag := a[1:2]
		// Process combined flags one at a time so -vF/-vo cannot bypass Mira's
		// identity/transport options by hiding behind a no-argument flag.
		if len(a) > 2 && !strings.Contains(withValue, flag) {
			args = append([]string{"-" + a[2:]}, args...)
			a = a[:2]
		}
		if strings.Contains("FiJ", flag) || (command == "ssh" && strings.Contains("lpWw", flag)) || (command != "ssh" && strings.Contains("DPS", flag)) {
			return nil, nil, fmt.Errorf("-%s is managed by Mira", flag)
		}
		if strings.Contains(withValue, flag) {
			value := a[2:]
			if value == "" {
				if len(args) == 0 {
					return nil, nil, fmt.Errorf("%s requires a value", a)
				}
				value = args[0]
				args = args[1:]
			}
			if flag == "o" {
				if strings.TrimSpace(value) == "" {
					return nil, nil, fmt.Errorf("empty SSH option")
				}
				if err := validateOpenSSHOption(value); err != nil {
					return nil, nil, err
				}
			}
			options = append(options, "-"+flag, value)
		} else {
			options = append(options, a)
		}
	}
	return options, args, nil
}

func (client *cliClient) runOpenSSHClient(ctx context.Context, command string, args []string, in io.Reader, out, stderr io.Writer) error {
	program, err := openSSHProgram(command)
	if err != nil {
		return err
	}
	options, operands, err := splitOpenSSHArgs(command, args)
	if err != nil {
		return err
	}
	if len(operands) == 0 {
		return fmt.Errorf("mira %s requires a Node", command)
	}
	selector := operands[0]
	if command == "scp" {
		selector = ""
		for i, arg := range operands {
			node, path, remote := splitRemoteOperand(arg)
			if !remote {
				continue
			}
			if selector != "" && selector != node {
				return fmt.Errorf("one transfer cannot address different Nodes")
			}
			selector = node
			operands[i] = "mira-target:" + path
		}
		if selector == "" {
			return fmt.Errorf("SCP requires a remote Node operand")
		}
	} else {
		operands[0] = "mira-target"
		if len(operands) > 1 && operands[1] == "--" {
			operands = append(operands[:1], operands[2:]...)
		}
	}
	node, err := client.resolveNode(ctx, selector)
	if err != nil {
		return err
	}
	nodeID := selectorValue(node, "nodeId")
	var remote struct {
		HostKey         string `json:"hostKey"`
		Username        string `json:"username"`
		ProtocolVersion int    `json:"protocolVersion"`
	}
	if err = client.request(ctx, http.MethodGet, "/v1/nodes/"+nodeID+"/ssh/keys", nil, &remote); err != nil {
		return err
	}
	key, _, _, rest, err := ssh.ParseAuthorizedKey([]byte(remote.HostKey))
	if err != nil || len(rest) != 0 || key.Type() != ssh.KeyAlgoED25519 || remote.ProtocolVersion != 1 || remote.Username == "" {
		return fmt.Errorf("invalid approved SSH target identity")
	}
	dir, err := privateSSHDirectory(filepath.Dir(client.options.Identity))
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	private, err := sshPrivateKeyPEM(client.identity.Token, "client")
	if err != nil {
		return err
	}
	if err = privateSSHFile(filepath.Join(dir, "client"), private); err != nil {
		return err
	}
	clear(private)
	if err = privateSSHFile(filepath.Join(dir, "known_hosts"), append([]byte(nodeID+" "), ssh.MarshalAuthorizedKey(key)...)); err != nil {
		return err
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	// Preserve a verified multicall argv[0] alias when one was used.
	if original, e := filepath.Abs(os.Args[0]); e == nil {
		a, e1 := os.Stat(original)
		b, e2 := os.Stat(executable)
		if e1 == nil && e2 == nil && os.SameFile(a, b) {
			executable = original
		}
	}
	proxyArgs := []string{executable}
	if len(os.Args) > 1 && os.Args[1] == "cli" {
		proxyArgs = append(proxyArgs, "cli")
	}
	proxyArgs = append(proxyArgs, "ssh-proxy", nodeID)
	for i, s := range proxyArgs {
		proxyArgs[i] = escapeSSHProxyArg(s)
	}
	proxy := strings.ReplaceAll(strings.Join(proxyArgs, " "), "%", "%%")
	settings := []string{"Host *", "HostName mira-target", "IdentitiesOnly yes", "IdentityAgent none", "BatchMode yes", "PreferredAuthentications publickey", "PasswordAuthentication no", "KbdInteractiveAuthentication no", "StrictHostKeyChecking yes", "CheckHostIP no", "UpdateHostKeys no", "HostKeyAlgorithms ssh-ed25519", "GlobalKnownHostsFile none"}
	if strings.ContainsAny(proxy, "\x00\r\n") {
		return fmt.Errorf("invalid proxy path")
	}
	settings = append(settings, "ProxyCommand "+proxy)
	for _, e := range [][2]string{{"User", remote.Username}, {"HostKeyAlias", nodeID}, {"IdentityFile", filepath.ToSlash(filepath.Join(dir, "client"))}, {"UserKnownHostsFile", filepath.ToSlash(filepath.Join(dir, "known_hosts"))}} {
		q, err := sshConfigString(e[1])
		if err != nil {
			return err
		}
		settings = append(settings, e[0]+" "+q)
	}
	config := filepath.Join(dir, "ssh_config")
	if err = privateSSHFile(config, []byte(strings.Join(settings, "\n")+"\n")); err != nil {
		return err
	}
	argv := []string{"-F", config}
	if command != "ssh" {
		sshPath, err := openSSHProgram("ssh")
		if err != nil {
			return err
		}
		argv = append(argv, "-S", sshPath)
	}
	argv = append(argv, options...)
	argv = append(argv, operands...)
	cmd := exec.CommandContext(ctx, program, argv...)
	cmd.Env = append(os.Environ(), "MIRA_IDENTITY_FILE="+client.options.Identity)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = in, out, stderr
	return cmd.Run()
}
