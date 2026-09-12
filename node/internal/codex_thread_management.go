package node

import "fmt"

// Protect only the transferred threads. Live status is checked by Codex after
// this gate is acquired, so a missed completion notification cannot keep an
// otherwise idle thread busy forever.
func (manager *appServerManager) beginThreadManagement(sessionID string, threadIDs []string) error {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.managementSession != "" {
		return errCodexAccountBusy
	}
	if len(threadIDs) == 0 || len(threadIDs) > 4096 {
		return fmt.Errorf("invalid thread handoff scope")
	}
	scope := make(map[string]bool, len(threadIDs))
	for _, id := range threadIDs {
		if id == "" || len(id) > 128 || manager.threadManagement[id] != "" {
			return fmt.Errorf("对话 %s 正在交接，请稍后重试", id)
		}
		scope[id] = true
	}
	for _, pending := range manager.pendingTurns {
		if scope[pending.ThreadID] {
			return fmt.Errorf("对话 %s 的请求尚未完成，请稍后重试", pending.ThreadID)
		}
	}
	if manager.threadManagement == nil {
		manager.threadManagement = map[string]string{}
	}
	for id := range scope {
		manager.threadManagement[id] = sessionID
	}
	return nil
}

func (manager *appServerManager) endThreadManagement(sessionID string) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	for id, owner := range manager.threadManagement {
		if owner == sessionID {
			delete(manager.threadManagement, id)
		}
	}
}
