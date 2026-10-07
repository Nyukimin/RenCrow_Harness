# 保存・復旧・所有権契約

Document RH-STORE-001 / v0.2.2 / 初期DDL: [sql/001_initial.sql](../migrations/001_initial.sql)

## 1. 採用する構成

独立Harnessが自身のSession、Task、実行、原本、checkpointを所有するため、新しい論理store`harness.execution`を登録する。物理実装は一つのローカルSQLite DB。COREのstoreへ別processからwriteしない。新規moduleの保存責任/復旧単位が異なることをStorage Proposalの理由とし、CORE正本/Manifestの規約へ追加する。

Harness configured data_root配下の`execution.sqlite3`を正本とし、`locks/`をOS lock、`staging/`を未採用のbounded一時出力に使う。stagingはcanonical sourceではない。添付/Tool結果は初回はSQLiteのchunked BLOBへ保存し、参照の原子的確定を一つのstore内に保つ。

例: productionの論理subtreeは`/srv/rencrow/db/harness`。Windows/macOSは設定済みabsolute path。standalone初期化も明示data_rootを必須とする。一度決めたrootが使用不能ならfail closed。repository/home/tempへの自動fallbackやCORE DB再利用はしない。

## 2. 論理record

| table | 正本の内容 | 不変条件 |
|---|---|---|
| sessions / threads | workspace、binding、policy、context/control/queue revision、writer_epoch | 同じThreadのwriterは一つ |
| turns / tasks | 受理入力、Root/child仕事、ParentTask参照、upstream相関 | 外部Task IDを同義の新IDとして再発行しない |
| runs | Taskの一回の実行、phase、status、resume_source、結果 | 再開ごとに新Run、古いrecord改名禁止 |
| actions / attempts | 意図、検証済み引数、物理試行、dispatch-start/結果 | 同Action retryは新Attempt。unknownをnot_startedへしない |
| tool_links | Model response/tool ordinalとActionの対応 | key一意、args_hash変更はconflict |
| evidence / evidence_chunks | privateなraw bytes、capture状態、digest | sealed後不変 |
| items | canonicalな入力/Model/Tool履歴 | append-only、出所とEvidence参照 |
| checkpoints | 検証済みcandidate bytes、digest、semantic/durable境界 | immutable、current pointerはtransactionでのみ切替 |
| receipts | 冪等mutationと結果 | caller+key一意、異payload conflict |
| queue_inputs | 受付と適用の区別、disposition | 受付済み入力をcompactionで失わない |
| events | 発生済み事実 | append-only、Thread内event_seq一意 |
| model_calls | logical/physical要求、usageの既知/未知、LLM receipt | secret/原本文字列はprivate Evidenceへ |

外部COREのSession/Thread/Turn/Taskは参照だけであり、foreign keyをCORE DBへ跨がせない。upstream JSONは厳密DTOでvalidateし、authの代用にしない。

## 3. SQLite設定とwriter

foreign_keys=ON、journal_mode=WAL、synchronous=FULL。local filesystemを前提にし、network share上でこの保証を広告しない。schema migrationは新規driver受付停止とbackup後、唯一のmigration processが実行する。DBのpermissionはowner-onlyを基準に、Windowsは同等ACLを設定する。

ThreadごとのOS lockを取ったprocessだけがwriter_epochを増やしてdriverになる。lease heartbeatは診断用で、期限切れだけでOS lockを奪わない。各短いwrite transactionはexpected epoch/revisionsをWHEREで比較し、affected row=1を確認する。

SQLiteはwriterを直列化するが、外部Toolや別processのfilesystem副作用までtransactionに含めない。workspaceのexclusive OS lockを別に取得する。

## 4. Rawの取り込み

1. raw Evidenceをbuildingで作り、bounded chunksを順序付きで保存する。total capture上限はRun limit以内。
2. stream終了/上限/取消に応じてcapture_completeを確定し、保存済みbytesのdigest/sizeを計算する。
3. sealedへの一回のtransitionとterminal Tool result/item/eventを同じtransactionで確定する。sealed後のpayload/chunksをUPDATE/DELETEしない。
4. processが落ちたbuilding Evidenceはincomplete capture。既存chunksは保持し、結果照会なしに完了させない。owner recoveryがpartialとしてsealできるが、full execution proofにはしない。

