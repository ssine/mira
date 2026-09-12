package views

import (
	"context"
	"math/big"
	"strconv"
)

const (
	PricingDate   = "2026-09-06"
	PricingSource = "https://developers.openai.com/api/docs/pricing"
	costPredicate = `payload::text ~ '"type"[[:space:]]*:[[:space:]]*"(turn_context|thread_settings_applied|token_count)"'`
)

type modelPrice struct {
	input, cached, write, output int64 // nanodollars per token
	hasWrite                     bool
	longContext                  int64
}

var modelPrices = map[string]modelPrice{
	"gpt-6-astra":   {input: 10_000, cached: 1_000, write: 12_500, output: 50_000, hasWrite: true, longContext: 272_000},
	"gpt-5.6-sol":   {input: 4_000, cached: 400, write: 5_000, output: 20_000, hasWrite: true, longContext: 272_000},
	"gpt-5.6-terra": {input: 2_000, cached: 200, write: 2_500, output: 12_000, hasWrite: true, longContext: 272_000},
	"gpt-5.6-luna":  {input: 200, cached: 20, write: 250, output: 1_200, hasWrite: true, longContext: 272_000},
	"gpt-5.5":       {input: 5_000, cached: 500, output: 30_000, longContext: 272_000},
	"gpt-5.4":       {input: 2_500, cached: 250, output: 15_000, longContext: 272_000},
	"gpt-5.3-codex": {input: 1_750, cached: 175, output: 14_000},
}

type costTotals struct {
	observed, long                 bool
	priced, unpriced, longRequests int64
	models, reasons                []string
	input, cached, write, output   big.Int
}

type CostProjection struct {
	costTotals
	model             string
	last              [4]int64
	turnID            string
	turns             map[string]*costTotals
	turnOrder         []string
	turnFilter        map[string]bool
	turnLimit         int
	awaitForkBoundary bool
	scopeStarted      bool
}

type cachedCost struct {
	itemCount int64
	state     *CostProjection
}

func NewCostProjection(awaitForkBoundary bool, turnIDs []string) *CostProjection {
	var filter map[string]bool
	limit := 512
	if turnIDs != nil {
		filter = map[string]bool{}
		for _, id := range turnIDs {
			if id != "" {
				filter[id] = true
			}
		}
		limit = len(filter)
	}
	return &CostProjection{turns: map[string]*costTotals{}, turnFilter: filter, turnLimit: limit,
		awaitForkBoundary: awaitForkBoundary, scopeStarted: !awaitForkBoundary}
}

func cloneCostTotals(source costTotals) costTotals {
	result := source
	result.models = append([]string(nil), source.models...)
	result.reasons = append([]string(nil), source.reasons...)
	result.input.Set(&source.input)
	result.cached.Set(&source.cached)
	result.write.Set(&source.write)
	result.output.Set(&source.output)
	return result
}

func (state *CostProjection) clone() *CostProjection {
	result := *state
	result.costTotals = cloneCostTotals(state.costTotals)
	result.turnOrder = append([]string(nil), state.turnOrder...)
	result.turns = map[string]*costTotals{}
	for id, turn := range state.turns {
		copy := cloneCostTotals(*turn)
		result.turns[id] = &copy
	}
	if state.turnFilter != nil {
		result.turnFilter = map[string]bool{}
		for id := range state.turnFilter {
			result.turnFilter[id] = true
		}
	}
	return &result
}

func usageCounts(value any) ([4]int64, bool) {
	valueObject := object(value)
	if valueObject == nil {
		return [4]int64{}, false
	}
	var result [4]int64
	keys := []string{"input_tokens", "cached_input_tokens", "cache_write_input_tokens", "output_tokens"}
	for index, key := range keys {
		if index == 2 && valueObject[key] == nil {
			continue
		}
		count, ok := safeInteger(valueObject[key])
		if !ok || count < 0 {
			return [4]int64{}, false
		}
		result[index] = count
	}
	if result[1] > result[0] || result[2] > result[0]-result[1] {
		return [4]int64{}, false
	}
	return result, true
}

func equalCounts(left, right [4]int64) bool { return left == right }

func incomplete(total *costTotals, reason string) {
	total.reasons = appendUnique(total.reasons, reason)
}

