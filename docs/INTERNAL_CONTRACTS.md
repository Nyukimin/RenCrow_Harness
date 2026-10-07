# Kernel内部の値契約

RH-IMPL-001付属。これらはGoで実装する内部型の必須fieldであり、Modelが直接生成する形式ではない。外部から渡された可変map/sliceはconstructorで複製する。

| 型 | 必須field | 不変条件 |
|---|---|---|
| ModelInvocation | recovery_request, recovery_policy_revision, input_digest, base_request_digest, stage, TaskID, RunID, ActionID, AttemptID, RequestID, Binding, prompt_plan, output_contract, reserved_output_tokens, expected_digest, expected_fingerprint | stageはact/selection/summaryの内部enum。wireへは定義済み値のみ。対話IDを推論で作らない |
| ModelCompletion | generation_state, backend_attempts, applied_transformations, RequestID, ResponseID, terminal_kind, final_text, tool_intents[], usage, strict_receipt, raw_EvidenceID | terminal_kind=final/tool_calls/incomplete/refused/error。strict_receiptを検査してから採用。usage未知はnull |
| ToolIntent | ProviderToolCallID, accepted_ResponseID, ordinal, name, arguments_json, schema_revision | ordinalは同応答内0始まり。arguments_jsonは完全な単一objectの検証済みbytes。args_hashはHost計算で別途保持 |
| ToolTicket | TaskID, RunID, ActionID, AttemptID, intent_key, arguments_hash, writer_epoch, control_revision, policy_revision, scope | Prepareが発行。公開mutable structをToolへ渡しただけで認可済みとはしない |
| ToolResult | ActionID, AttemptID, effect_state, exit_code, evidence_ids[], capture_complete, error | effect_state=not_started/running/completed/failed/cancelled/unknown。exit_codeはprocess終端で判明した場合だけ値あり |
| PreparedSnapshot | ThreadID, context_revision, control_revision, queue_revision, writer_epoch, binding_revision, policy_revision, applied_entries[], pending_inputs[], latest_durable_checkpoint, latest_semantic_checkpoint, source_index | pendingとappliedを混同しない。source_indexは元rawへの検証済み参照 |
| RetentionPlan | snapshot_digest, retained_exact_refs[], removed_refs[], corrections[], completed_links[], protected_refs[] | 同じPlanをSummary入力とcandidateの両方に使用。再選別しない |
| CompletionLink | handle, target_SourceRef, call_SourceRef, output_SourceRef, terminal_EventID, ActionID, AttemptID, full_call_bytes, full_output_bytes | IMPL§10.3の条件を満たす。linkだけでは意味的完了ではない |
| SummaryCandidate | parsed_summary, stage_input_digest, handle_to_source_map, covered_context_ranges, verification_evidence_refs, raw_EvidenceID | JSON内handleの存在だけで意味正当化しない。negative素材は別の検証側へ |
| CompactionCandidate | mode, expected_revisions, snapshot_digest, retained_exact_refs, retention_plan, accepted_summary, observations, semantic_boundary, durable_boundary, before_count, after_count, serialized_bytes, serialized_hash | mode normal/emergency。immutable bytesを検査し、そのままcommitへ |
| ValidatedCandidate | package-private CompactionCandidate + validation_receipt | 外部constructorなし。commitで再CASするので検証時の型だけで安全を認定しない |
| CommitDecision | committed/rejected/unknown, ReceiptID, CheckpointID or null, new_context_revision or null, error | unknownはcheckpoint成功として公開しない |
| ControlEvent | delivery_kind, disposition, MessageID/QueueItemID, origin, accepted_sequence, control_revision, application_revision or null | delivery_kind=data/cancel/revocation、dispositionはPROTOCOLの3分類。原文はEvidence参照 |

## stage名の一致

公開LLM stageは`act`、`instruction_selection`、`work_summary`。内部enumも同じ意味を持つ。ログへ省略名selection/summaryを使う場合も既存public値との一対一mappingを固定する。unknown stageをactへdefaultしない。

## Tool schema

`schemas/tools.schema.json`に初回6Toolの入力を収録する。Hostはこのschemaをcatalogへ反映し、同じschemaで戻ったargumentsを検証する。Modelのdescriptionだけで型を強制したと扱わない。Tool呼出しの最大数/深さ/bytesはRun limitsが上限。

process.execのenvはenv_profile_refとしてhost設定から解決する。caller/Modelが任意のcredential値をargumentsへ追加できない。未登録profileは拒否。要求にはenv_profile_refが必須。未登録は拒否し、cleanへ自動fallbackしない。cleanは空valuesの明示profile、OS必須変数が必要なprofileはHOST_ASSETSで構成する。

Process以外のToolはexit_code=nullを返す。正常なfile操作も自動でTask意味達成にはしない。各Toolのraw結果をsealed Evidenceに保存し、bounded viewとcapture_completeを渡す。


有効なModel refusalは通常actではRunResult `rejected / MODEL_REFUSED`にし、実行成功にしない。Selection/SummaryのrefusalはNormalのModel failureとしてEmergency経路で扱う。未知のterminal_kindは明示contract errorである。

## 本版で追加する内部型

| 型 | 必須field | 不変条件 |
|---|---|---|
| PreparedContextBlock | ContextBlock, computed_text_digest, resolved_origin, intake_receipt_ref | 公開4fieldに対しHostが内部metadataを追加。callerからdigestを信用しない |
| RetryDecision | retry_allowed, terminal_status, reason_code, recovery_profile, delay_ms, expected_revisions | RETRY_CONTRACTの表だけから決定。LLMやclockを呼ぶ純粋関数にしない |
| RunBudgetState | effective_limits, deadline_at, recovery_policy_json/revision, model_steps_used, generation_attempts_used, generation_attempts_unknown | 予約はdispatch前transaction、unknownは返却しない |
| IntakeRecord | IntakeReceipt, raw_EvidenceID, OriginProof or null, mutation_payload_hash | 原本と署名検査・受付を同transactionで保存 |
| SourceProvenance | reference_commit, manifest_commit, manifest_component_pin, observed_binary_sha256, observed_source_commit, verification_scope | 不明値はnull。同じsourceかどうかを日付だけで推測しない |

RetryDecisionはnowや残り時間を入力値として受ける。実clock/乱数/network/state writeを内部で呼ばない。ModelInvocationの実要求はAttemptごとにEvidence保存し、Actionの論理要求を上書きしない。

## serializationを共有する内部型

LogicalInput、NormalizedRequest、Projection、ProjectionEntry、StoredSummary、ObservationReference、CheckpointCandidate、HookInput/HookResultは新しい付属schema/文書のとおり。候補の内部cache・検証済み型マーカーはcandidate_bytesへserializeしない。型のfield名と保存field名の差はserializerの明示mappingで扱う。