canonical rawがbinaryであることとtext projectionは別。raw hashは原payloadのbytes、projection hashはversion付き投影bytes。初回text/v1は既知text partをsource順にLFで結合し、その区切りもprojection定義に含める。unknown part/mixed mediaは投影して全文扱いせずprotected。

## 5. Compaction commit transaction

F14は以下を一つのBEGIN IMMEDIATE transactionで行う。

1. caller scope、writer_epoch、context/control/binding/policy revisionを検査。
2. 検証済みcandidate bytesを受け取り、そのbytesのSHA-256、SourceRef、原本取得可、保持/coverage/semantic/durable境界を再照合。
3. immutable checkpoints行を追加。
4. same caller/keyのreceiptとcheckpoint.committed Eventを追加/確定。
5. threads.current_checkpoint_idとcontext_revisionをCASで更新。
6. COMMITの成功を確認してからlive pointerとクライアントへpublish。

通常Model/Tool historyの追加にも同じrevision disciplineを使う。count対象とcommit対象が別snapshotにならないようにする。受付queueだけの追加はcontext_revisionを進めず、control changeとは別に扱う。

serializeは一回。DBに保存したcandidate BLOBのdigestを確認し、読み戻しで別serializerのJSONと比較しない。意味fieldの検証はparseした値で、bytes不変は保存BLOBで行う。raw JSONのkey順/数値表現を変更した場合は別projection version。

## 6. 停止とdispatchの線形化

cancel transactionはcontrol_revisionを増やし、cancel要求eventとreceiptを確定。dispatch-start transactionも同じThread rowのepoch/controlを比較し、Attemptのdispatch stateとeventを確定する。

cancelが先ならdispatchは不可。dispatch-startが先なら副作用が既に起き得るものとして記録する。外部呼出しが実際にはまだ開始していなくても、確実なnot_started証拠がなければ再実行しない。ロックを持たない「直前に読んだcontrol」だけで許可しない。

checkpointも同じcontrol revisionをCASするので、cancel後の古いcandidateは採用されない。通常のqueue追加は別revisionなので古い履歴snapshotと矛盾しなければcommit可能。

## 7. Cold resumeとreceipt不確実性

Resumeは、旧driver解放→raw/checkpoint/receipt整合確認→旧未確定Attemptの照会→新Trace/Run作成→次の合法step、の順。checkpointがない初回Runでもcanonical itemsが健全ならそこから再構成する。checkpointが存在して壊れている場合は「ない」と読み替えない。

旧ProcessのPIDだけで照会/killしない。host incarnationと開始tokenを照合できない場合はunknown。自動的に同commandを再実行して結果を取り直さない。

commit応答が消失した場合、receipt/idempotency keyで同結果を取得する。commit成立不明のまま別checkpointを重ねない。DBが再読取不能ならrestart_required。読取後も決定的矛盾があればIntegrityBlocked。

## 8. Backup、restore、保持

SQLiteの整合したbackup APIまたは停止状態の正規backupを使い、稼働DBファイルだけをcopyしない。raw/chunks、checkpoints、receipts、events、pending inputs、Task/Actionの参照が同じ復旧点に含まれることを検査する。lock file内容を復旧後の所有証明にしない。

初期retentionは明示archive/export後のowner lifecycle操作だけ。Modelからraw削除を要求できない。容量quotaに達したら新たな出力/要求を停止し、不完全capture/未保存を明示する。古いHumanやEvidenceを無断削除して続行しない。disk-full時に追加のdurable診断保存まで必ずできるとは保証しない。

復旧時は参照closure、sealed digest、各pointer、queueの未適用、unknown副作用を確認する。backup後のfilesystem/外部sendはDBを戻しても巻き戻らない。復旧証拠を確認せず旧Taskを再実行しない。

## 9. DDLの位置付け

