package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/gorilla/websocket"
)

type residencyCandidate struct {
	ThreadID string `json:"threadId"`
	Revision string `json:"revision"`
	IdleAt   int64  `json:"idleAt"`
}

type residencyResponse struct {
	Oldest            *residencyCandidate `json:"oldest"`
	EvictionScheduled bool                `json:"evictionScheduled"`
}

var errResidencyUnsupported = errors.New("runtime does not support memory residency")

// This connection never subscribes to a thread or loads conversation history.
func callCodexResidency(parent context.Context, listenURL string, evict *residencyCandidate) (residencyResponse, error) {
	ctx, cancel := context.WithTimeout(parent, 3*time.Second)
	defer cancel()
	var result residencyResponse
	connection, _, err := websocket.DefaultDialer.DialContext(ctx, listenURL, nil)
	if err != nil {
		return result, fmt.Errorf("cannot connect to residency controller")
	}
	defer connection.Close()
	stop := context.AfterFunc(ctx, func() { _ = connection.Close() })
	defer stop()
	connection.SetReadLimit(64 * 1024)
	deadline, _ := ctx.Deadline()
	_ = connection.SetReadDeadline(deadline)
	_ = connection.SetWriteDeadline(deadline)
	call := func(id int, method string, params, target any) error {
		if err := connection.WriteJSON(map[string]any{"id": id, "method": method, "params": params}); err != nil {
			return fmt.Errorf("residency connection closed")
		}
		for {
			var message struct {
				ID     int             `json:"id"`
				Method string          `json:"method"`
				Result json.RawMessage `json:"result"`
				Error  *struct {
					Code int `json:"code"`
				} `json:"error"`
			}
			if err := connection.ReadJSON(&message); err != nil {
				return fmt.Errorf("residency query failed")
			}
			if message.Method != "" || message.ID != id {
				continue
			}
			if message.Error != nil {
				if message.Error.Code == -32601 {
					return errResidencyUnsupported
				}
				return fmt.Errorf("runtime rejected residency query")
			}
			if len(message.Result) == 0 || string(message.Result) == "null" || json.Unmarshal(message.Result, target) != nil {
				return fmt.Errorf("invalid residency response")
			}
			return nil
		}
	}
	var initialized map[string]any
	if err := call(1, "initialize", map[string]any{
		"clientInfo":   map[string]string{"name": "mira_node_residency", "version": Version},
		"capabilities": map[string]bool{"experimentalApi": true},
	}, &initialized); err != nil {
		return result, err
	}
	if err := connection.WriteJSON(map[string]string{"method": "initialized"}); err != nil {
		return result, err
	}
	var wire map[string]json.RawMessage
	err = call(2, "mira/thread/residency", map[string]any{"evict": evict}, &wire)
	if err == nil {
		oldest, present := wire["oldest"]
		scheduled := wire["evictionScheduled"]
		if !present || len(scheduled) == 0 || string(scheduled) == "null" || json.Unmarshal(oldest, &result.Oldest) != nil || json.Unmarshal(scheduled, &result.EvictionScheduled) != nil {
			return result, fmt.Errorf("invalid residency response")
		}
	}
	if err == nil && result.Oldest != nil && (result.Oldest.ThreadID == "" || len(result.Oldest.ThreadID) > 128 || result.Oldest.Revision == "" || len(result.Oldest.Revision) > 128 || result.Oldest.IdleAt <= 0) {
		err = fmt.Errorf("invalid residency candidate")
	}
	return result, err
}
