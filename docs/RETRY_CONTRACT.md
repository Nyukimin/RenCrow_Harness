# LLM試行・回復・予算契約

RH-RETRY-001 / v0.2.2 / 決定: AD-04/05/09。実装担当: Claude。
この文書は**新設する規則**である。現行Gatewayの自動再送が既にこの契約を満たすという意味ではない。

## 1. 単位を固定する

- Model Action: 同じ適用済みContextで、通常の次の一手を決める論理生成処理。actでは一つのActionに最大2Attempt。
- Attempt: Harnessが認可して送る一回のHTTP生成要求。初回ordinal=0、retry=1。全生成はこの単位で台帳に入れる。
- Backend generation: 一つのAttemptにつき0または1回。receiptを失って確認不能なら回数はnull。生成を伴わないmeasureやTCPレベルの再送とは区別する。
- Step: actの論理Action開始で1増やす。retryはStepを増やさず、GenerationAttempt予算を必ず消費する。

同じActionの`args_bytes`は論理要求と許可された回復policyの不変snapshot。retryごとに異なる実送信bytesはmodel_callsのEvidenceに保存する。Tool Actionのargumentsは変更不可で、この生成回復規則をToolの再実行へ流用しない。

## 2. 初回の上限

`RecoveryPolicy.contract_version=act-recovery/v1`、`max_attempts_per_act=2`を固定する。callerは増やせない。起動設定は利用を許すprofile集合を指定し、Run受付時にrevisionを固定する。

`Limits.max_generation_attempts`を新規必須fieldとする。初期設定例は32。act、retry、Selection、Summary、stale後の新Compactionの全HTTP生成要求を合算する。非生成measureは含めないが、同じRunのdeadlineには含める。`max_model_steps`は通常actだけの上限で、Compaction生成を隠す予算ではない。

残予算がない通常actはincomplete / GENERATION_BUDGET_EXHAUSTEDで終える。Compactionの段生成を始められない場合はNormalのモデル利用不可として、追加生成なしのEmergencyだけを試せる。Emergencyの容量判定・原本検証は省かない。

送信前transactionで残予算とcontrol/fenceを照合し、Attempt preparedと予算消費を保存する。送信後にtokenが不明でも試行予算は返却しない。新しいRunには新limitsを指定するが、Task累積消費やunknownを消さない。

## 3. act失敗からの唯一の遷移表

全retryは、stage=act、attempt ordinal=0、generation_stateがnot_startedまたはterminal、Tool未dispatch、control/binding/policy不変、deadline/試行予算あり、を前提とする。前提不成立はretryしない。

| normalized code | 回復profile | 待ち | 再試行できない場合のRun結果 |
|---|---|---|---|
| REASONING_ONLY / EMPTY_FINAL_CONTENT | terminal_output_onceが許可・対応済みならそれを使用。それ以外はsame_request | 0ms | failed / MODEL_OUTPUT_INVALID |
| RAW_TOOL_MARKUP / MODEL_OUTPUT_SCHEMA_INVALID | 同上。壊れた出力は捨てずEvidence保存、Contextには入れない | 0ms | failed / MODEL_OUTPUT_INVALID |
| CONNECT_FAILED / UPSTREAM_TRANSIENT | same_request。not_startedまたはterminalを確認できる場合のみ | 1,000ms | failed / MODEL_TRANSPORT_FAILED |
| RATE_LIMITED / QUEUE_TIMEOUT | same_request。not_startedであることが必須 | max(1,000ms, retry_after_ms)。10,000ms超またはdeadlineを越える場合は再送しない | incomplete / MODEL_TEMPORARILY_UNAVAILABLE |
| CONTEXT_LIMIT_EXCEEDED | 同じpayloadのretry禁止。fit再評価と共通Compactionへ一度だけ渡す | なし | blocked / CAPACITY_BLOCKED、またはBUDGET_UNVERIFIED |
| LENGTH / INCOMPLETE / premature stream EOF | 不完全なTool引数を全拒否。初回の自動retry対象外 | なし | incomplete / MODEL_OUTPUT_TRUNCATED |
| REFUSED | 回避用retryをしない | なし | rejected / MODEL_REFUSED |
| BINDING_CHANGED / UNSUPPORTED_CONTRACT / AUTH_FAILED / hidden retry検出 | 経路や条件を変えて迂回しない | なし | blocked / 各契約code |
| timeout/接続断でgeneration_state=unknown | concurrent再生成しない | なし | blocked / MODEL_GENERATION_OUTCOME_UNKNOWN |
| cancelled / revoked | 取消処理を優先 | なし | cancelled |

上限に達した後も同じsignatureで新しいActionを作ってretryを続けない。2回目の失敗は表の終端分類へ進む。1回目のfailed completionにToolIntentが混在しても、未採用ならToolは一件も実行してはいけない。

初回は通常actのgeneration timeoutでも、Runtimeが終了済みと証明できた場合のみUPSTREAM_TRANSIENTとして上限内retryできる。clientの待ち時間が切れただけでは終了済みではない。unknownを勝手にterminalへ変えない。

