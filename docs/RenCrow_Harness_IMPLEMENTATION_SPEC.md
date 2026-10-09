# RenCrow_Harness 実装仕様書

Document: RH-IMPL-001 / Version: 0.2.2 / 2026-10-07 JST  
Status: **ルミナ確定の実装指示。Claudeが実装・検証する。記載API・package・SQLは新設対象**

上位は[仕様書](RenCrow_Harness_SPEC.md)。公開fieldは[PROTOCOL](PROTOCOL.md)と`schemas/protocol.schema.json`、LLM境界は[LLM_INTEGRATION](LLM_INTEGRATION.md)、保存は[STORAGE](STORAGE.md)と`sql/001_initial.sql`で定義する。これらは本実装仕様の付属契約である。

## 0. 実装担当への決定事項

ルミナが設計責任を持つ。Claudeは本書とWORK_ORDERに従って、Harnessだけでなく許可されたCORE・LLM・profileの接続変更も行う。公開契約や初回範囲を再提案することから始めない。反証があれば設計決定IDとsource/実測を添えて該当箇所だけ照会する。

`PROTOCOL`とschemaは公開field、`RETRY_CONTRACT`は試行、`CORE_INTEGRATION`はrevision/由来、`LLM_INTEGRATION`はモデル境界、`STORAGE`とSQLは保存を所有する。本文・schema・実装が衝突した場合、都合のよい側だけ採らず差分を修正してから通す。

## 1. 実装の配置と依存

```text
RenCrow_Harness/
  cmd/rencrow-harness/           # chat / exec / serve / inspect / resume等
  pkg/client/                   # CORE等が使うstdio client。engineを実行しない
  pkg/protocol/                 # JSON DTO、schema、protocol version
  internal/service/             # 全入口共通。受付・ACL・operation・返却
  internal/identity/            # Canonical名/prefix/UUIDv7の形式と発行窓口
  internal/kernel/              # pure transition + effect driver
  internal/session/             # Thread、Turn、queue、writer
  internal/contextplan/         # typed assembly / retention / budget
  internal/compaction/          # prepare / select / summary / emergency / validate
  internal/modelclient/          # RenCrow_LLMのみ。Chat strictとmeasure
  internal/toolruntime/          # registry、policy、Action/Attempt、dispatch
  internal/tools/files/          # read/search/create/edit
  internal/tools/process/        # argv実行、capture、stop/reconcile
  internal/evidence/             # raw chunk、投影、range、coverage
  internal/state/sqlite/         # transactions、immutable raw、receipt、checkpoint
  internal/recovery/             # replay / outcome classification
  internal/policy/               # data-driven判定。Modelへ決定を委ねない
  internal/extensions/           # trusted context、skill、固定hooks
  internal/transport/stdio/      # native JSON-RPC 2.0 + NDJSON framing
  internal/cli/                  # 同じServiceを呼ぶUI
  prompts/ schemas/ migrations/  # version付き契約資産
  testdata/                     # 合成fixture。private raw転載なし
```

COREに新設するのは`internal/adapter/nativeharnessclient`とcomposition wiring。既存`ToolHarness`/`super_agent_harness`と別名にする。CORE internal packageをHarnessからimportしない。LLM repositoryのinternal codeもimportしない。`pkg/client`はプロトコルだけに依存し、COREへruntime/store driverを持ち込まない。

初期依存は既存COREと同じGo 1.25.0基準、UUID、JSON Schema、SQLiteの既存採用libraryを候補とする。具体的にCOREの`github.com/google/uuid v1.6.0`、`github.com/santhosh-tekuri/jsonschema/v6 v6.0.3`、`modernc.org/sqlite v1.53.0`を基準候補としてpinし、三OS/Go buildを確認する。これは最新versionの推奨ではない。[C11]

## 2. 発行主体とID

### 2.1 形式と実行責任を分ける

HarnessはCanonical IDをそのまま使う。Task `tsk_`、Run `run_`、Action `act_`、Attempt `att_`、Evidence `evd_`、Checkpoint `ckp_`、Receipt `rcp_`、Request `req_`、Response `rsp_`等。新規はUUIDv7、migrationだけUUIDv5。entropy失敗で別形式へfallbackしない。[C09]

既存のUUID生成primitiveを使い、prefix/parse/goldenはIdentity Canonicalへ一致させる。COREへ問い合わせるID serviceは作らない。これは新しいID体系ではなく、Harness domainのissuer追加である。採用時にIdentity Canonicalのowner表を更新する。

