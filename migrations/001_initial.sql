-- Draft RH-STORE-001 v0.2. Apply only to a NEW, explicitly initialized Harness store.
PRAGMA foreign_keys = ON;
PRAGMA journal_mode = WAL;
PRAGMA synchronous = FULL;
CREATE TABLE schema_version(version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL);
INSERT INTO schema_version VALUES(1, strftime('%Y-%m-%dT%H:%M:%fZ','now'));
CREATE TABLE sessions(
 session_id TEXT PRIMARY KEY, principal TEXT NOT NULL, created_at TEXT NOT NULL,
 workspace_path TEXT NOT NULL, policy_ref TEXT NOT NULL, execution_mode TEXT NOT NULL
 CHECK(execution_mode IN ('structured_only','trusted_host','isolated')));
CREATE TABLE threads(
 thread_id TEXT PRIMARY KEY, session_id TEXT NOT NULL REFERENCES sessions(session_id),
 source_thread_id TEXT REFERENCES threads(thread_id), binding_json TEXT NOT NULL CHECK(json_valid(binding_json)),
 policy_revision TEXT NOT NULL, binding_revision TEXT NOT NULL,
 context_revision INTEGER NOT NULL DEFAULT 0 CHECK(context_revision>=0),
 control_revision INTEGER NOT NULL DEFAULT 0 CHECK(control_revision>=0),
 queue_revision INTEGER NOT NULL DEFAULT 0 CHECK(queue_revision>=0),
 writer_epoch INTEGER NOT NULL DEFAULT 0 CHECK(writer_epoch>=0),
 event_seq INTEGER NOT NULL DEFAULT 0 CHECK(event_seq>=0),
 current_checkpoint_id TEXT, active_run_id TEXT);
CREATE TABLE turns(
 turn_id TEXT PRIMARY KEY, thread_id TEXT NOT NULL REFERENCES threads(thread_id),
 source_message_id TEXT, root_task_id TEXT, status TEXT NOT NULL, created_at TEXT NOT NULL);
CREATE TABLE tasks(
 task_id TEXT PRIMARY KEY, thread_id TEXT NOT NULL REFERENCES threads(thread_id),
 origin_turn_id TEXT REFERENCES turns(turn_id), parent_task_id TEXT, parent_owner TEXT,
 kind TEXT NOT NULL CHECK(kind IN ('work','compaction','session_maintenance')),
 upstream_json TEXT CHECK(upstream_json IS NULL OR json_valid(upstream_json)),
 status TEXT NOT NULL, last_run_id TEXT, created_at TEXT NOT NULL);
CREATE TABLE runs(
 run_id TEXT PRIMARY KEY, task_id TEXT NOT NULL REFERENCES tasks(task_id),
 trace_id TEXT NOT NULL, resume_source_run_id TEXT REFERENCES runs(run_id),
 phase TEXT NOT NULL, status TEXT NOT NULL CHECK(status IN ('running','completed','incomplete','rejected','blocked','cancelled','failed','restart_required')),
 writer_epoch INTEGER NOT NULL, started_at TEXT NOT NULL, ended_at TEXT,
 result_json TEXT CHECK(result_json IS NULL OR json_valid(result_json)),
 limits_json TEXT NOT NULL CHECK(json_valid(limits_json)),
 deadline_at TEXT NOT NULL, recovery_policy_json TEXT NOT NULL CHECK(json_valid(recovery_policy_json)),
 recovery_policy_revision TEXT NOT NULL CHECK(length(recovery_policy_revision)=64),
 generation_attempts_used INTEGER NOT NULL DEFAULT 0 CHECK(generation_attempts_used>=0),
 generation_attempts_unknown INTEGER NOT NULL DEFAULT 0 CHECK(generation_attempts_unknown>=0 AND generation_attempts_unknown<=generation_attempts_used));
CREATE UNIQUE INDEX one_running_task ON runs(task_id) WHERE status='running';
CREATE TABLE actions(
 action_id TEXT PRIMARY KEY, run_id TEXT NOT NULL REFERENCES runs(run_id),
 kind TEXT NOT NULL CHECK(kind IN ('model','tool','measurement','verification')),
 name TEXT NOT NULL, args_bytes BLOB NOT NULL, args_hash TEXT NOT NULL CHECK(length(args_hash)=64),
 status TEXT NOT NULL, current_attempt_id TEXT, created_at TEXT NOT NULL);