func (state *CostProjection) turn(id string, create bool) *costTotals {
	if id == "" || state.turnFilter != nil && !state.turnFilter[id] {
		return nil
	}
	turn := state.turns[id]
	if turn != nil || !create {
		return turn
	}
	if state.turnLimit == 0 {
		return nil
	}
	for len(state.turns) >= state.turnLimit && len(state.turnOrder) > 0 {
		delete(state.turns, state.turnOrder[0])
		state.turnOrder = state.turnOrder[1:]
	}
	turn = &costTotals{}
	state.turns[id] = turn
	state.turnOrder = append(state.turnOrder, id)
	return turn
}

func addPrice(target *big.Int, tokens, rate int64) {
	target.Add(target, new(big.Int).Mul(big.NewInt(tokens), big.NewInt(rate)))
}

func priceUsage(model string, counts [4]int64) (costTotals, bool) {
	rate, ok := modelPrices[model]
	if !ok || counts[2] != 0 && !rate.hasWrite {
		return costTotals{}, false
	}
	long := rate.longContext != 0 && counts[0] > rate.longContext
	inputRate, cachedRate, writeRate, outputRate := rate.input, rate.cached, rate.write, rate.output
	if long {
		inputRate *= 2
		cachedRate *= 2
		writeRate *= 2
		outputRate = outputRate * 3 / 2
	}
	result := costTotals{long: long}
	addPrice(&result.input, counts[0]-counts[1]-counts[2], inputRate)
	addPrice(&result.cached, counts[1], cachedRate)
	addPrice(&result.write, counts[2], writeRate)
	addPrice(&result.output, counts[3], outputRate)
	return result, true
}

func addPriced(target *costTotals, price costTotals, model string) {
	target.input.Add(&target.input, &price.input)
	target.cached.Add(&target.cached, &price.cached)
	target.write.Add(&target.write, &price.write)
	target.output.Add(&target.output, &price.output)
	target.priced++
	if price.long {
		target.longRequests++
	}
	target.models = appendUnique(target.models, model)
}

// ApplyCostRecord consumes one canonical rollout record in sequence order.
func ApplyCostRecord(state *CostProjection, record map[string]any, threadID string) {
	payload := object(record["payload"])
	if stringValue(record["type"]) == "turn_context" {
		state.model = stringValue(payload["model"])
		if id := stringValue(payload["turn_id"]); id != "" {
			state.turnID = id
		}
		return
	}
	if stringValue(record["type"]) != "event_msg" {
		return
	}
	if stringValue(payload["type"]) == "thread_settings_applied" {
		state.model = stringValue(object(payload["thread_settings"])["model"])
		if state.awaitForkBoundary && !state.scopeStarted && stringValue(payload["thread_id"]) == threadID {
			state.scopeStarted = true
			state.turnID = stringValue(payload["turn_id"])
		}
		return
	}
	if stringValue(payload["type"]) != "token_count" || payload["info"] == nil {
		return
	}
	turnID := stringValue(payload["turn_id"])
	if turnID == "" {
		turnID = state.turnID
	}
	if turnID != "" {
		state.turnID = turnID
	}
	var turn *costTotals
	if state.scopeStarted {
		turn = state.turn(turnID, true)
	}
	info := object(payload["info"])
	total, totalOK := usageCounts(info["total_token_usage"])
	last, lastOK := usageCounts(info["last_token_usage"])
	if !totalOK {
		if state.scopeStarted {
			incomplete(&state.costTotals, "invalid_usage")
			if turn != nil {
				incomplete(turn, "invalid_usage")
			}
		}
		return
	}
	state.observed = true
	if turn != nil {
		turn.observed = true
	}
	if equalCounts(total, state.last) {
		return
	}
	delta := [4]int64{}
	for index := range delta {
		delta[index] = total[index] - state.last[index]
	}
	state.last = total
	if !state.scopeStarted || equalCounts(total, [4]int64{}) {
		return
	}
	invalidLast := !lastOK
	if !invalidLast {
		for index := range last {
			if last[index] > total[index] {
				invalidLast = true
			}
		}
	}
	if invalidLast {
		state.unpriced++
		if turn != nil {
			turn.unpriced++
		}
		incomplete(&state.costTotals, "missing_request_usage")
		if turn != nil {
			incomplete(turn, "missing_request_usage")
		}
		return
	}
	if !equalCounts(total, last) {
		inconsistent := false
		for index := range delta {
			if delta[index] < last[index] {
				inconsistent = true
			}
		}
		if inconsistent {
			state.unpriced++
			if turn != nil {
				turn.unpriced++
			}
			incomplete(&state.costTotals, "inconsistent_usage")
			if turn != nil {
				incomplete(turn, "inconsistent_usage")
			}
			return
		}
		if !equalCounts(delta, last) {
			incomplete(&state.costTotals, "missing_request_usage")
			if turn != nil {
				incomplete(turn, "missing_request_usage")
			}
		}
	}
	if state.priced == 0 && state.unpriced == 0 && !equalCounts(delta, last) {
		incomplete(&state.costTotals, "missing_request_usage")
		if turn != nil {
			incomplete(turn, "missing_request_usage")
		}
	}
	if equalCounts(last, [4]int64{}) {
		return
	}
	price, ok := priceUsage(state.model, last)
	if !ok {
		state.unpriced++
		if turn != nil {
			turn.unpriced++
		}
		reason := "unknown_model"
		if _, exists := modelPrices[state.model]; exists {
			reason = "unsupported_cache_write"
		}
		incomplete(&state.costTotals, reason)
		if turn != nil {
			incomplete(turn, reason)
		}
		return
	}
	addPriced(&state.costTotals, price, state.model)
	if turn != nil {
		addPriced(turn, price, state.model)
	}
}

