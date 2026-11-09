package views

import "context"

// Tier-less legacy turn contexts must not cause one database round trip per
// turn. Use the existing model-event index and decode only the latest actual
// tier snapshot for each current generation.
func (service *Service) addServiceTiers(ctx context.Context, storeID string, threads []Thread) ([]Thread, error) {
	if len(threads) == 0 {
		return threads, nil
	}
	ids, generations, counts := threadCoordinates(threads)
	rows, err := service.pool.Query(ctx, `SELECT selected.thread_id,events.payload
	 FROM unnest($2::text[],$3::bigint[],$4::bigint[]) selected(thread_id,generation,item_count)
	 LEFT JOIN LATERAL (
	  SELECT payload FROM codex_thread_events
	  WHERE store_id=$1 AND thread_id=selected.thread_id AND generation=selected.generation AND item_seq<=selected.item_count
	   AND `+modelEventPredicate+`
	   AND ((payload->>'type'='event_msg' AND payload->'payload'->>'type'='thread_settings_applied')
	     OR (payload->>'type'='turn_context' AND payload->'payload'->'service_tier' IS NOT NULL))
	  ORDER BY item_seq DESC LIMIT 1
	 ) events ON TRUE`, storeID, ids, generations, counts)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	tiers := map[string]*string{}
	for rows.Next() {
		var id string
		var raw []byte
		if err := rows.Scan(&id, &raw); err != nil {
			return nil, err
		}
		if len(raw) > 0 {
			record, err := decodeObject(raw)
			if err != nil {
				return nil, err
			}
			tiers[id] = serviceTierFromRecord(record)
		}
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	for i := range threads {
		threads[i].ServiceTier = tiers[threads[i].ThreadID]
	}
	return threads, nil
}
