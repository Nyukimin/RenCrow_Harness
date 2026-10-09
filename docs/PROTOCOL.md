# Native API・CLI契約

本書と`schemas/protocol.schema.json`がRH-IMPL-001の公開DTO付属仕様。全APIは新設案であり、現行CORE/LLM/Codexに存在するとは主張しない。

## 1. Transport

`rencrow-harness serve --stdio --config <absolute-config-path>`をclientが子processとして起動する。UTF-8のJSON-RPC 2.0 request/response/notificationを1行1objectのNDJSONで送る。stdoutはprotocolだけ、stderrは機密を含まない診断だけ。改行はJSON内でescapeする。1frame最大16MiB、深さ64、重複key/trailing値/invalid UTF-8は拒否。batch requestは初回unsupportedとしてinvalid requestを返す。

RPCの`id`は一回の通信を表すCanonical RequestID。responseは同idを返す。再送時は新idと同idempotency_keyを使う。notificationにidはない。protocol自体はJSON-RPC 2.0の意味に従うが、method/schemaは独自でCodex App Server、ACP、MCPとは互換を主張しない。[O01][O03]

initializeを最初に要求し、protocol_versionの一致を確認する。初回のversion文字列は`rencrow-harness/v1`で、schemaのdraft状態と製品releaseを混同しない。未知method、未知required field、対応しないversionを無視して続行しない。

1connectionの主体・アクセス可能workspace・policy上限は起動時のtrusted host profileから固定。payloadでcaller identityやallow-allを自己申告できない。COREも人も同じService APIを使い、入口だけが違う。

## 2. CLI

以下は実装後に提供するcommand。現在の実行済みcommandではない。

```text
rencrow-harness init --config ABS --data-root ABS
rencrow-harness chat --config ABS --workspace ABS --binding PROFILE
rencrow-harness exec --config ABS --workspace ABS --binding PROFILE --input-file ABS --json
rencrow-harness sessions list --config ABS
rencrow-harness inspect --config ABS --run RUN_ID --json
rencrow-harness resume --config ABS --task TASK_ID --last-run RUN_ID --json
rencrow-harness compact --config ABS --thread THREAD_ID --json
rencrow-harness evidence --config ABS --id EVIDENCE_ID --start N --end N
rencrow-harness serve --stdio --config ABS
```

chatは初回に行入力とstream表示を提供すればよく、Codex同等のフルスクリーンTUIは不要。`/stop`、`/compact`、`/status`、`/exit`をService操作へ変換する。対話中の普通の追記はnext_step、明示`/interrupt <text>`はinterrupt_current、`/later <text>`はnext_turn。busyのmanual compactを自動queueしない。

execのJSONLは`{type:"event",event:Event}`と、最後の`{type:"result",result:RunResult}`だけ。UIの装飾文字を混ぜない。終了codeはcompleted=0、incomplete=2、rejected/blocked=3、cancelled=4、failed=5、restart_required=6、CLI/config/protocol不正=64。ただし0でもTask意味達成ではなくRunResult.verificationを読む。

stdin pipe切断はcancel相当。未取得のdurable結果はinspect/receiptで取得できる。TTY Ctrl-Cもcancelを記録し、有限時間内にprocess treeを停止/照会する。単にSIGINTを送っただけでcancelled確定としない。

## 3. API一覧