CREATE TABLE attempts(
 attempt_id TEXT PRIMARY KEY, action_id TEXT NOT NULL REFERENCES actions(action_id),
 ordinal INTEGER NOT NULL CHECK(ordinal>=0),
 state TEXT NOT NULL CHECK(state IN ('prepared','dispatch_started','running','completed','failed','cancelled','unknown')),
 host_incarnation TEXT, process_token TEXT, started_at TEXT NOT NULL, ended_at TEXT,
 result_json TEXT CHECK(result_json IS NULL OR json_valid(result_json)), UNIQUE(action_id,ordinal));
CREATE TABLE tool_links(
 run_id TEXT NOT NULL REFERENCES runs(run_id), model_response_id TEXT NOT NULL,
 provider_tool_call_id TEXT NOT NULL, ordinal INTEGER NOT NULL CHECK(ordinal>=0),
 action_id TEXT NOT NULL UNIQUE REFERENCES actions(action_id), args_hash TEXT NOT NULL,
 PRIMARY KEY(run_id,model_response_id,provider_tool_call_id,ordinal));
CREATE TABLE evidence(
 evidence_id TEXT PRIMARY KEY, principal TEXT NOT NULL, owner TEXT NOT NULL,
 run_id TEXT REFERENCES runs(run_id), attempt_id TEXT REFERENCES attempts(attempt_id),
 state TEXT NOT NULL CHECK(state IN ('building','sealed')),
 capture_complete INTEGER CHECK(capture_complete IN (0,1)), total_bytes INTEGER CHECK(total_bytes>=0),
 raw_hash TEXT CHECK(raw_hash IS NULL OR length(raw_hash)=64), media_type TEXT NOT NULL,
 projection_version TEXT, projection_hash TEXT, metadata_json TEXT NOT NULL CHECK(json_valid(metadata_json)),
 CHECK(state='building' OR (raw_hash IS NOT NULL AND total_bytes IS NOT NULL AND capture_complete IS NOT NULL)));
CREATE TABLE evidence_chunks(
 evidence_id TEXT NOT NULL REFERENCES evidence(evidence_id), ordinal INTEGER NOT NULL CHECK(ordinal>=0),
 byte_start INTEGER NOT NULL CHECK(byte_start>=0), data BLOB NOT NULL,
 PRIMARY KEY(evidence_id,ordinal), UNIQUE(evidence_id,byte_start));
CREATE TABLE items(
 message_id TEXT PRIMARY KEY, thread_id TEXT NOT NULL REFERENCES threads(thread_id),
 run_id TEXT REFERENCES runs(run_id), sequence INTEGER NOT NULL CHECK(sequence>=0),
 history_kind TEXT NOT NULL, origin TEXT NOT NULL,
 evidence_id TEXT NOT NULL REFERENCES evidence(evidence_id),
 metadata_json TEXT NOT NULL CHECK(json_valid(metadata_json)), UNIQUE(thread_id,sequence));
CREATE TABLE context_entries(
 thread_id TEXT NOT NULL REFERENCES threads(thread_id), context_seq INTEGER NOT NULL CHECK(context_seq>0),
 message_id TEXT NOT NULL REFERENCES items(message_id), applied_context_revision INTEGER NOT NULL,
 PRIMARY KEY(thread_id,context_seq), UNIQUE(thread_id,message_id));
CREATE TRIGGER context_entries_no_update BEFORE UPDATE ON context_entries BEGIN SELECT RAISE(ABORT,'context application is immutable'); END;
CREATE TRIGGER context_entries_no_delete BEFORE DELETE ON context_entries BEGIN SELECT RAISE(ABORT,'context application is immutable'); END;
CREATE TABLE checkpoints(
 checkpoint_id TEXT PRIMARY KEY, thread_id TEXT NOT NULL REFERENCES threads(thread_id),
 run_id TEXT NOT NULL REFERENCES runs(run_id), parent_checkpoint_id TEXT REFERENCES checkpoints(checkpoint_id),
 mode TEXT NOT NULL CHECK(mode IN ('normal','emergency','fork')),
 context_revision INTEGER NOT NULL, control_revision INTEGER NOT NULL,
 semantic_boundary INTEGER NOT NULL CHECK(semantic_boundary>=0), durable_boundary INTEGER NOT NULL CHECK(durable_boundary>=semantic_boundary),
 candidate_bytes BLOB NOT NULL, candidate_hash TEXT NOT NULL CHECK(length(candidate_hash)=64),
 count_json TEXT NOT NULL CHECK(json_valid(count_json)), created_at TEXT NOT NULL);
