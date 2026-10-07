# RenCrow_Harness 仕様書

Document: RH-SPEC-001 / Version: 0.2.2 / 2026-10-07 JST  
Status: **設計責任者ルミナの実装指示版。既存正本への反映・製品実装・配備は未実施**  
Reader: RenCrow repositoryを参照できるが、その内容を知らない実装担当  
Entry: [START_HERE](START_HERE.md) / [実装仕様](RenCrow_Harness_IMPLEMENTATION_SPEC.md)

## 0. 本版の決定権と読み方

れんの要求を実現するための仕様判断はルミナが所有し、Claudeが調査・実装・試験を行う。本書の「実装する」「禁止する」は実装指示であり、選択肢の提示ではない。既存sourceが反証した場合は事実を示して影響境界だけ停止する。勝手な方式変更や、初回範囲を削って完了とすることは認めない。[設計決定](ARCHITECTURE_DECISIONS.md)と[作業指示](WORK_ORDER.md)を参照する。

本文の現状記述と、今回の新しい設計契約を区別する。論文・Codex・現行sourceにない追加条件は、本書で決めたRenCrow_Harness固有の契約である。既存repositoryが既にこの形になっているとは扱わない。

## 1. 製品の目的

RenCrow_Harnessは、自然言語の作業依頼をLLMとToolを使って実行し、履歴・根拠・未完了状態を保持しながら作業を継続する独立した実行ツールである。COREと人が対等な呼出し元である。「独立」はCOREのprocess・DB・ID発行serviceがなくても利用できるという意味で、RenCrow_LLM等の既存moduleまで再実装する意味ではない。

初回製品の成立条件は、(a) RenがCORE停止中にCLIで利用できる、(b) COREがnative APIで同じHarnessを利用できる、(c) 両方が同じ保存・圧縮・取消・再開・Tool契約を使う、の三つである。

Codingを初回の主要用途とするが、Runの核をコード編集専用にしない。Tool契約で表せる調査・ファイル処理・検証にも利用できる。外部send等の新しい副作用は、対応するpolicyと再照会契約の追加なしに許可しない。

## 2. 現行RenCrowとの関係

現行COREはAgent、Persona、意味ルーティング、Execution Role、Memory、Toolの許可/実行を所有する。LLM GatewayはaliasとRuntimeへの割当、RuntimeはBackend/Modelと共通応答への正規化を所有する。Switch CoreはCodex forkで、独自Compaction V2を持つ。これらの詳細と基準commitはREPOSITORY_MAPに記した。[C01][C12][L01][D01]

本版は次の**所有権変更を指示する**。既存製品へ既に反映されているとは扱わない。

| 領域 | 新しい正本owner | COREが行うこと |
|---|---|---|
| Agent identity、Persona、長期Memory、上位目的、routing | CORE | 従来どおり |
| COREの親Task/Run/Action | CORE | 一つの委譲ActionからHarnessの子Taskを参照 |
| Harnessへ委譲した子Task、実行Run/Action/Attempt | Harness | 読取projectionと成果の採用。内部Tool実行を再発行しない |
| HarnessのSession/Thread、作業入力原本、Tool原本、checkpoint、再開 | Harness | APIで参照。DBへ直接writeしない |
| 段専用Selection/Summary prompt、Context再構成 | Harness | Persona/Recall等の入力を提供。段promptは変更しない |
| Model通信、template、tokenizer、thinking外装、normalization | RenCrow_LLM | 有効なaliasを指定。モデル固有処理を持たない |
| engine、weights、KV、GPU、物理token化 | Runtime / Backend / profile | 所有しない |
| 実際のTool実装 | Harnessの組込みexecutor、または契約済み外部Tool owner | 必要な外部Toolだけ公開。COREを必須executorにしない |

「同じTaskを二つのIDで表す」設計にはしない。COREのTaskは仕事の割当・結果採用を含む親の仕事、HarnessのTaskは委譲された一つの実行可能な子仕事であり、ParentTaskIDで関係を表す。CLIのTaskはHarnessが直接受けたRoot Task。RunIDを階層化しない。[C09]

## 3. 構成と言語

```text
Ren（chat / exec CLI）                     RenCrow_CORE
       │                                  native client
       │                                      │
       └─────────── Service API ────────────────┘
                         │
          rencrow-harness（native Go executable）
                         │
          Session + Execution kernel + Context/Compaction
                         │
              ┌──────────┼──────────────┐
              ▼          ▼              ▼
          LLM client  Tool runtime  Harness State/Evidence
              │                     SQLite + private raw
       RenCrow LLM Gateway
              ↓
       Runtime → Backend → Model
```