| method | params型 | result型 | 意味 |
|---|---|---| 
| `initialize` | InitializeInput | CapabilitiesResult | 初期handshake。client名は認証にしない。 |
| `service/capabilities` | EmptyInput | CapabilitiesResult | 利用可能な実装/接続/予算の状態。 |
| `session/open` | SessionOpenInput | SessionOpenResult | 新Session/Threadをdurable作成。Model生成なし。 |
| `session/list` | SessionListInput | SessionListResult | callerが読めるSessionのみ。 |
| `session/get` | SessionGetInput | SessionInfo | 同じownerの現在metadata。 |
| `session/fork` | SessionForkInput | ForkResult | 確定checkpointから別Thread。filesystem復元なし。 |
| `turn/start` | StartInput | StartResult | 新Turn/Task/Run受付。作業完了ではない。 |
| `input/append` | InputAppendInput | InputReceipt | 追記の分類をdurable保存。 |
| `turn/interrupt` | InterruptInput | InterruptReceipt | 停止信号を記録。外部停止完了とは別。 |
| `run/get` | RunGetInput | RunInfo | 現在phaseまたは7分類の確定RunResult。 |
| `run/resume` | ResumeInput | ResumeResult | 同Taskに新Run/Trace。replayの後だけ実行。 |
| `context/compact` | CompactInput | OperationAccepted | idle Threadのmanual compact。receiptで結果取得。 |
| `receipt/get` | ReceiptGetInput | ReceiptRecord | 応答消失後も同operationの結果を読む。 |
| `events/read` | EventsReadInput | EventsReadResult | 確定eventの再取得。provisionalは含まない。 |
| `evidence/read` | EvidenceReadInput | EvidenceReadResult | 最大64KiBの原本/投影範囲。元Toolを実行しない。 |
| `service/shutdown` | ShutdownInput | ShutdownResult | 新規受付停止。drain/cancelと有限deadline。 |

## 4. 入力とmutationの共通意味

全mutationはcaller scope内のidempotency_keyを持つ。処理前にBYTE_CONTRACTS§4のmutation_payload_hashを作る。通信id/送受信時刻をdigestから除き、workspace、binding、期待revision、本文、限度を含める。同key・同payloadは以前のreceipt/result。同key・異payloadはIDEMPOTENCY_CONFLICT。期限を過ぎたから同keyで新しい副作用を作ってはいけない。recordのretentionはTaskと同じである。

expected revisionはlost update防止。再送は最初に同keyの既存receiptを照会し、それからrevisionを検査する。成功後の再送を古いexpected revisionだからと拒否しない。

`upstream=null`はstandalone。CORE委譲はupstream.owner=RenCrow_COREとcanonical親Task等を付けるが、その値だけでauthorityを与えない。binding.kind=aliasではagent_id/execution_roleが必須で、LLM Gatewayのbindingと一致する。model_routeではnullを許す。両方ともprofile_revisionは解決済みの値で、`latest`は許さない。

ContextBlockのsourceは既存証拠の参照であり、callerがsourceを付けたことだけでHuman認証にならない。sourceがないActor/Stableの本文はcaller profile由来としてHarnessが新規intakeし、hostの権限を超えることはできない。

## 5. 型の正式field

全fieldは表でYesのものを常に送る。optionalをnullで表すfieldと、省略可能を混同しない。IDのprefix/UUIDはschemaを参照。bytes/rangeはUTF-8半開区間。型に加えて以下§6〜8の意味条件を必ず検査する。

### ErrorInfo

| field | 型・構造 | 必須 |
|---|---|---|
| `code` | string | Yes |
| `message` | string | Yes |
| `retryable` | boolean | Yes |
| `evidence_id` | EvidenceID / null | Yes |

### ByteRange

| field | 型・構造 | 必須 |
|---|---|---|
| `start` | integer | Yes |
| `end` | integer | Yes |

### SourceRef

| field | 型・構造 | 必須 |
|---|---|---|
| `owner` | string | Yes |
| `source_id` | string | Yes |
| `raw_hash` | string | Yes |
| `projection_version` | string | Yes |
| `range` | ByteRange | Yes |
| `origin` | human / automation / host / agent / tool / unknown | Yes |
| `sequence` | integer | Yes |

### ContextBlock

| field | 型・構造 | 必須 |
|---|---|---|
| `kind` | character_system_prompt / stable_runtime_context / recall_pack / variable_runtime_context / user_message | Yes |
| `text` | string | Yes |
| `revision` | string | Yes |
| `source` | SourceRef / null | Yes |
### Binding

| field | 型・構造 | 必須 |
|---|---|---|
| `kind` | alias / model_route | Yes |
| `selector` | string | Yes |
| `profile_revision` | string | Yes |
| `agent_id` | string / null | Yes |
| `execution_role` | string / null | Yes |

### Upstream

| field | 型・構造 | 必須 |
|---|---|---|
| `owner` | string | Yes |
| `task_id` | TaskID | Yes |
| `trace_id` | TraceID | Yes |
| `session_id` | SessionID / null | Yes |
| `thread_id` | ThreadID / null | Yes |
| `turn_id` | TurnID / null | Yes |
| `action_id` | ActionID / null | Yes |
| `attempt_id` | AttemptID / null | Yes |