| entity | 新設/更新できる主体 | 他方が保持する値 |
|---|---|---|
| CORE親Task/Run/Action | CORE | upstream reference |
| Harness Session/Thread/Turn/Root or child Task | Harness intake | COREは委譲receiptの参照/projection |
| Harness Run/Action/Attempt | Harness runtime | 受信したIDをそのまま引用 |
| Harness checkpoint/Evidence/receipt/event | Harness state owner | APIで参照 |
| 1回のGateway通信Request/Response | 通信を実施/受信するHarness clientとLLM | wire IDとcanonical IDの境界を区別 |

COREの同名型をpackage alias表でRuntimeの旧IDへ対応付けない。protocol境界は文字列を厳密parseする。純粋なID helperの共通化は可能だが、CORE root moduleと循環依存させず、実行責任と混同しない。

### 2.2 CORE委譲

COREは自身の親Task/Runの中で委譲Action/Attemptを一つ記録し、そのAttemptのidempotency keyで`turn/start`を呼ぶ。Harnessは新Turn、子Task、Runをtransactionで作る。`ParentTaskID=upstream.task_id`とsource scopeを記録する。親子は別の仕事であり同義aliasではない。

CORE内の古いtoolloopへ同じ仕事を同時投入しない。取得したHarness内部Actionの写しをCOREの実行済みActionとして再発行しない。Event引用は`{owner, event_id}`で残し、CORE側の「委譲完了」という別事実に新EventIDを付ける。

### 2.3 再開

cold resume、checkpoint resume、lease再取得は同じTaskに**新Run、新Trace**を作る。元のTurn/Thread、原本、依存関係は維持する。旧Runへ新しいdriverを継ぎ足さない。旧Run内の未確定Actionは照会/解決記録だけを更新でき、新たな物理再試行はしない。再実行が必要かつ安全なら新Runの新Actionとして、旧事実へのDependencyEventIDを付ける。同一Run中のretryだけは同Actionに新Attemptを発行する。

`ResumeInput.limits`は必須の新Run予算。F35 ResolveResumeLimitsで、host上限、全正整数、有限deadline、`max_generation_attempts`を検査して保存する。caller上限を超えた値はclampで隠さずINVALID_LIMITS。新deadlineは再開受付確定時刻+deadline_seconds。残余時間が失効した旧Runのdeadlineを継承しない。Task累積実績と旧Runの未確定試行は維持する。

`binding=null`は保存された同じbindingを使用する。明示binding変更は既存許可profileだけ、停止状態で新Runへ適用し、初回から再measureする。過去Taskの成功/失敗の意味は書き換えない。ResumeResultとRunInfoに実際のlimits/deadline/recovery policy revisionを返す。

## 3. ServiceとKernel

ServiceはACL、入力のdurable受付、operation scheduling、writer取得、Kernel結果の公開を担当する。Kernelの純粋関数は`Transition(state, event) -> next_state, effects`で、時刻、乱数、DB、ネットワークを直接呼ばない。driverがeffectを実行し、結果をeventにする。

```text
Admit → Load → Assemble → Measure → Generate
                    ↑               ├─ Final → ValidateFinal → Persist → Terminal
                    │               └─ Tools → Validate/Permit → PersistIntent
                    │                                         ↓
                    └──── PersistObservation ← Execute ← DispatchStart

Measure → CompactPrepare → Selection? → Preflight → Summary → Validate → Commit
                                      └─ semantic failure → Emergency → Validate
                                      └─ minimum no-fit → CapacityBlocked
```

未検証入力はmutable DTO。constructorでslice/map/bytesをdeep-copyし、Kernelにimmutable valueとして渡す。`PreparedSnapshot`、`ValidatedCandidate`は外部から直接生成できない型。State ownerは型だけを信じず、commit時に再検証する。

同じThreadのdriverはOS advisory lockを保持し、`writer_epoch`をDB transactionで増やす。処理中はlockを保持し、長いModel生成中にDB transactionは保持しない。期限切れだけではlockを奪わない。OS lockが取れない場合BUSY。PID文字列だけを所有証明にしない。

変更を許すRunはworkspace単位の別exclusive lockも取る。複数Threadの読み取りは可能だが、同じworkspaceへの書込みRunは直列。外部editor等の非協調writerまでOS advisory lockで止められるとは主張しない。expected digestの再確認で観測した変更をconflictにし、完全なfilesystem CASを保証しない。