Go 1.25.0を初期互換基準とする。COREがこのversionとGo中心の配布契約を持つことが理由であり、性能優位を実測した判断ではない。[C01][C11]

エンジンはprocess内で再利用可能なGo packageとし、製品入口は独立binaryにする。初回のCORE連携は子processの`serve --stdio`を使用し、COREへengineを埋め込まない。これにより呼出し元のGo型や内部stateに依存しない。

Unix socket/Windows named pipeによる常駐multi-client server、HTTP公開、ACP、MCP-server化は拡張境界を用意するが初回必須ではない。初回の「Toolとして使う」は対話CLI、非対話JSONL、native RPCで成立させる。COREとCLIの同じlive Sessionへの同時操作は初回保証せず、明示handoffで排他を移す。

## 4. 初回に実装する機能

| 区分 | 初回必須 |
|---|---|
| 人向け入口 | `chat`の行入力/stream表示、`exec`、Session一覧/読取、停止、resume、manual compact |
| プログラム入口 | native JSON-RPC、型付きイベント、非対話JSONL、Go client。stdoutは機械可読出力専用 |
| 実行 | 一つの作業driver、有限step/時間/出力予算、Tool意図の検証、順次実行、typed終端 |
| Tool | file read/search/create/exact edit、process argv実行と停止、Evidence範囲取得 |
| 継続性 | immutable intake、原本保存、checkpoint、cold resume、新Run発行、結果不明照会 |
| Compaction | Normal、決定的Emergency、5分類の結果、原本参照、手動/自動/overflowの共通核 |
| 既存LLM接続 | Chat経路のstrict contract、正規化/再送観測、実入力token計数とfingerprint照合 |
| 拡張 | trustedなAGENTS.md、SKILL.md metadataと明示load、固定callback hook境界 |
| 運用 | 三OS native build、権限とデータ分離、backup/restore検査、diagnostics |

初回に作らないもの: 独自LLM Gateway、人格DB、長期記憶昇格、独立した全体Task Scheduler、Tool APIの無差別コピー、vendor OAuth模倣、Codex互換TUI、任意pluginの自動download/実行、並列変更Tool、自己増殖するsubagent、cross-vendor mesh、原本の推測修復。

MCP client等は既存Tool ownerが提供する場合にadapterで利用できるが、未対応transportをenabledとして広告しない。外部機能がなくても、初回の組込みfile/process Toolでcoding作業を完遂可能にする。

## 5. 優先順位と不変条件

P0 意味/権限の正しさ、P1 作業継続、P2 復元可能性、P3 効率、P4 単純性の順とする。P3/P4のために上位条件を弱めない。[D01]

| ID | 必須契約 |
|---|---|
| RH-001 | 同一Thread/Taskのdriverは一つ。同じ変更workspaceも同時に二つのdriverで変更しない |
| RH-002 | ModelはActorや権限発行者ではない。callerの要求権限はhost policyの上限を超えない |
| RH-003 | originは受付経路と由来receiptで確定。本文、role、agent名でHumanへ昇格しない |
| RH-004 | 現在保持すべきHuman原文・順序・UTF-8範囲を維持し、summaryに置換しない |
| RH-005 | 原本と派生Contextを分離。Unknown、active Tool、mixed mediaは無条件削除しない |
| RH-006 | durableかつ取得可能な原本だけを参照化。取得に元Toolの再実行を使わない |
| RH-007 | 今回提示、過去提示、summaryへの関連付けを区別。partialを全文理解と見なさない |
| RH-008 | Model出力は候補。ToolIntent、実行receipt、意味的なTask達成は別 |
| RH-009 | commit/dispatch時にwriter fence、context/control/binding/policy revisionを再照合 |
| RH-010 | timeoutやreceipt欠落を未実行と見なさず、結果不明の副作用を自動再実行しない |
| RH-011 | completed/incomplete/rejected/blocked/cancelled/failed/restart_requiredを区別 |
| RH-012 | 別Model、effort、route、権限、元のCompactionへの暗黙fallbackをしない |
| RH-013 | Selection/Summaryは段専用promptとdata。実行役instructions/toolsを混ぜない |
| RH-014 | 計測欠落はunknown。合成0や成功へ補完しない |
| RH-015 | 確定raw/checkpoint不整合を旧checkpointへのfallbackで隠さない |
| RH-016 | raw本文/secretを公開diagnosticへ出さない。必要な証拠は私有保存 |
| RH-017 | COREなしでも起動・実行・圧縮・保存・再開が成立する |
| RH-018 | CLIとCOREは同じService/Kernelとschemaを使う。UIごとに安全契約を実装し直さない |
| RH-019 | 新しいRun境界でもTask/根拠を維持。新RunIDを発行し、古いActionの結果を捏造しない |
| RH-020 | token数は最終Backend入力を対象にする。広告値・推定・上界・実測を混同しない |
| RH-021 | 契約未対応はcapability単位で明示し、未実装をgateという言葉だけで将来へ逃がさない |
| RH-022 | policyによる許可とOS sandboxによる強制隔離を別capabilityとして表す |
| RH-023 | actの回復は最大2Attemptで可視化し、CompactionやToolへ無断流用しない |
| RH-024 | Context revision/由来はproducerからconsumerまで一貫し、CLIにもintake receiptを残す |
| RH-025 | 再開Runの全limits/deadlineを明示し、旧Runの消費/unknownを消さない |
| RH-026 | source参照pin、配布指定pin、稼働binary、試験対象を別に記録する |