CREATE TABLE receipts(
 receipt_id TEXT PRIMARY KEY, principal TEXT NOT NULL, idempotency_key TEXT NOT NULL,
 operation TEXT NOT NULL, payload_hash TEXT NOT NULL CHECK(length(payload_hash)=64),
 stage TEXT NOT NULL CHECK(stage IN ('accepted','running','terminal')),
 result_json TEXT CHECK(result_json IS NULL OR json_valid(result_json)),
 error_json TEXT CHECK(error_json IS NULL OR json_valid(error_json)),
 created_at TEXT NOT NULL, updated_at TEXT NOT NULL, UNIQUE(principal,idempotency_key));
CREATE TABLE queue_inputs(
 queue_item_id TEXT PRIMARY KEY, thread_id TEXT NOT NULL REFERENCES threads(thread_id),
 run_id TEXT REFERENCES runs(run_id), message_id TEXT NOT NULL REFERENCES items(message_id),
 receipt_id TEXT NOT NULL REFERENCES receipts(receipt_id),
 disposition TEXT NOT NULL CHECK(disposition IN ('next_step','interrupt_current','next_turn')),
 delivery_state TEXT NOT NULL CHECK(delivery_state IN ('queued','applied','deferred')),
 queue_revision INTEGER NOT NULL, applied_context_revision INTEGER,
 created_at TEXT NOT NULL, UNIQUE(thread_id,queue_revision));
CREATE TABLE events(
 event_id TEXT PRIMARY KEY, thread_id TEXT NOT NULL REFERENCES threads(thread_id),
 event_seq INTEGER NOT NULL CHECK(event_seq>0), task_id TEXT REFERENCES tasks(task_id), run_id TEXT REFERENCES runs(run_id),
 type TEXT NOT NULL, causation_event_id TEXT, dependency_event_ids_json TEXT NOT NULL CHECK(json_valid(dependency_event_ids_json)),
 receipt_id TEXT REFERENCES receipts(receipt_id), evidence_id TEXT REFERENCES evidence(evidence_id),
 payload_json TEXT NOT NULL CHECK(json_valid(payload_json)), recorded_at TEXT NOT NULL,
 UNIQUE(thread_id,event_seq));
CREATE TABLE model_calls(
 request_id TEXT PRIMARY KEY, response_id TEXT, run_id TEXT NOT NULL REFERENCES runs(run_id),
 action_id TEXT NOT NULL REFERENCES actions(action_id), attempt_id TEXT NOT NULL REFERENCES attempts(attempt_id),
 stage TEXT NOT NULL CHECK(stage IN ('act','instruction_selection','work_summary')),
 request_digest TEXT NOT NULL, binding_fingerprint TEXT NOT NULL,
 request_evidence_id TEXT REFERENCES evidence(evidence_id), response_evidence_id TEXT REFERENCES evidence(evidence_id),
 logical_requests INTEGER NOT NULL CHECK(logical_requests=1), backend_attempts INTEGER CHECK(backend_attempts IN (0,1)),
 generation_state TEXT NOT NULL CHECK(generation_state IN ('not_started','terminal','unknown')),
 attempt_ordinal INTEGER NOT NULL CHECK(attempt_ordinal IN (0,1)),
 recovery_profile TEXT NOT NULL CHECK(recovery_profile IN ('same_request','terminal_output_once')),
 recovery_profile_revision TEXT NOT NULL, base_request_digest TEXT NOT NULL CHECK(length(base_request_digest)=64),
 applied_transformations_json TEXT NOT NULL CHECK(json_valid(applied_transformations_json)),
 receipt_json TEXT CHECK(receipt_json IS NULL OR json_valid(receipt_json)), usage_json TEXT CHECK(usage_json IS NULL OR json_valid(usage_json)),
 UNIQUE(attempt_id),
 CHECK((generation_state='unknown') = (backend_attempts IS NULL)),
 CHECK((generation_state='not_started' AND backend_attempts=0) OR (generation_state='terminal' AND backend_attempts=1) OR (generation_state='unknown' AND backend_attempts IS NULL)),
 CHECK(stage='act' OR (attempt_ordinal=0 AND recovery_profile='same_request')));