func dollars(value *big.Int) float64 {
	result, _ := new(big.Rat).SetFrac(value, big.NewInt(1_000_000_000)).Float64()
	return result
}

func totalDollars(total *costTotals) float64 {
	sum := new(big.Int).Add(&total.input, &total.cached)
	sum.Add(sum, &total.write)
	sum.Add(sum, &total.output)
	return dollars(sum)
}

func pricedEstimate(total *costTotals) map[string]any {
	available := total.priced > 0 || total.observed && total.unpriced == 0 && len(total.reasons) == 0
	return map[string]any{
		"currency": "USD", "basis": "standard", "pricingDate": PricingDate, "pricingSource": PricingSource,
		"status": func() string {
			if !available {
				return "unavailable"
			}
			if len(total.reasons) > 0 {
				return "partial"
			}
			return "complete"
		}(),
		"amount": func() any {
			if !available {
				return nil
			}
			return totalDollars(total)
		}(),
		"breakdown": map[string]any{"input": dollars(&total.input), "cached": dollars(&total.cached), "write": dollars(&total.write), "output": dollars(&total.output)},
		"models":    append([]string{}, total.models...), "pricedRequests": total.priced, "unpricedRequests": total.unpriced,
		"longRequests": total.longRequests, "reasons": append([]string{}, total.reasons...),
	}
}

func TurnCostEstimates(state *CostProjection) map[string]map[string]any {
	result := map[string]map[string]any{}
	for _, id := range state.turnOrder {
		if turn := state.turns[id]; turn != nil {
			result[id] = pricedEstimate(turn)
		}
	}
	return result
}

func CostEstimate(state *CostProjection, thread Thread) map[string]any {
	reasons := append([]string(nil), state.reasons...)
	if state.awaitForkBoundary && !state.scopeStarted {
		reasons = appendUnique(reasons, "fork_boundary_missing")
	}
	usage := thread.TokenUsage
	if usage != nil {
		input, inputOK := safeInteger(usage["inputTokens"])
		cached, cachedOK := safeInteger(usage["cachedInputTokens"])
		output, outputOK := safeInteger(usage["outputTokens"])
		if !state.observed || !inputOK || !cachedOK || !outputOK || input != state.last[0] || cached != state.last[1] || output != state.last[3] {
			reasons = appendUnique(reasons, "usage_pending")
		}
	}
	knownZero := len(reasons) == 0 && state.scopeStarted && (state.awaitForkBoundary && state.priced == 0 && state.unpriced == 0 || state.observed && equalCounts(state.last, [4]int64{}))
	available := state.priced > 0 || knownZero
	result := pricedEstimate(&state.costTotals)
	result["scope"] = func() string {
		if state.awaitForkBoundary {
			return "fork"
		}
		return "thread"
	}()
	result["status"] = func() string {
		if !available {
			return "unavailable"
		}
		if len(reasons) > 0 {
			return "partial"
		}
		return "complete"
	}()
	if !available {
		result["amount"] = nil
	} else {
		result["amount"] = totalDollars(&state.costTotals)
	}
	result["reasons"] = reasons
	return result
}