### OriginProof

| field | 型・構造 | 必須 |
|---|---|---|
| `issuer` | string | Yes |
| `key_id` | string | Yes |
| `audience` | string | Yes |
| `origin` | human / automation | Yes |
| `source_message_id` | MessageID | Yes |
| `source_thread_id` | ThreadID | Yes |
| `destination_thread_id` | ThreadID | Yes |
| `mutation_key` | MutationKey | Yes |
| `raw_hash` | string | Yes |
| `sequence` | integer | Yes |
| `issued_at` | string | Yes |
| `expires_at` | string | Yes |
| `nonce` | string | Yes |
| `mac` | string | Yes |
### InputMessage

| field | 型・構造 | 必須 |
|---|---|---|
| `text` | string | Yes |
| `origin_proof` | OriginProof / null | Yes |

### Limits

| field | 型・構造 | 必須 |
|---|---|---|
| `max_model_steps` | integer | Yes |
| `max_tool_calls_per_step` | integer | Yes |
| `deadline_seconds` | integer | Yes |
| `max_capture_bytes` | integer | Yes |
| `max_generation_attempts` | integer | Yes |
### StartInput

| field | 型・構造 | 必須 |
|---|---|---|
| `thread_id` | ThreadID | Yes |
| `input` | InputMessage | Yes |
| `context_blocks` | ContextBlock[] | Yes |
| `upstream` | Upstream / null | Yes |
| `expected_context_revision` | integer | Yes |
| `expected_control_revision` | integer | Yes |
| `idempotency_key` | MutationKey | Yes |
| `limits` | Limits | Yes |

### StartResult

| field | 型・構造 | 必須 |
|---|---|---|
| `receipt_id` | ReceiptID | Yes |
| `accepted` | `true` | Yes |
| `session_id` | SessionID | Yes |
| `thread_id` | ThreadID | Yes |
| `turn_id` | TurnID | Yes |
| `task_id` | TaskID | Yes |
| `run_id` | RunID | Yes |
| `trace_id` | TraceID | Yes |
| `effective_limits` | Limits | Yes |
| `deadline_at` | string | Yes |
| `recovery_policy_revision` | string | Yes |
| `intake` | IntakeReceipt | Yes |
### ResumeInput

| field | 型・構造 | 必須 |
|---|---|---|
| `task_id` | TaskID | Yes |
| `expected_last_run_id` | RunID | Yes |
| `checkpoint_id` | CheckpointID / null | Yes |
| `expected_control_revision` | integer | Yes |
| `binding` | Binding / null | Yes |
| `idempotency_key` | MutationKey | Yes |
| `limits` | Limits | Yes |
### ResumeResult

| field | 型・構造 | 必須 |
|---|---|---|
| `receipt_id` | ReceiptID | Yes |
| `task_id` | TaskID | Yes |
| `thread_id` | ThreadID | Yes |
| `previous_run_id` | RunID | Yes |
| `run_id` | RunID | Yes |
| `trace_id` | TraceID | Yes |
| `checkpoint_id` | CheckpointID / null | Yes |
| `effective_limits` | Limits | Yes |
| `deadline_at` | string | Yes |
| `recovery_policy_revision` | string | Yes |
### InputAppendInput

| field | 型・構造 | 必須 |
|---|---|---|
| `thread_id` | ThreadID | Yes |
| `run_id` | RunID | Yes |
| `input` | InputMessage | Yes |
| `disposition` | next_step / interrupt_current / next_turn | Yes |
| `expected_control_revision` | integer | Yes |
| `idempotency_key` | MutationKey | Yes |

### InputReceipt

| field | 型・構造 | 必須 |
|---|---|---|
| `receipt_id` | ReceiptID | Yes |
| `queue_item_id` | QueueItemID | Yes |
| `message_id` | MessageID | Yes |
| `origin` | human / automation / unknown | Yes |
| `disposition` | next_step / interrupt_current / next_turn | Yes |
| `delivery_state` | queued / applied / deferred | Yes |
| `queue_revision` | integer | Yes |
| `control_revision` | integer | Yes |
| `intake` | IntakeReceipt | Yes |
### BudgetReport