schema/hash/型は構造と対応関係の検証であり、自然言語の意味が完全に正しい証明ではない。意味保持は実モデル試験と継続作業で評価する。

## 6. Session、Task、Runと所有権

Sessionは一定期間の作業対話、Threadは継続する作業文脈、Turnは入力から安定状態までの相互作用。各新TurnはRoot Taskを一つ持つ。CORE委譲では、そのTaskはCORE親Taskの子でもある。外部Session/Thread/TurnのIDは`upstream`参照に置き、HarnessのIDと同一視しない。

Taskは一つの仕事、Runはその一回の実行、Actionは一つの論理操作、Attemptはその物理試行。全ID名/prefix/UUIDv7は既存Identity Canonicalを継承する。新規issuerとしてHarnessの担当範囲を同正本へ追加する。[C09]

Process再起動、checkpoint resume、lease再取得、実行Agent変更では**新RunID**を発行する。TaskIDと原本は維持する。resumeという新Triggerには新TraceIDを発行する。旧Runを新Runへ改名しない。新しいユーザー依頼は新Turn/Taskであり、既存Taskのresumeとは別。

COREは一つの委譲Actionを持ち得るが、そのActionとHarness内部の各Tool Actionは別の論理操作。Harness結果をCOREへ返す際はsource referenceとReceiptIDを引き継ぎ、内部ToolをCORE Actionとして再実行しない。

再開要求は`ResumeInput.limits`に新Runの全予算を必須で指定する。新Runのdeadlineは受付時から計算し、旧Runの消費と不明試行は履歴へ残す。CLIの初期値利用は送信前に解決し、null/省略による暗黙継承をしない。詳細は実装仕様§2.3とRETRY_CONTRACT。

## 7. 実行と完了

入力認証/設定検証 → durable intake → writer取得 → source load → Context assembly → budget → 必要時Compaction → LLM → 終端検証 → Tool validation/認可 → durable Attempt → 実行 → Evidence/結果確定 → 次step、を共通Kernelが行う。

Modelが最終本文を返しても、実行に関する既知のcontradiction、未確定Tool、出力不完全があれば完了としない。Run `completed`は有効な最終応答が確定したことだけを示す。

別field`verification_status=passed|failed|not_run|unknown`とEvidence参照を持つ。提出された検証手順に合格しても、未知の要件まで全て達成した証明ではない。CORE親Taskの採用はCOREが行う。CLIも検証状態とRun状態を両方表示する。

## 8. 入力、停止、引継ぎ

追加入力は`next_step`、`interrupt_current`、`next_turn`を明示する。COREは意味判断してこの値を付けられる。CLIは通常追記をnext_step、`/stop`をcancel、明示的な割込み入力をinterrupt_currentにする。自然言語の「やめて」を即時停止commandとして検出できると保証しない。

普通のqueue追加だけではContext snapshotは変わらない。適用した時にcontext_revisionを進める。interrupt/cancel/権限revocationはcontrol_revisionを即時に進め、次のdispatch/commitを拒否する。到着順・受付済み・適用済み・次Turn待機をreceiptで識別する。

cancel受理は停止信号を記録した事実であり、外部副作用が巻き戻ったことではない。実行中processを停止/照会し、done/failed/cancelled/unknownを確定する。反応しないprocessがあれば再実行せずblockedにする。

