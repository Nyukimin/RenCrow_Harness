# 確定Eventのpayload契約

RH-EVENTS-001 / v0.2.2 / F23・F38 / 設計: ルミナ
Eventの共通fieldはPROTOCOL。**payloadを追加し、events.payload_jsonにはCJ1(Event.payload)を保存する。** Event自体をpayloadへ再帰格納しない。

## 1. 全15 type

既存列挙13件とv0.2.1で追加したretry 2件を含む。全field required、表のnullは明示null。詳細型と未知key拒否はschemaのEvent.allOfを正本とする。

| type | payloadの全field |
|---|---|
| session.created | session_id, binding, mode |
| input.accepted | intake:IntakeReceipt, queue_item_id|null, disposition:initial/next_step/interrupt_current/next_turn, queue_revision, control_revision |
| input.applied | message_id, queue_item_id|null, context_revision, disposition |
| task.created | task_id, turn_id|null, parent_task_id|null, kind:work/compaction/session_maintenance |
| run.started | run_id, previous_run_id|null, trace_id, writer_epoch, effective_limits, deadline_at, recovery_policy_revision |
| model.requested | action_id, attempt_id, request_id, stage, ordinal, input_digest, request_digest, binding_fingerprint, stream |
| model.completed | action_id, attempt_id, outcome:completed/incomplete/refused/error, generation_state, failure_code|null, attempt_receipt_evidence_id |
| action.prepared | action_id, attempt_id, kind:model/tool/measurement/verification, name, args_hash, policy_revision |
| action.dispatch_started | action_id, attempt_id, writer_epoch, control_revision |
| action.completed | action_id, attempt_id, effect_state, exit_code|null, result_evidence_ids[], capture_complete |
| checkpoint.committed | checkpoint_id, mode, candidate_hash, context_revision（採用後）, semantic_boundary, durable_boundary, before_count_evidence_id, after_count_evidence_id |
| control.cancel_requested | run_id, control_revision, reason（固定code）, principal |
| run.terminal | status（7終端）, code, result_evidence_id, last_checkpoint_id|null |
| model.retry_scheduled | action_id, failed_attempt_id, next_ordinal:1, trigger_code, delay_ms, recovery_profile, recovery_profile_revision |
| model.attempt_started | action_id, attempt_id, ordinal, request_id, generation_attempts_used, deadline_at |

`model.completed`/`action.completed`の語は一試行の終端記録であり、成功はoutcome/effect_stateで判断する。試行不明も明示記録できるが成功にしない。result_evidence_idから完全RunResult/ModelAttemptReceiptを取れ、公開payloadへ最終本文を重複しない。

## 2. 対応条件

payloadと共通fieldに同じIDがある場合は一致必須。typeに必要な共通task_id/run_id/receipt_id/evidence_idを埋める。session.createdはtask/run=null。input.accepted/input.appliedは適用先Task/Runが未決定ならnull、task.createdはrun作成前ならrun_id=null。その他の実行eventは所属task_id/run_idを必須にする。input.acceptedのqueue_item_idは初期受付ならnull、追加入力なら非null。receiptのaliasや新しいResultIDは作らない。

Eventが辿るEvidenceは同transaction以前にsealedであること。run.terminalはRunのresultと同じtransactionで一回だけ書く。checkpoint.committedはpointer更新と同じtransaction。cancel_requestedの記録は停止完了の証拠ではない。

run.started等の時刻は共通recorded_atへUTC秒精度で保存し、duration測定は別のmonotonic clock値。IDとevent_seqから因果を推測せず、DBのcausation_event_id/dependency_event_ids_jsonには実依存だけを保存する。公開Eventに因果全graphを増やさず、必要なら私有Evidenceで参照する。

payloadの未知type/keyは拒否する。新typeはschemaと本表の更新を要する。progress/delta/reset/gapは永続Eventではなく既存の別通知。raw本文や鍵をEvent公開payloadへ入れない。

## 3. idempotency

確定Eventは保存後だけ通知。同じEventIDの再配信は許す。recipientはevent_idでdedupeし、operationを再実行しない。events/readはpayloadを同じJSON値として再返却する（wireの空白は問わないが保存CJ1 hashは維持）。scope外のEventをhas_more/cursorから推測できるようにしない。