## 4. 公開型の実装

正式field一覧・制約・例はPROTOCOLを参照する。以下の名前に未定義の型を残してはいけない。

- StartInput → StartResult。新Turn/Task/Runのdurable受付。
- InputAppendInput → InputReceipt。`disposition`必須、queue/controlの扱いを返す。
- ResumeInput → ResumeResult。新RunIDと前回RunID、保存された再開根拠を返す。
- CompactInput → OperationAccepted、処理結果はCompactResult。
- RunResult。7終端status、verificationとEvidence、未確定Actionを分離。
- CapabilitiesResult / BudgetReport。能力名に加えstatus、basis、revisionを返す。

公開APIは非同期受付と実行完了を区別する。`turn/start`や`context/compact`の成功responseは受付の成功であり、作業成功ではない。Receipt取得で同じoperationへ戻れ、events/readは取りこぼした確定eventを再取得する。

## 5. Input受付とauthority

署名対象・期限・nonce・受付fieldはCORE_INTEGRATION§2〜3で固定する。全入口はF34 PersistIntakeを通り、raw Evidence、MessageID、受付metadata、operation receiptを一括確定してから応答する。CLI専用の省略経路を作らない。

対話CLIのprofile宣言は`declared_local`由来、scriptはAutomation、COREで生成した委譲指示はAutomation、本人原文の中継は有効なHMACを伴う`verified_relay`由来にする。Humanを名乗るproofが不正な場合はINVALID_ORIGIN_PROOFで拒否し、Automationに落として受理したことにしない。proofが最初からない許可済み自動入力はAutomationとして受け付ける。

同一OS userの悪意あるprocessを防ぐ完全な本人認証とは主張しない。initialize.client_name、role、本文の自己申告は認証に使わない。COREの本物のHuman原文を再生成・要約してから同じproofを付けることを禁止する。

入力はUTF-8の原文bytesで保存し、trim/Unicode正規化をしない。表示用の加工は別投影とする。JSON escapeと原本文字を区別する。

## 6. Prompt組立と既存COREの接続

公開`ContextBlock`はkind/text/revision/sourceの4fieldだけを持つ。内部`PreparedContextBlock`はこれにcomputed_text_digest、resolved_origin、intake_receipt_refを加える。digestをcallerから受け取って正解としない。F32 MaterializeContextRevisionをCORE/CLIの入力準備に追加し、規則はCORE_INTEGRATION§1に固定する。Character/Stable/Recall/Variable/Userを順序で再推定しない。CORE bridgeは5区分のまま送る。Shiroの`renderSystemMessages`で潰した値を新経路の入力に使用しない。Recall未投入の既存経路を正常なgoldenにしてはいけない。

初期の順序とTool継続の順序はSPEC§9に従う。canonical Messageの順序とpromptのprojection順序を別recordにする。日時/進捗等はVariable、Character/Stableはrevision変更まで固定。logical hash一致とBackend token prefix cache一致を混同しない。

内部history種別はHumanInstruction、AutomationInstruction、HostContext、Work、Observation、Protected。Unknown originは保護属性で残す。旧summaryはWork rawとは別にaccepted semantic checkpointとして参照する。進行中process、未完了Tool、opaque dataはProtected。

## 7. Model呼出し

初回は`/v1/chat/completions`＋LLM_INTEGRATIONのstrict extensionを使う。COREの既存Providerを経由しない。これは承認済みのCaller追加後の正規経路であり、Backend直結ではない。

Actはstream=true/STREAM_CONTRACTに従い認可済みtool schema、Selection/Summaryはtools=[]、tool_choice=none、json_object、non-stream。後二者には段専用promptのみを送る。Actもargsを完全受信・schema検証するまでToolを実行しない。

`length`、`incomplete`、premature EOF、終端二重、reasoning-only、空final、strict contract欠落は失敗。HTTP200や本文があることだけでは成功にしない。Tool用JSONの部分parseで得た値がvalidでも、生成自体がlength終了なら全Toolを未実行で拒否する。

strict v1ではGateway/Runtime/Backend adapterの無断追加生成を禁止する。一方、通常actはF31 PlanModelRetryにより同じ論理Actionの新Attemptとして最大1回の回復を行う。RETRY_CONTRACTにないretryはしない。既存Responsesの自動retryをそのまま有効にするのではなく、同じモデル固有補正をLLM側の明示recovery profileに移して使う。

