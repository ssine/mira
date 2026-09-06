//go:build windows

package node

import (
	"context"
	"fmt"
	"os"
	"strings"

	"golang.org/x/sys/windows/svc"
)

// RunWindowsServiceIfNeeded connects a Supervisor invocation to the Windows
// Service Control Manager. Normal CLI invocations never enter this path.
func RunWindowsServiceIfNeeded(args []string) (bool, int) {
	roleArgs := args
	if len(roleArgs) > 1 && roleArgs[0] == "cli" && isSystemRole(roleArgs[1:]) {
		roleArgs = roleArgs[1:]
	}
	if len(roleArgs) == 0 || roleArgs[0] != "supervisor" {
		return false, 0
	}
	isService, err := svc.IsWindowsService()
	if err != nil || !isService {
		return false, 0
	}
	name := "Mira"
	for index := 1; index < len(roleArgs); index++ {
		if roleArgs[index] == "--windows-service-name" && index+1 < len(roleArgs) {
			name = roleArgs[index+1]
			break
		}
		if value, found := strings.CutPrefix(roleArgs[index], "--windows-service-name="); found && value != "" {
			name = value
			break
		}
	}
	if err := svc.Run(name, &miraWindowsService{args: roleArgs}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return true, 1
	}
	return true, 0
}

type miraWindowsService struct {
	args []string
}

func (service *miraWindowsService) Execute(_ []string, requests <-chan svc.ChangeRequest, changes chan<- svc.Status) (bool, uint32) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	changes <- svc.Status{State: svc.StartPending}
	done := make(chan int, 1)
	go func() {
		_, code := RunSystemRole(ctx, service.args, os.Stdin, os.Stdout, os.Stderr)
		done <- code
	}()
	status := svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	changes <- status
	for {
		select {
		case code := <-done:
			if code != 0 {
				return true, uint32(code)
			}
			return false, 0
		case request := <-requests:
			switch request.Cmd {
			case svc.Interrogate:
				changes <- status
			case svc.Stop, svc.Shutdown:
				changes <- svc.Status{State: svc.StopPending}
				cancel()
			}
		}
	}
}