| field | 型・構造 | 必須 |
|---|---|---|
| `state` | verified_exact / verified_bound / estimated / unverified | Yes |
| `prompt_lower` | integer / null | Yes |
| `prompt_upper` | integer / null | Yes |
| `context_limit` | integer / null | Yes |
| `reserved_output_tokens` | integer | Yes |
| `safety_margin_tokens` | integer | Yes |
| `request_digest` | string | Yes |
| `normalization_fingerprint` | string | Yes |
| `evidence_ref` | string / null | Yes |

### CompactInput

| field | 型・構造 | 必須 |
|---|---|---|
| `thread_id` | ThreadID | Yes |
| `expected_context_revision` | integer | Yes |
| `expected_control_revision` | integer | Yes |
| `dry_run` | boolean | Yes |
| `idempotency_key` | MutationKey | Yes |

### CompactResult

| field | 型・構造 | 必須 |
|---|---|---|
| `status` | executed / cancelled / stale / unavailable / dry_run | Yes |
| `outcome` | NormalCompacted / EmergencyCompacted / CapacityBlocked / IntegrityBlocked / RestartRequired / null | Yes |
| `checkpoint_id` | CheckpointID / null | Yes |
| `before` | BudgetReport / null | Yes |
| `after` | BudgetReport / null | Yes |
| `required_minimum_tokens` | integer / null | Yes |
| `available_tokens` | integer / null | Yes |
| `semantic_boundary` | integer / null | Yes |
| `durable_boundary` | integer / null | Yes |
| `error` | ErrorInfo / null | Yes |

### Verification

| field | 型・構造 | 必須 |
|---|---|---|
| `status` | passed / failed / not_run / unknown | Yes |
| `evidence_ids` | EvidenceID[] | Yes |
| `criteria_revision` | string / null | Yes |

### RunResult

| field | 型・構造 | 必須 |
|---|---|---|
| `run_id` | RunID | Yes |
| `task_id` | TaskID | Yes |
| `status` | completed / incomplete / rejected / blocked / cancelled / failed / restart_required | Yes |
| `code` | string | Yes |
| `final_message_id` | MessageID / null | Yes |
| `final_text` | string | Yes |
| `verification` | Verification | Yes |
| `evidence_ids` | EvidenceID[] | Yes |
| `unresolved_action_ids` | ActionID[] | Yes |
| `last_checkpoint_id` | CheckpointID / null | Yes |
| `resumable` | boolean | Yes |

### RunInfo

| field | 型・構造 | 必須 |
|---|---|---|
| `run_id` | RunID | Yes |
| `task_id` | TaskID | Yes |
| `thread_id` | ThreadID | Yes |
| `phase` | Admitting / Loading / Assembling / Measuring / Generating / PreparingAction / Executing / PersistingObservation / Compacting / CommittingCheckpoint / ValidatingFinal / PersistingResult / Cancelling / Recovering / RetryWaiting / Terminal | Yes |
| `terminal` | boolean | Yes |
| `result` | RunResult / null | Yes |
| `context_revision` | integer | Yes |
| `control_revision` | integer | Yes |
| `last_event_seq` | integer | Yes |
| `effective_limits` | Limits | Yes |
| `deadline_at` | string | Yes |
| `recovery_policy_revision` | string | Yes |
| `generation_attempts_used` | integer | Yes |
| `generation_attempts_unknown` | integer | Yes |
### SessionInfo

| field | 型・構造 | 必須 |
|---|---|---|
| `session_id` | SessionID | Yes |
| `thread_id` | ThreadID | Yes |
| `workspace_path` | string | Yes |
| `binding` | Binding | Yes |
| `policy_ref` | string | Yes |
| `execution_mode` | structured_only / trusted_host / isolated | Yes |
| `context_revision` | integer | Yes |
| `control_revision` | integer | Yes |
| `active_run_id` | RunID / null | Yes |

### InitializeInput

| field | 型・構造 | 必須 |
|---|---|---|
| `client_name` | string | Yes |
| `client_version` | string | Yes |
| `protocol_version` | `"rencrow-harness/v1"` | Yes |