初回のhandoffは、旧clientがdrainまたはcancelし、writerを解放した後、新clientが同じstoreの同じTaskをresumeする方式。旧processがliveである可能性がある間、lease timeoutだけで強制乗取りしない。別hostへの移動はoffline export/restoreの別契約とし、ローカルSQLiteの共有mountで代替しない。

全ての入力には、CLIを含めて[CORE_INTEGRATION](CORE_INTEGRATION.md)のIntakeReceiptを作る。profileのorigin宣言と実際の受付経路、raw hash、原本参照、署名検証状態を同一受付transactionへ記録する。設定値だけで後から原文の出所を作り直さない。

## 9. ContextとPrompt

配置責務`prompt_block`と履歴種別`history_kind`を別fieldにする。COREが供給するCharacter、Stable、Recall、Variable、Userの五区分を保持し、文字列へ潰さない。standaloneではCharacterを明示profileの実行役説明、Recallを空または利用者指定の参照として扱い、CORE人格を自動取得しない。

初期promptはCharacter→Stable→Recall→Variable→User、通常Tool継続ではその後にassistant ToolCallと対応するToolResultを順序どおり追加する。「毎回userが最後」を守るためにTool履歴を並べ替えてはいけない。

Host policyと権限はprompt外で強制する。AGENTS.mdやSKILL.mdはtrusted workspaceのscope付き作業規則として読み、Human本人の指示やhost権限へ昇格しない。LLMの形式差・template・thinking指示はLLM Runtimeへ残す。

公開ContextBlockは`kind / text / revision / source`の4fieldである。COREまたはCLI側がCORE_INTEGRATIONの決定的規則でrevisionを作り、Harnessは受信したtextのdigestを内部PreparedContextBlockへ計算する。COREの既存static hashだけでこの供給機構が実装済みとはしない。

SelectionとSummaryの段promptはHarnessがversion管理する。完全な必須文面を`prompts/`へ含める。COREのActor promptとはownerが異なる。旧指示の失効対象本文とnegative-validation素材はSummary入力へ混ぜない。

## 10. LLM、能力、予算

HarnessはRenCrow_LLM Gatewayだけを呼び、物理Backendへ直結しない。CORE委譲では許可されたexecution alias、standaloneでは許可されたmodel routeを選ぶ。どちらもGatewayが解決したbinding revisionを固定する。Model routeは物理URL/ファイル名ではない。[L02]

新しい`harness-v1` contractをChat CompletionsとRuntimeの共通境界に実装する。**一つのHTTP生成要求につきBackend生成は最大1回**とし、下位層が隠れて追加生成しない。論理的なact Actionは初回と最大1回の明示retryを持てる。retryは同Actionの新Attemptと新RequestIDで記録する。正常にBackendへ渡した各要求ではbackend_attempts=1、生成前拒否では0、成否が分からない場合はnullである。

actのretry許可、最大回数、待ち時間、禁止条件は[RETRY_CONTRACT](RETRY_CONTRACT.md)で固定する。モデル固有の回復補正はRenCrow_LLMが実装し、Harnessは事前に許可されたprofileのIDだけを指定する。Selection/Summaryの失敗はretryせずEmergencyへ進む。補正による改善を未検証のまま保証しない。

初回対象の少なくとも一つのローカルbindingで、**最終prompt計数を公開する新APIの実装・配備・試験までをreleaseに含める**。現行GatewayにAPIがないことを理由にbudget_unverifiedのまま製品を完成扱いしない。

Budgetは`verified_exact`、`verified_bound`、`estimated`、`unverified`の4状態。productionの生成/圧縮採用はexactまたは根拠付きboundだけで行う。estimatedはdiagnostics/隔離評価のみ。unverifiedでは状態/原本読取と無生成dry-runを許すが、新規Model要求・圧縮commitを許さない。手動操作やEmergencyもこの境界を迂回しない。API実装済みの初回bindingを使うことで通常利用が成立する。

上界だけで適否や縮小を証明できない場合は追加のexact countへ進む。それもできなければBudgetUnverifiedで止め、実際のtoken超過と断定しない。token上限値、出力予約、model/backend/tokenizer/template/normalization revision、計数根拠を記録する。byte/4等を実測としない。

## 11. Compaction

### 11.1 通常処理

