package foundation

// Rebuildable read projection; native events remain authoritative.
const claudeUsageSQL = `ALTER TABLE mira_claude_turns ADD COLUMN node_account_id UUID, ADD COLUMN account_name TEXT;
CREATE TABLE mira_claude_usage (
 turn_id UUID PRIMARY KEY REFERENCES mira_claude_turns ON DELETE CASCADE,
 session_id UUID NOT NULL REFERENCES mira_claude_sessions ON DELETE CASCADE,
 revision BIGINT NOT NULL, happened_at TIMESTAMPTZ NOT NULL,
 account_name TEXT, node_account_id UUID,
 source_seq BIGINT NOT NULL DEFAULT 0,
 amount NUMERIC, input BIGINT, output BIGINT, cached BIGINT, cache_write BIGINT,
 self_input BIGINT, self_output BIGINT, self_cached BIGINT,
 UNIQUE(session_id,revision)
);
CREATE INDEX mira_claude_usage_account_time ON mira_claude_usage(account_name,happened_at);
CREATE INDEX mira_claude_result_events ON mira_claude_events(turn_id,seq DESC) WHERE event_type='result';
CREATE FUNCTION mira_claude_number(value TEXT) RETURNS NUMERIC LANGUAGE SQL IMMUTABLE AS $$
 SELECT CASE WHEN length(value)<32 AND value ~ '^[0-9]+(\.[0-9]+)?([eE][+-]?[0-9]{1,2})?$'
 THEN CASE WHEN value::numeric BETWEEN 0 AND 9007199254740991 THEN value::numeric END END
$$;
CREATE FUNCTION mira_claude_usage_owner() RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
 INSERT INTO mira_claude_usage(turn_id,session_id,revision,happened_at,node_account_id,account_name)
 VALUES(NEW.turn_id,NEW.session_id,NEW.revision,NEW.created_at,NEW.node_account_id,NEW.account_name);
 RETURN NEW;
END $$;
CREATE TRIGGER mira_claude_usage_owner AFTER INSERT ON mira_claude_turns FOR EACH ROW EXECUTE FUNCTION mira_claude_usage_owner();
-- Historical binding is only known when present in the immutable turn request.
-- JSON text extraction can reject escaped NUL in unrelated raw fields. The raw
-- write must always survive an unprojectable value.
CREATE FUNCTION mira_claude_request_account(value JSON) RETURNS TEXT LANGUAGE plpgsql IMMUTABLE AS $$
BEGIN RETURN value->>'nodeAccountId';
EXCEPTION WHEN untranslatable_character THEN RETURN NULL;
END $$;
UPDATE mira_claude_turns t SET node_account_id=a.node_account_id,account_name=a.name
 FROM mira_claude_accounts a WHERE a.node_account_id::text=mira_claude_request_account(t.request);
INSERT INTO mira_claude_usage(turn_id,session_id,revision,happened_at,node_account_id,account_name)
 SELECT turn_id,session_id,revision,created_at,node_account_id,account_name FROM mira_claude_turns;
CREATE FUNCTION mira_claude_project_result(id UUID, sequence BIGINT, payload JSON) RETURNS VOID LANGUAGE plpgsql AS $$
DECLARE tokens JSON; i NUMERIC; o NUMERIC; c NUMERIC; w NUMERIC;
BEGIN
 IF payload->>'parent_tool_use_id' IS NOT NULL THEN RETURN; END IF;
 tokens=payload->'modelUsage';
 IF json_typeof(tokens)='object' THEN
 SELECT CASE WHEN count(mira_claude_number(value->>'inputTokens'))=count(*) THEN sum(mira_claude_number(value->>'inputTokens')) END,
 CASE WHEN count(mira_claude_number(value->>'outputTokens'))=count(*) THEN sum(mira_claude_number(value->>'outputTokens')) END,
 CASE WHEN count(mira_claude_number(value->>'cacheReadInputTokens'))=count(*) THEN sum(mira_claude_number(value->>'cacheReadInputTokens')) END,
 CASE WHEN count(mira_claude_number(value->>'cacheCreationInputTokens'))=count(*) THEN sum(mira_claude_number(value->>'cacheCreationInputTokens')) END
 INTO i,o,c,w FROM json_each(tokens);
 END IF;
 UPDATE mira_claude_usage SET source_seq=sequence,amount=mira_claude_number(payload->>'total_cost_usd'),
 input=CASE WHEN i+c+w<=9007199254740991 THEN i+c+w END,
 output=CASE WHEN o<=9007199254740991 THEN o END,cached=CASE WHEN c<=9007199254740991 THEN c END,
 cache_write=CASE WHEN w<=9007199254740991 THEN w END,
 self_input=mira_claude_number(payload->'usage'->>'input_tokens')+mira_claude_number(payload->'usage'->>'cache_read_input_tokens')+mira_claude_number(payload->'usage'->>'cache_creation_input_tokens'),
 self_output=mira_claude_number(payload->'usage'->>'output_tokens'),self_cached=mira_claude_number(payload->'usage'->>'cache_read_input_tokens')
 WHERE turn_id=id AND source_seq<sequence;
EXCEPTION WHEN untranslatable_character OR invalid_text_representation OR numeric_value_out_of_range THEN
 UPDATE mira_claude_usage SET source_seq=sequence,amount=NULL,input=NULL,output=NULL,cached=NULL,cache_write=NULL,self_input=NULL,self_output=NULL,self_cached=NULL
 WHERE turn_id=id AND source_seq<sequence;
END $$;
CREATE FUNCTION mira_claude_usage_event() RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
 IF NEW.event_type='result' THEN PERFORM mira_claude_project_result(NEW.turn_id,NEW.seq,NEW.payload); END IF;
 RETURN NEW;
END $$;
CREATE TRIGGER mira_claude_usage_event AFTER INSERT ON mira_claude_events FOR EACH ROW EXECUTE FUNCTION mira_claude_usage_event();
DO $$ DECLARE e RECORD; BEGIN
 FOR e IN SELECT DISTINCT ON(turn_id) turn_id,seq,payload FROM mira_claude_events WHERE event_type='result' ORDER BY turn_id,seq DESC
 LOOP PERFORM mira_claude_project_result(e.turn_id,e.seq,e.payload); END LOOP;
END $$;
-- SDK totals include restored session spend and subagents. Difference consecutive
-- results once, before filtering by account/date. Missing/reset baselines stay partial.
CREATE VIEW mira_claude_usage_deltas AS
 WITH previous AS (SELECT *,row_number() OVER s AS ordinal,
 lag(amount) OVER s AS prev_amount,lag(input) OVER s AS prev_input,
 lag(output) OVER s AS prev_output,lag(cached) OVER s AS prev_cached,
 lag(cache_write) OVER s AS prev_write FROM mira_claude_usage
 WINDOW s AS(PARTITION BY session_id ORDER BY revision))
 SELECT *,
 CASE WHEN ordinal=1 THEN amount WHEN prev_amount IS NULL THEN NULL WHEN amount>=prev_amount THEN amount-prev_amount ELSE amount END AS cost_delta,
 CASE WHEN ordinal=1 THEN input WHEN prev_input IS NULL THEN NULL WHEN input>=prev_input THEN input-prev_input ELSE input END AS input_delta,
 CASE WHEN ordinal=1 THEN output WHEN prev_output IS NULL THEN NULL WHEN output>=prev_output THEN output-prev_output ELSE output END AS output_delta,
 CASE WHEN ordinal=1 THEN cached WHEN prev_cached IS NULL THEN NULL WHEN cached>=prev_cached THEN cached-prev_cached ELSE cached END AS cached_delta,
 CASE WHEN ordinal=1 THEN cache_write WHEN prev_write IS NULL THEN NULL WHEN cache_write>=prev_write THEN cache_write-prev_write ELSE cache_write END AS write_delta,
 amount IS NULL OR (ordinal>1 AND (prev_amount IS NULL OR amount<prev_amount)) AS cost_partial,
 input IS NULL OR output IS NULL OR (ordinal>1 AND (prev_input IS NULL OR prev_output IS NULL OR input<prev_input OR output<prev_output)) AS usage_partial
 FROM previous;
`

// Running turns are priced from their API responses until the SDK result lands.
const claudeLiveCostSQL = `CREATE INDEX mira_claude_assistant_events ON mira_claude_events(turn_id,seq) WHERE event_type='assistant';
`