### Capability

| field | 型・構造 | 必須 |
|---|---|---|
| `name` | string | Yes |
| `status` | ready / disabled / unavailable / unverified | Yes |
| `basis` | declared / contract_tested / runtime_verified | Yes |
| `reason` | string / null | Yes |

### CapabilitiesResult

| field | 型・構造 | 必須 |
|---|---|---|
| `protocol_version` | `"rencrow-harness/v1"` | Yes |
| `build_revision` | string | Yes |
| `capabilities` | Capability[] | Yes |

### SessionOpenInput

| field | 型・構造 | 必須 |
|---|---|---|
| `workspace_path` | string | Yes |
| `binding` | Binding | Yes |
| `policy_ref` | string | Yes |
| `execution_mode` | structured_only / trusted_host / isolated | Yes |
| `idempotency_key` | MutationKey | Yes |

### SessionOpenResult

| field | 型・構造 | 必須 |
|---|---|---|
| `receipt_id` | ReceiptID | Yes |
| `session` | SessionInfo | Yes |

### SessionListInput

| field | 型・構造 | 必須 |
|---|---|---|
| `cursor` | string / null | Yes |
| `limit` | integer | Yes |

### SessionListResult

| field | 型・構造 | 必須 |
|---|---|---|
| `sessions` | SessionInfo[] | Yes |
| `next_cursor` | string / null | Yes |

### SessionGetInput

| field | 型・構造 | 必須 |
|---|---|---|
| `thread_id` | ThreadID | Yes |

### SessionForkInput

| field | 型・構造 | 必須 |
|---|---|---|
| `thread_id` | ThreadID | Yes |
| `checkpoint_id` | CheckpointID | Yes |
| `idempotency_key` | MutationKey | Yes |

### ForkResult

| field | 型・構造 | 必須 |
|---|---|---|
| `receipt_id` | ReceiptID | Yes |
| `source_thread_id` | ThreadID | Yes |
| `thread_id` | ThreadID | Yes |
| `session_id` | SessionID | Yes |
| `checkpoint_id` | CheckpointID | Yes |

### InterruptInput

| field | 型・構造 | 必須 |
|---|---|---|
| `run_id` | RunID | Yes |
| `expected_control_revision` | integer | Yes |
| `idempotency_key` | MutationKey | Yes |

### InterruptReceipt

| field | 型・構造 | 必須 |
|---|---|---|
| `receipt_id` | ReceiptID | Yes |
| `run_id` | RunID | Yes |
| `signal_recorded` | boolean | Yes |
| `control_revision` | integer | Yes |
| `code` | CANCEL_REQUESTED / ALREADY_TERMINAL | Yes |

### RunGetInput

| field | 型・構造 | 必須 |
|---|---|---|
| `run_id` | RunID | Yes |

### OperationAccepted

| field | 型・構造 | 必須 |
|---|---|---|
| `receipt_id` | ReceiptID | Yes |
| `accepted` | `true` | Yes |

### ReceiptGetInput

| field | 型・構造 | 必須 |
|---|---|---|
| `receipt_id` | ReceiptID | Yes |

### ReceiptRecord

| field | 型・構造 | 必須 |
|---|---|---|
| `receipt_id` | ReceiptID | Yes |
| `operation` | string | Yes |
| `stage` | accepted / running / terminal | Yes |
| `result` | ReceiptPayload / null | Yes |
| `error` | ErrorInfo / null | Yes |

### EventsReadInput

| field | 型・構造 | 必須 |
|---|---|---|
| `thread_id` | ThreadID | Yes |
| `after_seq` | integer | Yes |
| `limit` | integer | Yes |

### Event

| field | 型・構造 | 必須 |
|---|---|---|
| `event_id` | EventID | Yes |
| `event_seq` | integer | Yes |
| `thread_id` | ThreadID | Yes |
| `task_id` | TaskID / null | Yes |
| `run_id` | RunID / null | Yes |
| `type` | session.created / input.accepted / input.applied / task.created / run.started / model.requested / model.completed / action.prepared / action.dispatch_started / action.completed / checkpoint.committed / control.cancel_requested / run.terminal / model.retry_scheduled / model.attempt_started | Yes |
| `receipt_id` | ReceiptID / null | Yes |
| `evidence_id` | EvidenceID / null | Yes |
| `message_id` | MessageID / null | Yes |
| `code` | string / null | Yes |
| `recorded_at` | string | Yes |
| `payload` | object | Yes |