Prepare → 候補がある場合だけSelection → Preflight → Summary → Validate → Commit。LLMはSelection最大1要求、Summary最大1要求。NoCandidatesではSummaryのみ。token計数はLLM生成要求に数えない。[D01][S01]

source.inventory、hash、ID、range、coverageはHostが構成する。Modelは失効候補と要約内容を選ぶだけで、全IDを転記しない。

### 11.2 Human、Automation、撤回、完了

Humanは認証済みの本人入力として受理された原文。Automationは認証済みの代理/監督入力。Unknownは保護し、意味推測でHumanへ昇格させない。Humanは後のHumanによる訂正/撤回だけで失効可能。Automationは後のHumanまたはAutomationで失効可能。継続制約は根拠なしに削除しない。

`drop_superseded`の`replacement_handle`は、代替実装要求だけでなく**後の明示的撤回文を含む失効根拠**を指す。「Aをやめて」も根拠になり、代替Bを必須にしない。`basis=amendment|revocation`で区別する。根拠となった後続発話を削除対象と同時に消さない。

`replace_completed`は検証済みcompletion handleを必須とする。自動completion linkは宣言済み入力1件、一意なterminal process.exec pair、call/output両方の全文が各2,048 bytes以下（付属A F12の「各」を継承し、合計の上限ではない）、nonpartial/nonprotected/nonactive、stable identityの範囲に限る。現行Human条件と採用済みAutomation拡張を継承する。範囲は実装仕様で固定する。linkは意味的完了の証明ではない。Model判定とHost検証の両方を通す。[S02][D01]

### 11.3 Preflightと失敗分類

安全な重複投影除去等はPrepare前/中に完了する。その後のactive Human＋Protected＋initial context＋minimum valid summaryが容量を超える場合は**CapacityBlocked。Emergencyへ進まない**。これは通常のModel/semantic failureとは別である。[S01]

| 状況 | 処理 |
|---|---|
| Normalのsemantic/model failure | 決定的Emergency |
| Normal最小候補のno-fit | CapacityBlocked、Emergencyなし |
| Emergencyで安全に縮小可能 | EmergencyCompacted |
| 安全に収まらない、縮まらない | CapacityBlocked |
| 確定原本/metadataの決定的不整合 | IntegrityBlocked |
| 保存成否不明 | RestartRequired |
| cancellation / snapshot競合 | cancelled / stale。上記5分類へ混ぜない |
| budget未検証 / LLM strict未対応 | unavailable。capacityの実測結果とは扱わない |

### 11.4 原本参照とEmergency

Observationは保存済み原本と一意に一致するinactive/text-onlyの結果だけを範囲・hash・total size・partial付き参照へ投影する。添付やmixed mediaは初回の要約対象外でprotected。

EmergencyはLLMを使わず、前回accepted semantic Summaryと未処理Workを保持する。前回durable checkpointより古い結果/excerptを正本で照合して小さいref-only markerへ変えられるが、これだけでsemantic boundaryを進めない。後続Normalは原本から合法な材料を再構成できる。連続Emergencyで必ず無限に続けられるとは保証しない。

## 12. 保存、復旧、データ権限

Harnessの実行状態はHarness所有の`harness.execution` storeへ保存する。初回は一つのローカルSQLite storeにdomain record、append-only event、raw Evidence、checkpoint、receiptを持つ。CORE DBと同一transactionだと仮定しない。新しい保存責任があるため、Storage Proposal/Manifestで独立storeを正式登録する。[C10][C12]

保存先は明示設定の絶対path。production標準はmodule別subtree、standaloneは利用者が初期化したprivate data rootを使用する。path不在/permission不足を理由にhome/temp/repositoryへ暗黙fallbackしない。

保存前に同じbytesを検証/hashし、そのbytesをtransactionへ渡す。checkpoint、receipt、current pointer、採用eventを一括確定する。公開前にdurabilityを満たす。private rawはpublic logへ出さない。

壊れた派生indexは健全な原本から一回再生成できる。壊れたSummary候補は不採用。不良な計測だけを識別できる場合はmetrics_missingとする。確定checkpoint/原本の不整合は停止し、古いcheckpointへ黙って戻らない。レイヤー別原本隔離/自動修復は初回の採用対象ではない。[D03]

## 13. Toolと安全性

初回の組込みfile toolはworkspace-containedな操作とexpected digestによる編集を提供する。曖昧なmatch、知らないpath、読み取っていない変更前bytesを、Modelの推測で補修しない。