failed outputはprivate Evidenceへ保存するが、会話ContextやTool dispatchに採用しない。新Attemptの生成条件が変わる場合は再measureし、profile/変更内容/digestを記録する。旧生成がterminalまたはnot_startedと確認できない場合はretryしない。取消・policy失効・予算枯渇はbackoff中も優先する。

## 8. 予算の実装

LLM_INTEGRATIONのmeasureが、通常生成と同じnormalization/template/Tool schema/出力予約を対象にcountを返す。`prompt_tokens`はoverhead込み。したがって下式でoverheadを二重に引かない。

```text
usable_prompt_tokens = effective_context_limit - reserved_output_tokens - safety_margin
fit = prompt_count.upper <= usable_prompt_tokens
```

推定値を用いた上位トリガは早めに発火してよいが、生成送信・Preflight・commit前の容量判定はverified countで行う。exact: lower=upper。bound: lower<=actual<=upperをtokenizer/template実装の根拠で保証。経験値はboundではない。

- `upper<=budget`: fit。
- `lower>budget`: no-fit。
- 区間が境界を跨ぐ: exact再計数。不能ならBudgetUnverifiedで終了し、実際のno-fitと断定しない。
- shrink: `after.upper < before.lower`なら確認可能。それ以外はexactで前後を計数する。単なるbytes縮小をtoken縮小とみなさない。

`budget_unverified`はmachine code `BUDGET_UNVERIFIED`、capability status unavailable。manual/auto/emergency共通。inspect/evidence/read/dry-runだけを許す。初回releaseには必須LLM計数実装が含まれるため、未検証の広告値だけを使う構成をrelease完了としない。

トリガは有効prompt budgetの既定85%を設計上の初期値とする。高精度計数結果と、追加のSummary材料/出力予約が収まるうちに実行する。85%は性能実測値ではなくconfig値。要約要求が大きくなる場合は合法な投影を再構成し、それでも入らなければCapacityBlocked。Hostが任意と宣言していないHuman/Protectedを削らない。

## 9. Tool、Action、Attempt

F16 DispatchToolは検証、Policy、control/fence確認、Action意図の冪等結合、Attempt開始のdurable記録、dispatchの順で行う。冪等keyは`RunID + accepted Model ResponseID + ProviderToolCallID + response内ordinal`。同keyでargs_hashが違えばconflict。

LLM生成も一つのActionであり、物理生成ごとにAttemptを持つ。通信ごとにRequestID/ResponseIDを持つ。Measureは生成ではないが、通信記録として別Requestを持つ。

Tool外部副作用とSQLiteは同一transactionではない。実行前記録、実行、結果確定の間にkillを注入する。未確定はReconcileし、結果を確認できなければEFFECT_OUTCOME_UNKNOWNで停止する。旧Run結果の読取を、同commandの再実行で代替しない。

### 9.1 初回の組込みTool

| name | 入力契約 | 結果/安全性 |
|---|---|---|
| file.read | workspace相対path、byte range、最大出力 | raw_hash、total_bytes、returned range、partial |
| file.search | path scope、literal/regex、件数/byte上限 | 一致場所・返した範囲。regexは線形時間実装、無制限再帰なし |
| file.create | path、UTF-8本文、expected_absent=true | 既存fileならconflict。書いたbytesとhash |
| file.edit | path、expected_hash、old_text、new_text | old_textは一意な完全一致。fuzzy/行番号補正なし |
| process.exec | executable、argv[]、cwd、env allowlist参照、timeout | exit code、stdout/stderr Evidence、capture_complete、Action/Attempt |
| evidence.read | EvidenceID、projection version、byte range | 原本/投影とhash、actual range。元Toolは実行しない |

実装時のTool schemaは上記契約から生成し、LLM_REQUEST toolsへ渡す。shellはprocess.execで`executable`が登録済みshell profileの場合だけ許可し、payloadの文字列を勝手に`sh -c`へ変換しない。

file.editは読取snapshot、workspace lock、preimage hashを照合し、同directoryの一時fileへ書いてflush後に原子的renameする。Unixでは置換fileの`0777` permission bitsを元fileから保持し、復元に失敗した場合はrenameせずI/O failureにする。`file.create`のmodeは通常どおりumaskの適用を受ける。setuid/setgid/sticky、owner/group、extended ACLは保持を保証しない。rename後のdurability失敗はunknown。外部非協調writerとの完全なCASは保証範囲外と明記し、変更検出時はconflict。symlink/junction、case folding、Windows reparse point、path traversalを三OSでテストし、文字列prefix検査だけでcontainmentを認定しない。