### EventsReadResult

| field | 型・構造 | 必須 |
|---|---|---|
| `events` | Event[] | Yes |
| `next_after_seq` | integer | Yes |
| `has_more` | boolean | Yes |

### EvidenceReadInput

| field | 型・構造 | 必須 |
|---|---|---|
| `evidence_id` | EvidenceID | Yes |
| `projection_version` | raw/v1 / text/v1 | Yes |
| `range` | ByteRange | Yes |

### EvidenceReadResult

| field | 型・構造 | 必須 |
|---|---|---|
| `evidence_id` | EvidenceID | Yes |
| `projection_version` | raw/v1 / text/v1 | Yes |
| `data_base64` | string | Yes |
| `total_bytes` | integer | Yes |
| `returned_range` | ByteRange | Yes |
| `partial` | boolean | Yes |
| `raw_hash` | string | Yes |
| `projection_hash` | string | Yes |
| `capture_complete` | boolean | Yes |

### ShutdownInput

| field | 型・構造 | 必須 |
|---|---|---|
| `mode` | drain / cancel | Yes |
| `deadline_seconds` | integer | Yes |

### ShutdownResult

| field | 型・構造 | 必須 |
|---|---|---|
| `accepted` | `true` | Yes |

## 6. 重要な意味条件

**StartInput**: active RunがあるThreadへのturn/startはBUSY。input/appendを使う。context_blocksのuser_messageを使ってinput本文を二重投入しない。渡すblockはCharacter/Stable/Recall/Variableの四種、user_messageはHostがinputから構成する。schemaは返却/内部共有のため五種を表現するが、入口のuser_messageはINVALID_REQUEST。

**ResumeInput**: 対象Task/Threadをcallerが利用可能、旧driverが解放、last_run一致、原本/checkpoint検証済み、未確定副作用が照会可能または明示blockedであること。binding=nullなら前回bindingを維持。新binding指定は明示権限・能力・budget検査を通す。completed Taskの自動rerunは受け付けない。

**CompactInput**: dry_run=trueはsource/budget/保護の検査だけで、LLM生成もcommitも行わない。falseはidle Threadでのみ受付。自動/overflowは内部driverの操作として同じEngine.Compactへ入る。Run生成を伴うmanual operationはsystem Task/Runを持ち、Business Task完了へは反映しない。

**CompactResult**: executed以外はoutcome=null。Normal/Emergencyだけcheckpoint_idあり。RestartRequiredでは検証済みの新checkpoint IDを返したことにせずnull。未確定の候補はreceiptのprivate記録へ保持する。Budget未確認ならstatus=unavailable/error.code=BUDGET_UNVERIFIED。stale/cancelledは5outcomeのいずれでもない。

**InputReceipt**: queue_revisionとcontrol_revisionは別。next_step/next_turnはqueueだけ進める。interrupt_currentはqueue保存とcontrol変更を同一transactionで行い、残りToolのdispatchを止める。新入力を失わずnext_turnへdeferし、旧Run停止後に次のturn/start相当へ一回だけ適用する。旧RunのLLM応答を新入力後の成果にしない。

**EvidenceRead**: requested rangeは0<=start<=end<=total、1回64KiB以下。text projectionの文字境界でない場合INVALID_RANGE。raw bytesはbase64で返す。partialは全文未返却またはcapture_complete=false。hashは全文raw/projectionに対する値で、返した断片のhashではない。別途断片hashを追加するならversion付きfieldを定義する。

**Session fork**: source checkpointを検証し、別Threadへsource refsを継承する。確定前Toolの状態はコピーしない。source ACLより広く公開しない。file systemをその時点へ巻き戻す処理ではない。新Threadの初期control/context revisionを返し、新仕事は次のturn/startで始める。

## 7. Eventsとstream

