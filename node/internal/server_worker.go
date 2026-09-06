package node

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/ssine/mira/node/internal/miraserver"
	"github.com/ssine/mira/node/internal/miraserver/foundation"
)

// RunServerWorker runs the native Server role from the same executable as the
// Supervisor, Node worker, CLI and SSH roles.
func RunServerWorker(ctx context.Context, args []string) error {
	configuration, err := foundation.LoadConfig()
	if err != nil {
		return err
	}
	for index := 0; index < len(args); index++ {
		switch args[index] {
		case "--listen":
			if index+1 >= len(args) {
				return fmt.Errorf("--listen requires host:port")
			}
			index++
			host, portText, err := net.SplitHostPort(args[index])
			if err != nil {
				return fmt.Errorf("parse --listen: %w", err)
			}
			port, err := strconv.Atoi(portText)
			if err != nil || port < 1 || port > 65535 {
				return fmt.Errorf("parse --listen: invalid port")
			}
			configuration.ListenHost, configuration.ListenPort = host, port
		case "--database-url":
			if index+1 >= len(args) {
				return fmt.Errorf("--database-url requires a value")
			}
			index++
			configuration.DatabaseURL = args[index]
		default:
			return fmt.Errorf("unknown server-worker argument %s", args[index])
		}
	}
	server, err := miraserver.New(ctx, miraserver.Config{Foundation: configuration, Version: Version})
	if err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- server.ListenAndServe() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		// Finish or force-close active handlers before the Supervisor's 15s
		// worker deadline, leaving time for process exit and OS reaping.
		shutdownContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownContext); err != nil {
			return err
		}
		return ctx.Err()
	}
}

// RunServerAdmin implements the database-only administrator command. It does
// not require a running Mira Server or Supervisor.
func RunServerAdmin(ctx context.Context, args []string, stdin io.Reader, stdout io.Writer) error {
	if len(args) != 2 || args[0] != "set-password" {
		return fmt.Errorf("usage: mira server admin set-password <username>")
	}
	configuration, err := foundation.LoadConfig()
	if err != nil {
		return err
	}
	pool, err := foundation.OpenPool(ctx, configuration)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := foundation.InitializeDatabase(ctx, pool); err != nil {
		return err
	}
	password := strings.TrimRight(readLine(stdin), "\r\n")
	if password == "" {
		return fmt.Errorf("administrator password must not be empty")
	}
	if _, err := foundation.SetAdminPassword(ctx, pool, args[1], password); err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "Configured Mira administrator %s.\n", args[1])
	return err
}

func readLine(reader io.Reader) string {
	line, _ := bufio.NewReader(reader).ReadString('\n')
	return line
}
