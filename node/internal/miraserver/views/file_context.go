package views

import "context"

// File attribution is a disposable projection of existing execution events;
// changing a thread's current Node never rewrites an older turn's origin.
func (service *Service) attachFileContexts(ctx context.Context, storeID, threadID string, generation int64, trace []map[string]any) error {
	turns := []string{}
	seen := map[string]bool{}
	for _, item := range trace {
		turn := stringValue(item["turnId"])
		if turn != "" && !seen[turn] {
			seen[turn] = true
			turns = append(turns, turn)
		}
	}
	if len(turns) == 0 {
		return nil
	}
	rows, err := service.pool.Query(ctx, `SELECT DISTINCT e.turn_id,b.node_id::text FROM mira_codex_execution_events e JOIN mira_node_codex_accounts b USING(node_account_id) WHERE e.store_id=$1 AND e.thread_id=$2 AND e.generation=$3 AND e.turn_id=ANY($4::text[])`, storeID, threadID, generation, turns)
	if err != nil {
		return err
	}
	defer rows.Close()
	byTurn := map[string][]string{}
	for rows.Next() {
		var turn, nodeID string
		if err = rows.Scan(&turn, &nodeID); err != nil {
			return err
		}
		byTurn[turn] = append(byTurn[turn], nodeID)
	}
	if err = rows.Err(); err != nil {
		return err
	}
	for _, item := range trace {
		nodes := byTurn[stringValue(item["turnId"])]
		source := object(item["sourceContext"])
		if source == nil {
			source = map[string]any{}
		}
		if len(nodes) == 1 {
			source["nodeId"] = nodes[0]
		} else if len(nodes) > 1 {
			source["candidateNodeIds"] = nodes
		}
		item["sourceContext"] = source
	}
	return nil
}