確定EventはDB commit後に通知`event/recorded`（params=Event）。event_seqはThread内の増分でありEventIDとは別。再接続はevents/read(after_seq)で再取得する。同じEventIDの重複配信は許容し、clientは重複除去するが、実行を重複させない。

text deltaは別notification `progress/delta`。paramsはProgressDelta（run_id、attempt_id、ordinal、text、provisional）。ordinalはAttempt内の順序。永続Eventではなくevent_seqを持たない。途中出力をcanonical最終Messageにしない。欠落時は`progress/gap`のProgressGap（run_id、attempt_id、from_ordinal、to_ordinal）を出し、最終Messageを取り直す。確定イベントは黙って捨てない。

bounded outbound queueは制御応答と確定Eventを優先する。追いつけないclientでは進捗を停止しgapを明示、なお制御応答も送れなければconnectionを閉じcancelする。保存済みEventは残す。遅いUIを理由に取消受付を永久に詰まらせない。

確定type（既存13＋retry 2。payloadはEVENT_CONTRACTに固定）: session.created, input.accepted, input.applied, task.created, run.started, model.requested, model.completed, action.prepared, action.dispatch_started, action.completed, checkpoint.committed, control.cancel_requested, run.terminal。各Eventからrun/receipt/EvidenceをAPIで辿れる。raw textはEventの公開metadataに入れない。

## 8. Error

JSON-RPC標準errorは-32700 parse、-32600 invalid request、-32601 unknown method、-32602 invalid params、-32603内部不整合を使う。domain errorは-32000のdataにErrorInfoを入れる。BUSY、FORBIDDEN、REVISION_CONFLICT、IDEMPOTENCY_CONFLICT、UNSUPPORTED_CONTRACT、BUDGET_UNVERIFIED、INVALID_RANGE、INTEGRITY_BLOCKED、PERSISTENCE_UNCERTAINを区別する。

受理後に起きたModel/Tool失敗はRunResultやCompactResultで返す。実行済み副作用があり得る失敗を単純な「API未実行」に丸めない。error.messageは短い診断で、本文/鍵/credential/path全体を出さない。


## 9. 付属型の追加意味条件

RunInfo.phaseの正式値はRetryWaitingを含むIMPL§18の16phaseに限定する。terminal=trueではphase=TerminalとRunResultを必須、falseではresult=null。Verification.passedはEvidenceを1件以上とcriteria_revisionを要する。設定されたhost verifierのcriteria_revisionはHarnessが実際に凍結・実行したplanのdigestであり、Task admission時にCOREがprofileへ固定した期待値と一致しない限り受理しない。Verifier Actionは既存`kind=verification`のAction/Attempt/Event contractを使うため、RPC method、Action event field、SDK型を追加しない。nonzero/timeout/cancel/incomplete captureはpassedではなく、dispatch後の結果が不明ならunknown、未実行ならnot_runとなる。OriginProofのMAC/nonce/date encodingはCORE_INTEGRATION§2を用い、独自のJSON再serializeで署名し直さない。

CompactResultのNormal/Emergencyではbefore/afterにverified BudgetReport、semantic/durable境界、checkpoint_idが必須。CapacityBlockedでverified最低値が分かる場合はrequired_minimum_tokensとavailable_tokensを返す。source破損のため計数できない場合はnullを維持する。dry_run/unauthorizedを実行した結果へ偽装しない。

SessionInfoはThread単位の公開snapshotを含む。session/listでは同一Sessionに複数Threadがある場合に複数要素があり得る。cursorはopaqueで同じcaller/filterにのみ有効。現在見えていないThreadがないと断定する一覧ではない。


[O01]: SOURCES.md#o01
[O03]: SOURCES.md#o03

## 付属: v0.2.2追加の公開型

### RecoveryPolicy

| field | 型・構造 | 必須 |
|---|---|---|
| `contract_version` | `act-recovery/v1` | Yes |
| `max_attempts_per_act` | `2` | Yes |
| `allowed_profiles` | array[same_request / terminal_output_once] | Yes |

### IntakeReceipt