Process captureはbounded chunkでprivate storeへ保存。上限到達時はcapture_partialを記録し、process停止手順へ進む。missing bytesを「元からなかった」としない。全出力を保存できなかった結果からfull completion linkを作らない。

CancellationはLinux/macOS process group、Windows Job Object等で子process treeへ伝播する。OSごとの実装詳細をadapterに隔離し、Job Objectをfilesystem/network sandboxとは表記しない。PID再利用時に無関係processを停止しないためhost incarnation、開始時刻、生成nonceを照合する。

## 10. Compactionのsourceとhandle

### 10.1 Prepare

immutable snapshot、latest durable/semantic checkpoint、input origin、applied SourceRefs、進行中Action、保護対象、budgetを一回の論理loadで取得する。rawをindexで再利用し、stageごとの全rollout読直しを避ける。最新committed metadata不正はIntegrityBlocked。

`SourceRef={owner, source_id, raw_hash, projection_version, byte_start, byte_end, origin, sequence}`をHostが作る。rangeはUTF-8 byte半開区間。LLMが書いたhash/IDを期待値にしない。

### 10.2 Selection dataset

`presented_sources[]`はcanonical sequence→range.startで整列し、`presented-0`から連番handleを付ける。各要素にhandle、origin、exact text、保護/恒久制約のmetadataを付ける。`completion_links[]`はtarget順・terminal pair順で`completion-0`から付ける。未提示sourceは削除できない。

Modelは`target_handle`と`target_quote`、訂正根拠の`replacement_handle`と`replacement_quote`、または`completion_handle`を返す。quoteは原文に一意に完全一致する必要がある。Hostがbyte offsetを計算する。LLMに数値offsetやhashを転記させない。部分指示を独立に照合できなければ全指示を保持する。

revocationとamendmentは`basis`で明示する。後続訂正が「Aをやめて」だけでもrevocationとして成立し、Bを要求しない。source全体ではなく該当quoteだけを失効できる。訂正/撤回の範囲は保持する。

### 10.3 Completion linkの生成条件

一つの宣言済みHuman/Automation入力fragmentから明示的に結び付いた、同じ作業に対する一意なterminal process.exec pairだけを候補にする。call全文とoutput全文は**それぞれ**UTF-8で2,048 bytes以下、nonpartial/nonprotected/nonactive、source identity一意、rawがdurable、結果codeが判明していること。Automationの扱いは9/27の拡張に対応する。サイズの「各」は付属A F12で明示される。[D01][S02]

同じthreadで後からexecされたというだけでlinkを作らない。自然言語の全要件達成をexit_code=0から認定しない。Modelが意味を判定し、Hostがsource範囲・権限・証拠との矛盾を検査する。継続制約や未来の義務は消さない。V2 ref-only/excerpt markerは全文実行証拠でないためcompletion生成元にしない。Emergencyは新しいcompletion判断を一切行わない。

### 10.4 Summary datasetと5名前空間

| 配列 | handle | 提示するもの | 許可用途 |
|---|---|---|---|
| work[] | work-N | 前回semantic boundary後の未要約Work、canonical順 | current_work/decisions/open/next/verificationの根拠 |
| observations[] | observation-N | 許可された投影と今回提示range、partial | 根拠引用、important_observation_handles |
| instructions[] | instruction-N | Selection後の有効原文とorigin。要約対象外の参照data | 要求への参照だけ。原文を置換/削除しない |
| prior_summaries[] | summary-0 | latest accepted semantic Summaryを最大1個 | まだ有効な前回進捗/判断の継承 |
| completions[] | completion-N | 今回検証済みの完了結果と出典 | 完了/verificationの候補根拠 |

各配列は0から始まり欠番なし。handleは要求localであり永続IDではない。対応表をstage receiptのprivate payloadへ保存し、commit時はSourceRefへ解決する。同じhandle名が別stageに現れても同じsourceと仮定しない。

Protectedは別の`protected_state`に保持参照として提示し、勝手に要約可能な第6名前空間を追加しない。本文をModelへ見せていないopaque部分から事実を推論しない。

`verification.state=passed`にはModelが引用したhandleから辿れる明示実行証拠を要する。previous summaryにpassedと書いてあるだけでは足りず、保存されたEvidenceとの対応を確認する。未検証がpassedへ変わることを禁止する。