付属SQLは初期schemaの設計と構文確認用。JSON payloadの詳細はPROTOCOL/IMPLの対応型を使う。DDLだけでauthority/UTF-8/hash/semantic correctnessが実装されるとは扱わない。immutable trigger、foreign key、unique index、transaction手順、domain validatorを併用する。

本パッケージではSQLiteへのschema作成と制約の合成試験を行えるが、Go driverのdurability、三OS filesystem、kill/replay、実Model/Toolの受入は製品実装後に別途行う。


## 10. Raw受付順とContext適用順

`items.sequence`はimmutable rawの受付順。`context_entries.context_seq`はModel用履歴へ適用した順序で、別の数値である。queue受付時はitemsとqueue_inputsだけを保存する。適用時にcontext_entriesとinput.applied eventを追加しcontext_revisionを進め、queue_inputsに適用revisionを記録する。

Compactionのsnapshot/digestとsemantic/durable boundaryはcontext_entriesの適用順を使う。pending rawが後から保存されただけではsnapshotは変わらない。applyされた追記は、raw受付時刻が要約中だった場合も、新しいContextの後段へ一回だけ入る。SourceRefの受付順とContext適用順を取り違えない。

Selectionの後続訂正判定は認証済み入力の受付順序で行い、Summaryへ提示する履歴は適用順で構築する。明示的なcontrol変更は別のcontrol_revisionで即時にcommit/dispatchを遮断する。

## 11. 製品実装で追加検査すること

DDLのthreads.current_checkpoint_id/active_run_id、tasks.last_run_id、turns.root_task_idは作成順の循環を避けた参照fieldで、Service transactionで存在・所属を検証する。文字列があるだけで有効pointerとしない。CHECK/triggerだけで全domain契約が強制されるとは主張しない。

採用bytesのimmutable性と、pointer/current-stateの変更可能性を分ける。sealed rawを変更するlifecycleは通常runtimeから到達不能にし、初回は削除操作を公開しない。

## 12. 試行・再開・由来の保存

runsはlimits_json、deadline_at、recovery_policy_json/revision、generation_attempts_used/unknownを持つ。新Run作成時に全値を保存し、既存Runのlimitを省略値で上書きしない。Taskの履歴を合算すると旧Run消費に到達できる。

model_callsは一Attempt一要求のunique制約と、stage、ordinal、base/current digest、profile/revision、適用変換、generation_state、backend_attemptsを保存する。act以外のordinal=1を拒否する。backend_attempts=0はnot_started、1はterminal、nullはunknown。後からreceiptを取得してunknownを解決するときは解決eventを保存し、当初何が不明だったかを隠さない。

retryを予約するtransactionはRunの残予算、control/writer、前Attemptの終端、同Action最大2を検査する。二つのdriver/リトライ要求から同じordinalを作らない。budget消費を記録してから生成を送る。Action.args_bytesは論理要求のsnapshot、各実要求bytesはModelInvocationのEvidenceに置く。

IntakeReceiptはitems.metadata_jsonとreceiptsの結果へ保存し、input.received eventと同transactionで確定する。CORE中継はrelay_noncesの(issuer,key_id,nonce)を一意に保存する。正しい同keyのreceipt再取得はnonce再検査より前。同nonceの異operationは拒否。

本版SQLは未配備draftの初期DDL更新であり、既存製品DBに直接流すmigrationではない。v0.2の隔離試験DBがある場合も、バックアップを取り、使い捨てと明示できるものだけを明示再初期化する。実データにはownerのmigration/restore手順が必要。

## v0.2.2のbyte形式

checkpoint candidate_bytes/hashはBYTE_CONTRACTS§6とCHECKPOINT_FORMATに従う。既存の概念列をDBに持つだけではserializerが決まったとは扱わない。events.payload_jsonはEvent.payloadのCJ1。Expanded ModelAttemptReceipt（input_digestを含む）は既存model_calls.receipt_jsonとEvidenceへ保存でき、同じ正本を別DBに追加しない。

context_entries.context_seqがProjectionEntry.sequence、semantic/durable boundaryの座標系。SourceRef.sequenceは原本側の受付順序であり別。tool batchのProjectionEntry.sequenceはbatchの最後の適用context_seq、boundaryがbatchを分断しないことを検証する。