func (service *Service) applyCostRows(ctx context.Context, storeID string, thread Thread, state *CostProjection, after int64) error {
	cursor := after
	for cursor < thread.ItemCount {
		rows, err := service.pool.Query(ctx, `SELECT events.item_seq::text,events.payload FROM codex_thread_events AS events
		 WHERE events.store_id=$1 AND events.thread_id=$2 AND events.generation=$3 AND events.item_seq>$4 AND events.item_seq<=$5 AND `+costPredicate+`
		 ORDER BY events.item_seq LIMIT 256`, storeID, thread.ThreadID, thread.Generation, cursor, thread.ItemCount)
		if err != nil {
			return err
		}
		count := 0
		for rows.Next() {
			var sequence string
			var raw []byte
			if err := rows.Scan(&sequence, &raw); err != nil {
				rows.Close()
				return err
			}
			parsed, err := postgresTextInt(sequence)
			if err != nil {
				rows.Close()
				return err
			}
			record, err := decodeObject(raw)
			if err != nil {
				rows.Close()
				return err
			}
			ApplyCostRecord(state, record, thread.ThreadID)
			cursor = parsed
			count++
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		if count < 256 {
			break
		}
	}
	return nil
}

func (service *Service) costProjection(ctx context.Context, storeID string, thread Thread) (*CostProjection, error) {
	key := storeID + "\x00" + thread.ThreadID + "\x00" + strconv.FormatInt(thread.Generation, 10) + "\x00"
	if thread.ForkedFromID != nil {
		key += *thread.ForkedFromID
	}
	service.costMu.Lock()
	entry, ok := service.costCache[key]
	service.costMu.Unlock()
	start := int64(0)
	state := NewCostProjection(thread.ForkedFromID != nil, nil)
	if ok && entry.itemCount <= thread.ItemCount {
		start, state = entry.itemCount, entry.state.clone()
	}
	if err := service.applyCostRows(ctx, storeID, thread, state, start); err != nil {
		return nil, err
	}
	service.costMu.Lock()
	if existing, exists := service.costCache[key]; !exists || existing.itemCount <= thread.ItemCount {
		service.costCache[key] = cachedCost{thread.ItemCount, state.clone()}
	}
	for len(service.costCache) > 100 {
		for candidate := range service.costCache {
			delete(service.costCache, candidate)
			break
		}
	}
	service.costMu.Unlock()
	return state, nil
}

func (service *Service) GetThreadCost(ctx context.Context, storeID string, thread Thread) (map[string]any, error) {
	state, err := service.costProjection(ctx, storeID, thread)
	if err != nil {
		return nil, err
	}
	return service.includeSubagentCosts(ctx, storeID, thread, state)
}

func (service *Service) GetTurnCosts(ctx context.Context, storeID string, thread Thread, turnIDs []string) (map[string]map[string]any, error) {
	state, err := service.costProjection(ctx, storeID, thread)
	if err != nil {
		return nil, err
	}
	estimates := TurnCostEstimates(state)
	if turnIDs == nil {
		return estimates, nil
	}
	wanted := []string{}
	seen := map[string]bool{}
	for _, id := range turnIDs {
		if id != "" && !seen[id] {
			wanted = append(wanted, id)
			seen[id] = true
		}
	}
	missing := []string{}
	for _, id := range wanted {
		if _, ok := estimates[id]; !ok {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		targeted := NewCostProjection(thread.ForkedFromID != nil, missing)
		if err := service.applyCostRows(ctx, storeID, thread, targeted, 0); err != nil {
			return nil, err
		}
		for id, estimate := range TurnCostEstimates(targeted) {
			estimates[id] = estimate
		}
	}
	result := map[string]map[string]any{}
	for _, id := range wanted {
		if estimate, ok := estimates[id]; ok {
			result[id] = estimate
		}
	}
	return result, nil
}