## 4. profileとモデル差

公開profile IDはモデル中立の`same_request`と`terminal_output_once`のみ。RenCrow_LLMが選択bindingに対する対応可否・revision・変更可能項目を公開する。HarnessはQwen、GLM、thinkingタグ、XMLを条件分岐しない。

same_requestは同じ適用Context・tools・出力条件・生成optionsを送る。新RequestID等の運用metadataを除けばrequest_digestは同じ。

terminal_output_onceはLLM ownerが実装する**事前許可された一回だけの生成形式補正**である。許す変更は、(1)可視の最終本文または正しい宣言済みToolCallを要求する形式専用suffix、(2)role/profileで明示許可された場合のthinking無効化、だけ。temperature、Model、route、権限、Tool集合、出力上限、要求本文、source削除は変更不可。suffixはモデルごとのLLM profileにversion付きで置き、Harness promptへ複製しない。

初回Qwen profileではこの回復を実装し、reasoning-onlyとtool記法漏れの実fixtureで使えることをWP05で受入する。対応を表すflagだけでは完了しない。既存のrole thinking policyが回復変更を許さない場合、LLM/profileの採用済み回復規則へ許可範囲を追加し、通常要求のthinkingを勝手に変更しない。

適用条件は`host許可 ∩ Runの固定policy ∩ LLM profileの許可`。許可されたterminal_output_onceが非対応なら、同条件のsame_requestを一度使う設計であり、別モデルfallbackではない。この分岐と理由もretry receiptに残す。初回対象の正式受入ではterminal_output_once実経路を別途必須とする。

## 5. measureとreceipt

retryの前に、選択profileを含む最終要求を再measureする。binding_fingerprintはモデル・Backend・tokenizer・template・正常化版・基本実行profileの同一性を表す。request_digestは最終messages/tools/optionsと回復profileの有効変換を表す。回復によるrequest_digest変更は許可範囲を検査するが、物理Modelの同一性は変えない。

各ModelAttemptReceiptに、ActionID、AttemptID、ordinal、RequestID/ResponseID、stage、base_request_digest、request_digest、binding_fingerprint、profile/revision、applied_transformations、failure code、generation_state、backend_attempts、usage既知/未知を保存する。モデルのraw出力は別のprivate Evidenceで保存する。

`model.retry_scheduled`と`model.attempt_started`は型付きevent。eventのevidence_idからModelAttemptReceiptを取得できる。人向け表示は「最終出力がなかったため再試行1/1」等にし、COREは同じeventを処理する。部分streamは旧Attemptのprovisionalとして破棄表示し、新Attemptの文字列へ黙って連結しない。

## 6. Compactionには適用しない

Selection、Summaryはいずれも最大1生成。failureからretryを挟まずEmergencyへ進む。Normal最低候補no-fitはEmergencyへ進まずCapacityBlocked。budget未検証はunavailable。Compactionの論理要求最大2を、actのretry規則で4回に増やさない。

段別の入力形式とthinking設定は最初の要求を作る前にLLM側stage profileとして固定・計数する。Summary失敗後にno-thinkingへ切り替えてもう一度生成することは初回の契約外。連続Emergencyを「品質同等」と見なさず、実課題の指示保持、未完了、Normal率、総試行数を受入証拠にする。

## 7. Resumeの予算

ResumeInput.limitsは全部必須。host policyを超える指定はINVALID_LIMITSで拒否する。新Runは0のstep/attempt消費と新しいdeadlineから始めるが、旧Runの試行・usage・unknownを保持する。意味上の未完了も保持する。許可済みTask全体上限がある運用では、その残余も上位ownerが検査し、Run再開で上限を迂回しない。

CLIのresumeは、保存limitsを初期値として表示し、現在profileの上限と突き合わせて**具体的な値を送る**。expiredな絶対deadlineは再利用しない。COREは自身の残余予算以内のlimitsを必ず渡す。再開のidempotency payloadにはlimitsも含め、同key・異limitsを拒否する。

## 8. 必須検査

A41〜A45で最大2Attempt、新ID、補正の正規化/計数、cancel/unknown/期限、Compaction追加生成なしを検査する。A47で新Run limits、A52で不明Backend生成と予算記録を検査する。実装前の付属fixture計算は製品retryが動いた証拠ではない。

## 9. source codeからの確定mapping

ERROR_MAPPINGの表をF19の唯一のsource→normalized対応とする。MODEL_OUTPUT_DEGENERATEは自動retryしない。MODEL_UNAVAILABLEはblocked。未知理由のNORMALIZATION_ERRORはMODEL_CONTRACT_FAILED、停止を確認できないfirst-output timeoutはunknown。HTTP状態やretryable=trueだけでF31の許可条件を満たさない。

input_digestは論理要求の一致、request_digestは実効回復変換後の一致。両方をModelAttemptReceiptに保持する。SSE Tool引数はSTREAM_CONTRACTどおり終端までbufferし、retry表示は旧Attemptと連結しない。