### 10.5 Preflight

verified token countによるNormal最小候補がno-fitならCapacityBlockedへ直行する。Emergencyや未定義の追加削減へ進まない。token境界不明はBudgetUnverifiedであり、数学的なcapacity floor超過とは区別する。Prepareでの重複除去・任意Recall除外はPreflightより前に完了する。

Summaryの生成要求が収まるかと、生成後の最小候補が収まるかは別々に確認する。空Summaryだけを「意味的に十分な最小」と見なさない。minimum_valid_summaryは構造の下限であり、実際の候補は全保持条件と実countを再検査する。

### 10.6 Summary、Emergency、candidate

通常Summaryは前回accepted summaryを一回、未処理Work、有効原文参照、許可Observation、確定結果から生成する。削除済み本文/negative検証用素材/全inventoryを自動投入しない。必須promptはprompts参照。

Summaryがsemantic/model failureなら、LLMなしEmergencyへ移る。旧semantic summary、未処理Work、active Human、Protectedを残す。古いdurable境界より前のraw/excerptを検証済みref-onlyへ縮約可能。fresh出力はbounded projection。semantic boundaryは進めない。

candidateはmode、snapshot/control/fence/binding/policy revision、retained refsとexact text、Summaryとsource対応、observation coverage、applied操作、semantic/durable境界、before/after count、canonical serialized bytes/hashを持つ。commit直前に全条件を再検査する。

## 11. CompactResultとRunResultの対応

| CompactResult.outcome（executedの場合だけ） | checkpoint | 継続中Runへの適用 |
|---|---|---|
| NormalCompacted | 新しいdurableとsemantic | 同Runを継続。Run completedにはしない |
| EmergencyCompacted | 新しいdurable、semantic維持 | 同Runを継続。Normal成功とは記録しない |
| CapacityBlocked | なし | blocked / CAPACITY_BLOCKED |
| IntegrityBlocked | なし | blocked / INTEGRITY_BLOCKED |
| RestartRequired | 成立未確定 | restart_required / PERSISTENCE_UNCERTAIN |

CompactResult.statusがcancelled/stale/unavailableならoutcome=null。cancelledはRun cancelled、staleは旧候補不採用後に新snapshotから1回だけ再評価可能、unavailableはblockedと不足code。stale retryは新transactionであり、1transaction最大2logical要求とは別にRun全体のretry予算にも計上する。

手動compactは操作receiptを持つsystem Task/Runで実施し、必要なら直近TaskをParentTaskとして参照する。新しいHuman発話を捏造して履歴へ追加しない。active driver中の手動compactはBUSYとし、待ちqueueへ黙って積まない。自動compactはそのdriverの安定点で行う。

## 12. 保存、cancel、replay

詳細はSTORAGE。writer_epoch/control更新とdispatch-start記録、checkpoint pointer変更は同じ短いDB transactionで線形化する。Model生成・process実行の間はDBロックを保持しない。

cancelが先に確定すればdispatch-startは失敗。dispatch-startが先なら開始済みとして扱い、後から停止/照会する。未実行かもしれないという推定でretryしない。取消後に遅着したModel応答は診断保管可能だが新Toolへ採用しない。

State transactionの結果がunknownなら同じreceipt/idempotency keyで照会し、別candidateを追加commitしない。rollback確実なら旧state、receiptありなら同結果、読取不能ならRestartRequired。再起動後の決定的hash不一致はIntegrityBlocked。

## 13. 回復と失敗分類

| 境界 | 分類 | 次の処理 |
|---|---|---|
| schema/no-tools/生成incomplete | ModelContract/Semantic | NormalならEmergency、actならRETRY_CONTRACTの有限retryまたはtyped終端 |
| raw/checkpoint/digest/authority確定矛盾 | IntegrityBlocked | 対象writerを停止、原本保存 |
| verified最低候補no-fit | CapacityBlocked | 終端、Emergencyなし |
| budget根拠/strict capability欠落 | Unavailable | 新Model/compact停止、読取は継続 |
| private rawで再生成可能なindex/context不良 | DerivedInvalid | 原本検証後、同一ownerで1回再生成 |
| metricsだけ欠落 | MetricsMissing | unknownで継続 |
| 保存成否不明 | PersistenceUncertain | ResolveReceipt→replay/restart |
| 外部Tool成否不明 | EffectOutcomeUnknown | 照会、不能ならblocked |
| cancellation | Cancelled | 新effect停止、実行中を照会 |

