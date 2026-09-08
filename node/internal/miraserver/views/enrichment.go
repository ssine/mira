package views

import (
	"context"
	"strings"
)

const tokenUsagePredicate = `payload::text ~ '"type"[[:space:]]*:[[:space:]]*"token_count"'`
const modelEventPredicate = `payload::text ~ '"type"[[:space:]]*:[[:space:]]*"(turn_context|thread_settings_applied)"'`
const threadUpdatePredicate = `payload::text ~ '"type"[[:space:]]*:[[:space:]]*"(user_message|agent_message|item_completed|view_image_tool_call|task_complete|turn_complete|turn_aborted|error|message|function_call_output|custom_tool_call_output)"'`

// NormalizeTokenUsage returns the stable public cumulative usage shape.
func NormalizeTokenUsage(value any) map[string]any {
	valueObject := object(value)
	if valueObject == nil {
		return nil
	}
	result := map[string]any{}
	provided := false
	for source, target := range map[string]string{
		"input_tokens": "inputTokens", "output_tokens": "outputTokens", "cached_input_tokens": "cachedInputTokens",
	} {
		count, ok := safeInteger(valueObject[source])
		if !ok || count < 0 {
			result[target] = nil
			continue
		}
		result[target] = count
		provided = true
	}
	if !provided {
		return nil
	}
	return result
}

