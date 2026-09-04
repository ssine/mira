package node

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
)

// Credentials arrive over the private supervisor pipe, never process arguments.
func RunSSHWorker(ctx context.Context) error {
	reader := bufio.NewReaderSize(os.Stdin, 64*1024)
	bootstrap, err := reader.ReadSlice('\n')
	if err != nil {
		return fmt.Errorf("invalid SSH worker bootstrap")
	}
	var config sshWorkerConfig
	if err := json.Unmarshal(bootstrap, &config); err != nil {
		return fmt.Errorf("invalid SSH worker bootstrap")
	}
	if len(config.Roots) == 0 || len(config.Roots) > 32 {
		return fmt.Errorf("invalid SSH worker roots")
	}
	return serveOpenSSH(ctx, reader, os.Stdout, config)
}