CREATE TRIGGER evidence_sealed_no_update BEFORE UPDATE ON evidence WHEN OLD.state='sealed'
 BEGIN SELECT RAISE(ABORT,'sealed evidence is immutable'); END;
CREATE TRIGGER evidence_no_delete BEFORE DELETE ON evidence BEGIN SELECT RAISE(ABORT,'evidence deletion requires offline lifecycle'); END;
CREATE TRIGGER chunks_no_update BEFORE UPDATE ON evidence_chunks BEGIN SELECT RAISE(ABORT,'raw chunks are immutable'); END;
CREATE TRIGGER chunks_no_delete BEFORE DELETE ON evidence_chunks BEGIN SELECT RAISE(ABORT,'raw chunks are immutable'); END;
CREATE TRIGGER chunks_only_building BEFORE INSERT ON evidence_chunks WHEN (SELECT state FROM evidence WHERE evidence_id=NEW.evidence_id)!='building'
 BEGIN SELECT RAISE(ABORT,'evidence is sealed'); END;
CREATE TRIGGER items_no_update BEFORE UPDATE ON items BEGIN SELECT RAISE(ABORT,'items are immutable'); END;
CREATE TRIGGER items_no_delete BEFORE DELETE ON items BEGIN SELECT RAISE(ABORT,'items are immutable'); END;
CREATE TRIGGER checkpoints_no_update BEFORE UPDATE ON checkpoints BEGIN SELECT RAISE(ABORT,'checkpoint is immutable'); END;
CREATE TRIGGER checkpoints_no_delete BEFORE DELETE ON checkpoints BEGIN SELECT RAISE(ABORT,'checkpoint is immutable'); END;
CREATE TRIGGER events_no_update BEFORE UPDATE ON events BEGIN SELECT RAISE(ABORT,'events are immutable'); END;
CREATE TRIGGER events_no_delete BEFORE DELETE ON events BEGIN SELECT RAISE(ABORT,'events are immutable'); END;

CREATE TABLE relay_nonces(
 issuer TEXT NOT NULL, key_id TEXT NOT NULL, nonce TEXT NOT NULL, payload_hash TEXT NOT NULL CHECK(length(payload_hash)=64),
 receipt_id TEXT NOT NULL REFERENCES receipts(receipt_id), accepted_at TEXT NOT NULL,
 PRIMARY KEY(issuer,key_id,nonce));

-- WP06 stage 2 (session/fork, F25). Added to the draft DDL; nothing above it changed.
-- The provenance a forked Thread imports: a fork checkpoint names sources (Evidence, and the
-- checkpoint that accepted its Summary) that belong to the Thread it was forked from, or that
-- Thread imported itself. The forked Thread may read, name and verify exactly these and no
-- other source of another Thread; the rows are written once, with the fork, and never changed.
CREATE TABLE source_imports(
 thread_id TEXT NOT NULL REFERENCES threads(thread_id),
 source_kind TEXT NOT NULL CHECK(source_kind IN ('evidence','checkpoint')),
 source_id TEXT NOT NULL,
 from_thread_id TEXT NOT NULL REFERENCES threads(thread_id),
 from_checkpoint_id TEXT NOT NULL REFERENCES checkpoints(checkpoint_id),
 fork_checkpoint_id TEXT NOT NULL REFERENCES checkpoints(checkpoint_id),
 PRIMARY KEY(thread_id, source_kind, source_id));
CREATE TRIGGER source_imports_no_update BEFORE UPDATE ON source_imports BEGIN SELECT RAISE(ABORT,'a provenance import is immutable'); END;
CREATE TRIGGER source_imports_no_delete BEFORE DELETE ON source_imports BEGIN SELECT RAISE(ABORT,'a provenance import is immutable'); END;
-- Nothing is applied to a Thread's context at or before the durable boundary of the checkpoint
-- it stands on: the snapshot reads what came after it, so such an entry would never be seen.
CREATE TRIGGER context_entries_after_checkpoint BEFORE INSERT ON context_entries
 WHEN NEW.context_seq <= COALESCE((SELECT c.durable_boundary FROM threads t JOIN checkpoints c ON c.checkpoint_id=t.current_checkpoint_id WHERE t.thread_id=NEW.thread_id),0)
 BEGIN SELECT RAISE(ABORT,'an entry is applied after the durable boundary of the current checkpoint'); END;