同じcontext/binding/budget/failure条件の無限compactを禁止する。未解決状態を消して条件hashだけ変えることも禁止する。

## 14. 実装工程

全作業の担当はClaude。moduleごとの変更場所、依存DAG、相対工数、具体的成果物、fixtureと実経路の完了条件はWORK_ORDERおよびwork_packages.jsonを実行計画の正本とする。

P0: WP00基準・実機source可用性を固定し、WP01でAD-01〜12を共有正本へ反映する。反証なしに要件を選び直さない。

P1: WP02 strict共通正常化、WP03計数のengine共用、WP04 Harness純粋kernel/保存を並行着手する。WP07のCORE snapshot/relay供給もこの時点から着手可能。LLM側変更を最後へ送らない。

P2: 独立binary、Service/stdio/CLI、Tool、F02/F16/F17/F29/F34/F35を接続。fake Modelでも本物の隔離file/processと保存を使って検証する。runtime成功とは分けて報告。

P3: WP02〜03の初回Qwen/MLX strict・measureを接続し、WP05のF15/F24/F31を実bindingで受入。生成前処理/計数の一致と有限retryの実証まで含む。

P4: WP06でNormal/Emergency、H01〜26、5結果、各2,048 bytes、Preflight、no-tools、連続圧縮、F25 session/forkを実装・検証する。forkの三OS最終受入はP6B。

P5: WP07〜08でCOREから五区分revision付きContext、署名Human relay、委譲Action、native client、結果projectionを接続。旧CodexWorkPath/Actionless loopを同Runで動かさない。

P6A: WP09としてmacOSの隔離構成でCLIとCORE両方を先行受入。計数・Compaction・retry・保存/restore・cold resumeを省かない。これは正式初回releaseではない。

P6B: WP10としてUbuntu/Windowsを含むnative実行・保存・process停止・CORE接続、session/fork、全必須試験とrelease provenanceを受入。**P6Bが正式な初回製品完成。**

P7: WP11は必要時だけ。旧Switchの対象source/exportを個別検証しoffline移行する。現在のEcoSystem pinを参照pinへ自動更新しない。移行対象がなければP6Bの妨げにしない。

## 15. 関数と試験

関数表、工程、試験の唯一の機械可読対応は`acceptance_matrix.json`。ACCEPTANCE_MATRIXの表はそこから生成する。各関数は入力、出力、前条件、副作用、失敗分類、postcondition、unit/contract/E2Eの試験を持つ。全関数に工程を割り当て、各必須試験から対応関数へ逆参照できるようにする。

全schemaは重複key、不正UTF-8、深さ/サイズ、trailing JSONを別parserで拒否してからJSON Schemaで検証する。JSON Schema単独でunique ID対応や意味証拠が検証できるとは扱わない。

## 16. 配備/rollback

機能追加の記述だけで本番serviceを再起動しない。実装者は許可された環境でbuild→fixture→実経路→配備を分けて報告する。配備ではbinary SHA、config revision、store schema、LLM contract/profile revisionを固定する。

対象CORE roleの新規admissionを止め、既存作業をdrain。new Harnessへ明示切替し、旧loopを同じ仕事のfallbackにしない。新checkpointを旧binaryが読めない場合binaryだけを戻さない。整合するbackupとmigration、backup後の副作用Evidenceを確認する。DB復元は既に行ったfile書込みや外部sendを取り消さない。

## 17. 完了報告に必要な証拠

文書、schema、DDLが検査されたこととruntime成功を区別する。各試験は入力hash、repository/source revision、OS/Backend/template/LLM contract、actual result、Run/Action/Receipt/Evidenceを記録する。全試験の初期状態はnot_run。実行しなかったものにpassedを付けない。

完了報告は「作ったファイル名」だけでなく、standaloneとCOREで何が成立し、どの故障を拒否し、未受入のbinding/OS/拡張が何かを示す。平均速度でP0/P1/P2違反を相殺しない。


## 18. 残すべき実装上の区別

