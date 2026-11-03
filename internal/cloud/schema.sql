CREATE DATABASE IF NOT EXISTS agent_history;

CREATE TABLE IF NOT EXISTS agent_history.raw_events
(
    source LowCardinality(String),
    session_id String,
    event_id FixedString(64),
    event_time DateTime64(3, 'UTC'),
    event_type LowCardinality(String),
    subtype LowCardinality(String),
    turn_id String,
    activity_class LowCardinality(String),
    model LowCardinality(String),
    role LowCardinality(String),
    phase LowCardinality(String),
    record_sha256 FixedString(64),
    record_json String CODEC(ZSTD(3)),
    source_locations_json String CODEC(ZSTD(3))
)
ENGINE = ReplacingMergeTree
ORDER BY (source, session_id, event_id);

CREATE VIEW IF NOT EXISTS agent_history.messages AS
SELECT source,session_id,event_id,event_time,turn_id,model,activity_class,role,phase,
JSONExtractRaw(record_json,'payload','content') AS content_json
FROM agent_history.raw_events FINAL
WHERE event_type='response_item' AND subtype='message';

CREATE VIEW IF NOT EXISTS agent_history.tool_events AS
SELECT source,session_id,event_id,event_time,turn_id,subtype,
JSONExtractString(record_json,'payload','call_id') AS call_id,
JSONExtractString(record_json,'payload','name') AS tool_name,
JSONExtractRaw(record_json,'payload','arguments') AS arguments_json,
JSONExtractRaw(record_json,'payload','input') AS input_json,
JSONExtractRaw(record_json,'payload','output') AS output_json
FROM agent_history.raw_events FINAL
WHERE event_type='response_item' AND subtype IN ('custom_tool_call','function_call','custom_tool_call_output','function_call_output');

CREATE VIEW IF NOT EXISTS agent_history.sessions AS
SELECT source,session_id,min(event_time) AS first_event_at,max(event_time) AS last_event_at,
count() AS event_count,uniqExactIf(turn_id,turn_id!='') AS turn_count
FROM agent_history.raw_events FINAL GROUP BY source,session_id;

CREATE VIEW IF NOT EXISTS agent_history.claude_messages AS
SELECT source,session_id,event_id,event_time,turn_id,model,activity_class,role,
JSONExtractRaw(record_json,'message','content') AS content_json,
JSONExtractRaw(record_json,'message','usage') AS usage_json,
JSONExtractString(record_json,'parentUuid') AS parent_uuid
FROM agent_history.raw_events FINAL
WHERE source='claude_code' AND event_type IN ('user','assistant');

CREATE VIEW IF NOT EXISTS agent_history.claude_tool_events AS
SELECT source,session_id,event_id,event_time,turn_id,block_index,
JSONExtractString(block_json,'type') AS block_type,
if(block_type='tool_use',JSONExtractString(block_json,'id'),JSONExtractString(block_json,'tool_use_id')) AS call_id,
JSONExtractString(block_json,'name') AS tool_name,
JSONExtractRaw(block_json,'input') AS input_json,
JSONExtractRaw(block_json,'content') AS output_json,
block_json
FROM
(SELECT *,JSONExtractArrayRaw(record_json,'message','content') AS blocks FROM agent_history.raw_events FINAL WHERE source='claude_code')
ARRAY JOIN blocks AS block_json,arrayEnumerate(blocks) AS block_index
WHERE JSONExtractString(block_json,'type') IN ('tool_use','tool_result');

CREATE VIEW IF NOT EXISTS agent_history.claude_sessions AS
SELECT source,session_id,min(actual_time) AS first_event_at,max(actual_time) AS last_event_at,
count() AS event_count,uniqExactIf(turn_id,turn_id!='') AS turn_count,
countIf(isNull(actual_time)) AS records_without_timestamp
FROM
(SELECT *,parseDateTime64BestEffortOrNull(JSONExtractString(record_json,'timestamp'),3,'UTC') AS actual_time
FROM agent_history.raw_events FINAL WHERE source='claude_code')
GROUP BY source,session_id;