| field | 型・構造 | 必須 |
|---|---|---|
| `receipt_id` | ReceiptID | Yes |
| `message_id` | MessageID | Yes |
| `thread_id` | ThreadID | Yes |
| `evidence_id` | EvidenceID | Yes |
| `principal` | string | Yes |
| `entrypoint` | cli_interactive / cli_exec / cli_pipe / stdio_core / stdio_automation | Yes |
| `caller_profile_digest` | string | Yes |
| `declared_origin` | human / automation / unknown | Yes |
| `effective_origin` | human / automation / unknown | Yes |
| `proof_basis` | declared_local / verified_relay / automation / unknown | Yes |
| `proof_digest` | string / null | Yes |
| `accepted_sequence` | integer | Yes |
| `accepted_at` | string | Yes |

### ModelAttemptReceipt

| field | 型・構造 | 必須 |
|---|---|---|
| `action_id` | ActionID | Yes |
| `attempt_id` | AttemptID | Yes |
| `ordinal` | integer | Yes |
| `request_id` | RequestID | Yes |
| `response_id` | ResponseID / null | Yes |
| `stage` | act / instruction_selection / work_summary | Yes |
| `base_request_digest` | string | Yes |
| `request_digest` | string | Yes |
| `binding_fingerprint` | string | Yes |
| `recovery_profile` | same_request / terminal_output_once | Yes |
| `recovery_profile_revision` | string | Yes |
| `applied_transformations` | format_suffix / thinking_disabled[] | Yes |
| `failure_code` | string / null | Yes |
| `generation_state` | not_started / terminal / unknown | Yes |
| `backend_attempts` | 0 / 1 / null | Yes |
| `usage_complete` | boolean | Yes |
| `request_evidence_id` | EvidenceID | Yes |
| `response_evidence_id` | EvidenceID / null | Yes |
| `input_digest` | string | Yes |


## v0.2.2で固定する意味

ContextBlock.digestは公開fieldではない。内部型でHostが計算する。revision構築はCORE_INTEGRATION§1、OriginProofは同§2のMAC順序を使う。Start/Resumeのlimitsは必須で、受付結果とRunInfoへeffective_limits/deadline_at/recovery_policy_revisionを返す。入力ごとにintakeを返す。旧draftでこれらを省いた要求はINVALID_PARAMSであり、live互換層を追加しない。

RetryWaitingはcancel可能な短い待機phase。model.retry_scheduled/model.attempt_startedのeventはEvidence経由でModelAttemptReceiptへ到達可能にする。ModelAttemptReceiptのbackend_attempts不明はnull。これは未実行の意味ではない。

本版はまだ配備されていないdraft契約の改訂である。rencrow-harness/v1を初回release前に本版で固定する。旧draft schemaの試験DBやclientとの互換を製品runtimeに残さない。実際の既存DBを扱う場合はmigration/backup規約に従い、勝手に削除しない。

retry開始時は`progress/reset`のProgressResetを送り、失敗Attemptのprovisional本文を破棄表示する。新Attemptの本文へ黙って連結しない。確定した最終応答の原文はrun/getとEvidenceから再取得できる。

### ProgressDelta

| field | 型・構造 | 必須 |
|---|---|---|
| `run_id` | RunID | Yes |
| `attempt_id` | AttemptID | Yes |
| `ordinal` | integer | Yes |
| `text` | string | Yes |
| `provisional` | `true` | Yes |

### ProgressReset

| field | 型・構造 | 必須 |
|---|---|---|
| `run_id` | RunID | Yes |
| `old_attempt_id` | AttemptID | Yes |
| `new_attempt_id` | AttemptID | Yes |
| `reason` | string | Yes |
| `provisional` | `true` | Yes |

### ProgressGap

| field | 型・構造 | 必須 |
|---|---|---|
| `run_id` | RunID | Yes |
| `attempt_id` | AttemptID | Yes |
| `from_ordinal` | integer | Yes |
| `to_ordinal` | integer | Yes |

## 実装前確定（v0.2.2）

[BYTE_CONTRACTS](BYTE_CONTRACTS.md)、[EVENT_CONTRACT](EVENT_CONTRACT.md)、[STREAM_CONTRACT](STREAM_CONTRACT.md)を本APIの規範付属契約とする。actはstream=true、Selection/Summaryはfalse。公開Event.payloadはtype別closed schemaに従い、同じJSONをevents.payload_jsonへ保存する。