- Contextの適用順はSTORAGE§10のcontext_entriesを用い、raw items.sequenceをそのままprompt順にしない。
- `MeasureResult.effective_context_limit`をnative BudgetReport.context_limitへ、binding_fingerprintをnormalization_fingerprintへ明示変換する。どちらも単なるModel名ではなく対象revision一式のfingerprintである。
- input_digest/request_digest/mutation_payload_hash/candidate_hashはBYTE_CONTRACTSの用途別規則を使う。rh-chat-payload-v1の曖昧な旧式は廃止。論理入力は両側計算、モデル固有変換後はRuntimeが計算しHarnessはechoする。
- bindingの実Model/権限/profileが変わった場合、単なる再measureで暗黙採用しない。現在Runを止め、明示された新bindingで新Runとしてresumeする。countキャッシュだけが失効しbinding契約が同一の再計数は一回可能。
- generationの通常actはappendされたToolCall/Resultの対が合法であることを確認する。Summary JSON stageをTool呼出しのように通常履歴へ追加せず、stage rawはModelInvocation Evidenceに置き、accepted Summaryはcheckpointのみに採用する。
- runtime phaseの正式名はAdmitting, Loading, Assembling, Measuring, Generating, PreparingAction, Executing, PersistingObservation, Compacting, CommittingCheckpoint, ValidatingFinal, PersistingResult, Cancelling, Recovering, RetryWaiting, Terminal。RunInfo.phaseはこのいずれか。Run.statusとは別。
- raw tool markupの検出は同じunderlying意図を構造化ToolCallへ無理に復元する処理ではない。no-toolsでは拒否、allowed-toolsでもModel formatが壊れたら明示errorを返し、Hostが推測実行しない。
- `verification.status=passed`には少なくとも一つの有効Evidenceとcriteria_revisionが必須。最低限のcounter/終端tagが検査できたことは、自然言語の全要求達成ではない。

初回configと起動validatorは[CONFIGURATION.md](CONFIGURATION.md)を使う。CLI profile名を具体的Bindingへ解決し、未知名・無許可workspace・非loopback Gatewayを初回標準で拒否する。


### Relay proofのbyte contract

署名fieldと連結順序はCORE_INTEGRATION§2を唯一の定義とする。OriginProof/schema/署名fixtureを同時に実装し、旧draftのrh-origin-v1で再署名しない。(issuer,key_id,nonce)は一回だけ新規受付可能。同key・同payloadの確定receipt再取得は期限/nonce検査より先に行う。

trusted CORE connection以外からupstream.owner=RenCrow_COREを主張しても、そのclaimだけで権限を与えない。署名原文とpayloadが異なれば受付を拒否する。

### Forkの生成record

ForkResult.checkpoint_idは新Thread用に検証して発行した新CheckpointID。source checkpointはrequestとreceiptに参照として残す。forkの保存操作にはHarness所有のsession_maintenance Task/Runを発行し、業務完了の意味を与えない。SourceRefは元のsourceを指し、rawや過去Actionを再発行しない。次のModel要求は新Threadのbinding/promptを改めてmeasureする。


[C09]: SOURCES.md#c09
[C11]: SOURCES.md#c11
[D01]: SOURCES.md#d01
[S01]: SOURCES.md#s01


内部値の必須fieldとTool schemaは[INTERNAL_CONTRACTS.md](INTERNAL_CONTRACTS.md)を参照する。

[S02]: SOURCES.md#s02

## 19. v0.2.2で閉じた実装境界

公開相互運用は[BYTE_CONTRACTS](BYTE_CONTRACTS.md)、[ERROR_MAPPING](ERROR_MAPPING.md)、[STREAM_CONTRACT](STREAM_CONTRACT.md)、Model投影は[MODEL_PROJECTION](MODEL_PROJECTION.md)、[STAGE_DATA](STAGE_DATA.md)、保存は[CHECKPOINT_FORMAT](CHECKPOINT_FORMAT.md)、[EVENT_CONTRACT](EVENT_CONTRACT.md)、Host資産は[HOST_ASSETS](HOST_ASSETS.md)を規範付属契約とする。

[最終設計回答](IMPLEMENTATION_DECISIONS.md)Q01〜17は本実装仕様の決定済み条件。WP08のShiro対象はshiro_native_coding_v1の新規受付、Executeの旧3分岐より前で委譲する。stream=trueと毎Attemptのmeasure→generateを初回既定とする。

F37 EncodeCanonicalContract、F38 BuildTypedEvent、F39 LoadHostAssets、F40 AssembleStrictStreamを追加する。各入出力・条件・WP・試験は台帳に記載。codeはClaudeが実装し、本パッケージのPython参照を製品runtimeへimportしない。

