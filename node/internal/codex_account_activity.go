package node

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/gorilla/websocket"
)

func (manager *appServerManager) clearAccountActivityLocked() {
	manager.activeThreads = nil
	manager.pendingTurns = nil
	manager.loginPending = false
}

// Called with mu held. Notifications are only a cache: the browser or reverse
// channel may close before completion arrives. Gate new execution while checking
// every loaded thread, including children, without holding up Node heartbeats.
func (manager *appServerManager) confirmAccountIdleLocked(ctx context.Context) error {
	instance := manager.instance
	if instance == nil || channelClosed(instance.done) {
		manager.clearAccountActivityLocked()
		return nil
	}
	if manager.managementSession != "" || len(manager.threadManagement) > 0 || manager.residencyEvicting {
		return errCodexAccountBusy
	}
	// An unacknowledged request might still create a thread after a read-only
	// snapshot. Neither a lost connection nor an empty list proves it finished.
	if len(manager.pendingTurns) > 0 {
		return fmt.Errorf("账号仍有未完成的执行请求，等待 App Server 确认")
	}
	if !instance.ready {
		return fmt.Errorf("账号尚未就绪，无法确认运行状态")
	}
	manager.activityChecking = true
	manager.mu.Unlock()
	active, err := readAccountActivity(ctx, instance.listenURL)
	manager.mu.Lock()
	manager.activityChecking = false
	if manager.instance != instance {
		return fmt.Errorf("核验期间账号运行实例已变更，请重试")
	}
	if channelClosed(instance.done) {
		manager.clearAccountActivityLocked()
		return nil
	}
	if err != nil {
		return fmt.Errorf("无法确认账号是否空闲：%w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	manager.activeThreads = active
	if len(active) > 0 {
		return fmt.Errorf("账号仍有任务执行，请先结束任务")
	}
	return nil
}

func readAccountActivity(parent context.Context, listenURL string) (map[string]bool, error) {
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	connection, _, err := websocket.DefaultDialer.DialContext(ctx, listenURL, nil)
	if err != nil {
		return nil, fmt.Errorf("无法连接本地 App Server")
	}
	defer connection.Close()
	stop := context.AfterFunc(ctx, func() { _ = connection.Close() })
	defer stop()
	connection.SetReadLimit(16 * 1024 * 1024)
	deadline, _ := ctx.Deadline()
	_ = connection.SetReadDeadline(deadline)
	_ = connection.SetWriteDeadline(deadline)
	sequence := 0
	changed := false
	call := func(method string, params, result any) error {
		sequence++
		if err := connection.WriteJSON(map[string]any{"id": sequence, "method": method, "params": params}); err != nil {
			return fmt.Errorf("状态查询连接已断开")
		}
		for {
			var message struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
				Result json.RawMessage `json:"result"`
				Error  json.RawMessage `json:"error"`
			}
			if err := connection.ReadJSON(&message); err != nil {
				return fmt.Errorf("状态查询未完成")
			}
			if message.Method != "" {
				switch message.Method {
				case "thread/status/changed", "turn/started", "turn/completed", "thread/closed":
					changed = true
				}
				continue
			}
			if string(message.ID) != fmt.Sprint(sequence) {
				continue
			}
			if len(message.Error) > 0 && string(message.Error) != "null" {
				return fmt.Errorf("App Server 拒绝状态查询")
			}
			if len(message.Result) == 0 || string(message.Result) == "null" || json.Unmarshal(message.Result, result) != nil {
				return fmt.Errorf("App Server 返回无效状态")
			}
			return nil
		}
	}
	var initialized map[string]any
	if err := call("initialize", map[string]any{
		"clientInfo":   map[string]string{"name": "mira_node_activity", "version": Version},
		"capabilities": map[string]bool{"experimentalApi": true},
	}, &initialized); err != nil {
		return nil, err
	}
	if err := connection.WriteJSON(map[string]string{"method": "initialized"}); err != nil {
		return nil, fmt.Errorf("状态查询连接已断开")
	}
	loaded := func() (map[string]bool, error) {
		ids, cursors := map[string]bool{}, map[string]bool{}
		cursor := ""
		for page := 0; page < 64; page++ {
			params := map[string]any{"limit": 100}
			if cursor != "" {
				params["cursor"] = cursor
			}
			var result struct {
				Data       *[]string `json:"data"`
				NextCursor string    `json:"nextCursor"`
			}
			if err := call("thread/loaded/list", params, &result); err != nil {
				return nil, err
			}
			if result.Data == nil || len(*result.Data) > 100 {
				return nil, fmt.Errorf("App Server 返回无效对话列表")
			}
			for _, id := range *result.Data {
				if id == "" || len(id) > 128 || ids[id] {
					return nil, fmt.Errorf("App Server 返回无效对话列表")
				}
				ids[id] = true
			}
			cursor = result.NextCursor
			if cursor == "" {
				return ids, nil
			}
			if cursors[cursor] {
				return nil, fmt.Errorf("App Server 返回重复分页游标")
			}
			cursors[cursor] = true
		}
		return nil, fmt.Errorf("运行中对话过多，无法确认账号空闲")
	}
	ids, err := loaded()
	if err != nil {
		return nil, err
	}
	active := map[string]bool{}
	for id := range ids {
		var result struct {
			Thread struct {
				ID     string `json:"id"`
				Status struct {
					Type string `json:"type"`
				} `json:"status"`
			} `json:"thread"`
		}
		if err := call("thread/read", map[string]any{"threadId": id, "includeTurns": false}, &result); err != nil {
			return nil, err
		}
		if result.Thread.ID != id || result.Thread.Status.Type == "" {
			return nil, fmt.Errorf("App Server 返回无效对话状态")
		}
		switch result.Thread.Status.Type {
		case "idle", "notLoaded", "systemError":
		default:
			active[id] = true
		}
	}
	// A parent may finish spawning a child during the scan. Do not infer idle
	// from a list that changed while its members were being checked.
	if len(active) == 0 && len(ids) > 0 {
		current, err := loaded()
		if err != nil {
			return nil, err
		}
		if len(current) != len(ids) {
			return nil, fmt.Errorf("运行中对话列表已变化，稍后重试")
		}
		for id := range current {
			if !ids[id] {
				return nil, fmt.Errorf("运行中对话列表已变化，稍后重试")
			}
		}
	}
	// Status notifications are broadcast to initialized clients. A task may
	// wake an already loaded child, leaving the membership unchanged; that is
	// also a changing snapshot, not proof that the whole account became idle.
	if len(active) == 0 && changed {
		return nil, fmt.Errorf("对话状态在核验期间发生变化，稍后重试")
	}
	return active, ctx.Err()
}