func (service *Service) addTokenUsage(ctx context.Context, storeID string, threads []Thread) ([]Thread, error) {
	pending := []Thread{}
	for index := range threads {
		threads[index].TokenUsage = NormalizeTokenUsage(threads[index].TokenUsage)
		if threads[index].TokenUsage == nil {
			pending = append(pending, threads[index])
		}
	}
	found := map[string]map[string]any{}
	before := map[string]int64{}
	for _, thread := range pending {
		before[thread.ThreadID] = thread.ItemCount + 1
	}
	for len(pending) > 0 {
		ids := make([]string, len(pending))
		generations := make([]int64, len(pending))
		positions := make([]int64, len(pending))
		for index, thread := range pending {
			ids[index], generations[index], positions[index] = thread.ThreadID, thread.Generation, before[thread.ThreadID]
		}
		rows, err := service.pool.Query(ctx, `SELECT selected.thread_id,events.item_seq::text,events.payload
      FROM unnest($2::text[], $3::bigint[], $4::bigint[]) selected(thread_id,generation,before)
      LEFT JOIN LATERAL (
        SELECT item_seq,payload FROM codex_thread_events
        WHERE store_id=$1 AND thread_id=selected.thread_id AND generation=selected.generation
          AND item_seq<selected.before AND `+tokenUsagePredicate+`
        ORDER BY item_seq DESC LIMIT 1
      ) events ON TRUE`, storeID, ids, generations, positions)
		if err != nil {
			return nil, err
		}
		type candidate struct {
			sequence *int64
			payload  map[string]any
		}
		candidates := map[string]candidate{}
		for rows.Next() {
			var id string
			var sequence *string
			var raw []byte
			if err := rows.Scan(&id, &sequence, &raw); err != nil {
				rows.Close()
				return nil, err
			}
			var parsedSequence *int64
			if sequence != nil {
				value, err := postgresTextInt(*sequence)
				if err != nil {
					rows.Close()
					return nil, err
				}
				parsedSequence = &value
			}
			payload, err := decodeObject(raw)
			if err != nil && sequence != nil {
				rows.Close()
				return nil, err
			}
			candidates[id] = candidate{sequence: parsedSequence, payload: payload}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
		next := []Thread{}
		for _, thread := range pending {
			candidate := candidates[thread.ThreadID]
			record := candidate.payload
			var usage map[string]any
			if stringValue(record["type"]) == "event_msg" {
				payload := object(record["payload"])
				if stringValue(payload["type"]) == "token_count" {
					usage = NormalizeTokenUsage(object(payload["info"])["total_token_usage"])
				}
			}
			if usage != nil || candidate.sequence == nil {
				found[thread.ThreadID] = usage
			} else {
				before[thread.ThreadID] = *candidate.sequence
				next = append(next, thread)
			}
		}
		pending = next
	}
	for index := range threads {
		if threads[index].TokenUsage == nil {
			threads[index].TokenUsage = found[threads[index].ThreadID]
		}
	}
	return threads, nil
}

func validModel(value *string) *string {
	if value == nil || strings.TrimSpace(*value) == "" {
		return nil
	}
	return value
}

func (service *Service) addModelSettings(ctx context.Context, storeID string, threads []Thread) ([]Thread, error) {
	pending := []Thread{}
	before := map[string]int64{}
	for _, thread := range threads {
		if validModel(thread.Model) == nil || thread.ReasoningEffort == nil {
			pending = append(pending, thread)
			before[thread.ThreadID] = thread.ItemCount + 1
		}
	}
	found := map[string]*string{}
	efforts := map[string]*string{}
	for len(pending) > 0 {
		ids := make([]string, len(pending))
		generations := make([]int64, len(pending))
		positions := make([]int64, len(pending))
		for index, thread := range pending {
			ids[index], generations[index], positions[index] = thread.ThreadID, thread.Generation, before[thread.ThreadID]
		}
		rows, err := service.pool.Query(ctx, `SELECT selected.thread_id,events.item_seq::text,events.payload
      FROM unnest($2::text[], $3::bigint[], $4::bigint[]) selected(thread_id,generation,before)
      LEFT JOIN LATERAL (
        SELECT item_seq,payload FROM codex_thread_events
        WHERE store_id=$1 AND thread_id=selected.thread_id AND generation=selected.generation
          AND item_seq<selected.before AND `+modelEventPredicate+`
        ORDER BY item_seq DESC LIMIT 1
      ) events ON TRUE`, storeID, ids, generations, positions)
		if err != nil {
			return nil, err
		}
		type candidate struct {
			sequence *int64
			model    *string
			effort   *string
		}
		candidates := map[string]candidate{}
		for rows.Next() {
			var id string
			var sequence *string
			var raw []byte
			if err := rows.Scan(&id, &sequence, &raw); err != nil {
				rows.Close()
				return nil, err
			}
			var parsedSequence *int64
			if sequence != nil {
				value, err := postgresTextInt(*sequence)
				if err != nil {
					rows.Close()
					return nil, err
				}
				parsedSequence = &value
			}
			record, _ := decodeObject(raw)
			model := modelFromRecord(record)
			candidates[id] = candidate{parsedSequence, model, reasoningEffortFromRecord(record)}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
		next := []Thread{}
		for _, thread := range pending {
			candidate := candidates[thread.ThreadID]
			if found[thread.ThreadID] == nil {
				found[thread.ThreadID] = candidate.model
			}
			if efforts[thread.ThreadID] == nil {
				efforts[thread.ThreadID] = candidate.effort
			}
			modelResolved := validModel(thread.Model) != nil || found[thread.ThreadID] != nil
			effortResolved := thread.ReasoningEffort != nil || efforts[thread.ThreadID] != nil
			if candidate.sequence != nil && (!modelResolved || !effortResolved) {
				before[thread.ThreadID] = *candidate.sequence
				next = append(next, thread)
			}
		}
		pending = next
	}
	for index := range threads {
		if validModel(threads[index].Model) == nil {
			threads[index].Model = found[threads[index].ThreadID]
		}
		if threads[index].ReasoningEffort == nil {
			threads[index].ReasoningEffort = efforts[threads[index].ThreadID]
		}
	}
	return threads, nil
}

// Reasoning effort is a rebuildable read projection, never a Node/UI preference.
// Read both upstream record formats and retain future non-empty effort values.
func reasoningEffortFromRecord(record map[string]any) *string {
	payload := object(record["payload"])
	var value string
	switch stringValue(record["type"]) {
	case "turn_context":
		value = stringValue(payload["effort"])
	case "event_msg":
		if stringValue(payload["type"]) == "thread_settings_applied" {
			value = stringValue(object(payload["thread_settings"])["reasoning_effort"])
		}
	}
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return &value
}

func modelFromRecord(record map[string]any) *string {
	var value string
	if stringValue(record["type"]) == "turn_context" {
		value = stringValue(object(record["payload"])["model"])
	} else if stringValue(record["type"]) == "event_msg" {
		payload := object(record["payload"])
		if stringValue(payload["type"]) == "thread_settings_applied" {
			value = stringValue(object(payload["thread_settings"])["model"])
		}
	}
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return &value
}

func hasReadableText(value any) bool {
	if text, ok := value.(string); ok {
		clean := strings.Map(func(character rune) rune {
			if character <= 0x1f || (character >= 0x7f && character <= 0x9f) {
				return -1
			}
			return character
		}, text)
		return strings.TrimSpace(clean) != ""
	}
	if values := array(value); values != nil {
		for _, entry := range values {
			if hasReadableText(entry) {
				return true
			}
		}
		return false
	}
	valueObject := object(value)
	if valueObject == nil {
		return false
	}
	for _, key := range []string{"text", "inputText", "input_text", "outputText", "output_text", "message", "content"} {
		if hasReadableText(valueObject[key]) {
			return true
		}
	}
	return false
}

func VisibleAssistantUpdate(record map[string]any) bool {
	payload := object(record["payload"])
	if stringValue(record["type"]) == "event_msg" {
		if stringValue(payload["type"]) == "agent_message" {
			return hasReadableText(first(payload["message"], payload["content"]))
		}
		if stringValue(payload["type"]) == "item_completed" {
			item := object(payload["item"])
			return normalizedType(item["type"]) == "agentmessage" && hasReadableText(first(item["content"], item["text"], item["message"]))
		}
	}
	return stringValue(record["type"]) == "response_item" && stringValue(payload["type"]) == "message" && stringValue(payload["role"]) == "assistant" &&
		hasReadableText(first(payload["content"], payload["output_text"], payload["text"]))
}

func (service *Service) addReadStates(ctx context.Context, storeID string, threads []Thread) ([]Thread, error) {
	if len(threads) == 0 {
		return threads, nil
	}
	latest := map[string]int64{}
	pending := append([]Thread(nil), threads...)
	before := map[string]int64{}
	for _, thread := range pending {
		before[thread.ThreadID] = thread.ItemCount + 1
	}
	for len(pending) > 0 {
		ids := make([]string, len(pending))
		generations := make([]int64, len(pending))
		positions := make([]int64, len(pending))
		for index, thread := range pending {
			ids[index], generations[index], positions[index] = thread.ThreadID, thread.Generation, before[thread.ThreadID]
		}
		rows, err := service.pool.Query(ctx, `SELECT selected.thread_id, events.item_seq::text, events.payload
      FROM unnest($2::text[], $3::bigint[], $4::bigint[]) selected(thread_id,generation,before)
      LEFT JOIN LATERAL (
        SELECT item_seq,payload FROM codex_thread_events
        WHERE store_id=$1 AND thread_id=selected.thread_id AND generation=selected.generation
          AND item_seq<selected.before AND `+threadUpdatePredicate+`
        ORDER BY item_seq DESC LIMIT 32
      ) events ON TRUE ORDER BY selected.thread_id,events.item_seq DESC`, storeID, ids, generations, positions)
		if err != nil {
			return nil, err
		}
		type candidate struct {
			sequence int64
			record   map[string]any
		}
		grouped := map[string][]candidate{}
		for rows.Next() {
			var id string
			var sequence *string
			var raw []byte
			if err := rows.Scan(&id, &sequence, &raw); err != nil {
				rows.Close()
				return nil, err
			}
			if sequence == nil {
				continue
			}
			value, err := postgresTextInt(*sequence)
			if err != nil {
				rows.Close()
				return nil, err
			}
			record, err := decodeObject(raw)
			if err != nil {
				rows.Close()
				return nil, err
			}
			grouped[id] = append(grouped[id], candidate{value, record})
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
		next := []Thread{}
		for _, thread := range pending {
			candidates := grouped[thread.ThreadID]
			visible := int64(0)
			for _, candidate := range candidates {
				if VisibleAssistantUpdate(candidate.record) {
					visible = candidate.sequence
					break
				}
			}
			if visible > 0 || len(candidates) == 0 {
				latest[thread.ThreadID] = visible
			} else {
				before[thread.ThreadID] = candidates[len(candidates)-1].sequence
				next = append(next, thread)
			}
		}
		pending = next
	}
	ids, generations, _ := threadCoordinates(threads)
	rows, err := service.pool.Query(ctx, `SELECT selected.thread_id, selected.generation::text, reads.item_count::text
    FROM unnest($2::text[], $3::bigint[]) selected(thread_id,generation)
    LEFT JOIN mira_thread_read_positions reads ON reads.store_id=$1
      AND reads.thread_id=selected.thread_id AND reads.generation=selected.generation`, storeID, ids, generations)
	if err != nil {
		return nil, err
	}
	states := map[string]map[string]any{}
	for rows.Next() {
		var id, generation string
		var readCount *string
		if err := rows.Scan(&id, &generation, &readCount); err != nil {
			rows.Close()
			return nil, err
		}
		generationNumber, _ := postgresTextInt(generation)
		readNumber := int64(0)
		if readCount != nil {
			readNumber, _ = postgresTextInt(*readCount)
		}
		states[id] = map[string]any{"generation": generationNumber, "latestItemSeq": latest[id], "readItemCount": readNumber, "unread": latest[id] > readNumber}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	for index := range threads {
		threads[index].ReadState = states[threads[index].ThreadID]
	}
	return threads, nil
}
