package supervisor

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// MaintenanceLock serializes release updates with install, repair and
// uninstall across processes. The global lock order is maintenance first,
// followed by Supervisor opMu or installation's .install.lock; callers must
// never acquire maintenance while holding either inner lock.
type MaintenanceLock struct {
	lock *instanceLock
}

func AcquireMaintenanceLock(ctx context.Context, stateDir string) (*MaintenanceLock, error) {
	layout, err := NewLayout(stateDir)
	if err != nil {
		return nil, err
	}
	if err := layout.Prepare(); err != nil {
		return nil, err
	}
	for {
		lock, err := acquireInstanceLock(layout.Maintenance)
		if err == nil {
			return &MaintenanceLock{lock: lock}, nil
		}
		if !errors.Is(err, ErrAlreadyRunning) {
			return nil, fmt.Errorf("lock Mira maintenance: %w", err)
		}
		timer := time.NewTimer(25 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, fmt.Errorf("lock Mira maintenance: %w", ctx.Err())
		case <-timer.C:
		}
	}
}

func (lock *MaintenanceLock) Close() error {
	if lock == nil || lock.lock == nil {
		return nil
	}
	err := lock.lock.Close()
	lock.lock = nil
	return err
}