process実行は実行ファイルとargvを分ける。shell文字列は明示したshell capabilityのみで扱い、通常argvをshellへ連結しない。環境はallowlistで作り、Gateway tokenを子processへ継承しない。Process tree停止はOS別実装で行うが、これをOS sandboxと呼ばない。

安全modeは`structured_only`（組込みfile/read/search/editのみ、任意processなし）と、利用者が配置時に許可する`trusted_host`（自分の開発機でprocess実行、OS隔離なし）を初回提供する。COREでも後者は明示deployment policyが必要。shared/敵対的環境の`isolated`は実際のsandbox adapterが受入されるまでunavailable。自動的にtrusted_hostへ落とさない。

trusted_hostでは、子processの全filesystem/network accessをHarnessのpath判定だけで制限できない。Goの環境変数除去、workspace gate、command policyは補助であり、悪意ある同一OS userや任意scriptからの完全隔離を保証しない。隔離が必須の用途を初回host modeで代用しない。

COREのNo-Human-Gateに合わせ、初回の製品runtimeにapproval_waitを作らない。許可済みpolicyを同期判定し、execute/rejected/blockedを返す。人向けCLIでもpolicyの変更は実行とは別の明示操作で行う。論文のinteractive approval推奨を無条件には採用しない。

## 14. 拡張と論文の利用

論文§2.3の七要素と、p.4図1/p.5のInterface/Session substrateを網羅性の確認軸に使う。Loop、LLM接続、Tool、Context、Safetyを本体に持ち、OrchestrationはCOREからの委譲境界、Extensibilityはskill/Tool/hookの契約として小さく始める。subagent自動生成は初回意図的に不採用。[P01]

CodexはCLI/プログラム入口、session、typed progress、停止/再開の実装参考。wire schema、保存形式、model依存prompt、vendorの認証はそのまま移さない。[O01][O02]

論文の推奨18項目への対応と不採用理由はREFERENCE_DECISIONSに記載する。source比較の論文であり、同条件の性能/信頼性benchmarkではない。速度改善・他モデルでも成功するという主張は実機試験の後に限る。[P01 §15.6]

## 15. 受入と初回完了

1. COREなしCLI、CORE native clientの双方で同じ課題を実行し、作業原本と結果を取得する。
2. 元のTaskのままprocess停止/再起動後に新Runでresumeし、結果不明の副作用を再実行しない。
3. 初回実bindingのChat strict、計数、Normal/Emergency、manual/auto/overflowを実経路で受入する。
4. H01〜H26とA01以降の必須試験、RH-001〜026、全公開methodのschema/negative試験を満たす。
5. P6AはmacOSでCLIとCOREの両方を先行受入し、P6Bでsession/forkと三OS native入出力/保存/停止契約を受入する。MLXはMacのoptional computeであり、三OSでMLXを動かす要求ではない。
6. Source/build/config/binding/template revision、試験入力、観測値、未実施範囲をreceiptへ残す。

P6Aは正式完成ではなく、P6Bが初回製品の完了点である。P7の旧Switch thread移行を利用しない運用は、その変換未実装だけでP6Bの新規作業利用を否定しない。ただしstandalone、CORE連携、原本保存、Compaction、resume、LLM計数を将来事項にしてP6Bを完了とはしない。


[C01]: SOURCES.md#c01
[C09]: SOURCES.md#c09
[C10]: SOURCES.md#c10
[C11]: SOURCES.md#c11
[C12]: SOURCES.md#c12
[D01]: SOURCES.md#d01
[D03]: SOURCES.md#d03
[L01]: SOURCES.md#l01
[L02]: SOURCES.md#l02
[O01]: SOURCES.md#o01
[O02]: SOURCES.md#o02
[P01]: SOURCES.md#p01
[S01]: SOURCES.md#s01

[S02]: SOURCES.md#s02

## 16. 実装前確定事項（v0.2.2）

17点の最終照会への回答をIMPLEMENTATION_DECISIONSで確定した。構成/責務は維持し、byte合同、Modelへ見せる役割と順序、stored Context投影、Policy/拡張資産、Event、Shiro切替経路、stream/計数頻度を具体化する。新たに拡張したフィールドは初回release前の同一v1契約改訂で、既存productionへ配備済みという意味ではない。

actは初回からstream、各Attemptで非生成measure→generateの二HTTPを許容。計数のためにLLMを二回生成しない。Model固有変換後のrequest_digestはLLM owner値としてechoし、入力同一性は双方が計算するinput_digestで確認する。

