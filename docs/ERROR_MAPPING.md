# GatewayからKernelまでの失敗正規化

RH-ERRORS-001 / v0.2.2 / F19・F31 / 設計: ルミナ
以下は新strict経路の契約。旧経路のcode名を見ただけで生成の開始/終了を推測しない。

## 1. 先に生成状態を確定する

receiptのgeneration_stateは次の実証だけで付ける。

- not_started: Runtime/Backendへ生成をdispatchしていない、またはRuntimeが生成前拒否を明示した。backend_attempts=0。
- terminal: 生成が開始され、Runtimeが完了または停止完了を観測済み。backend_attempts=1。出力の品質や可視本文の有無とは別。
- unknown: 開始または終了を確認できない。backend_attempts=null。client timeout/EOFだけではterminalにならない。

HTTP429/503/504、retryable=true、streamの[DONE]だけでは根拠にならない。strict receiptが欠ける/矛盾する場合はMODEL_CONTRACT_FAILED。送信後に何が起きたか不明ならgeneration_state=unknownを保存する。制御取消を最優先し、その後はunknownがretryを遮断する。unknownのまま次Actionで同じ生成をやり直さない。

生成前にnormalizeできずdigestが得られなかったerrorでは、GatewayAttemptReceiptのinput_digest/request_digest/binding_fingerprintをnullにできる。期待値を実測値としてechoしない。成功receiptでは全て必須の実値。Harnessが保存するModelAttemptReceiptの期待値は直前measure由来、観測receiptは別のraw Evidenceで保持し区別する。

## 2. 対応表

| source code / 観測 | normalized code | generation状態と処理 |
|---|---|---|
| CAPACITY_EXCEEDED（dispatch前のqueue待機切れ） | QUEUE_TIMEOUT | 0/not_started。act初回だけsame_request、待ち規則はRETRY_CONTRACT |
| TARGET_UNAVAILABLE（接続を確立できずrequest bytes未送信が確認済み） | CONNECT_FAILED | 0/not_started。act初回だけsame_request |
| TARGET_UNAVAILABLE（開始後の一時失敗、停止完了確認済み） | UPSTREAM_TRANSIENT | 1/terminal。act初回だけsame_request |
| TARGET_UNAVAILABLE（上記の証拠なし） | MODEL_GENERATION_OUTCOME_UNKNOWN | unknown。blocked。再送なし |
| NORMALIZATION_ERROR（terminalのJSON/schema出力違反と識別済み） | MODEL_OUTPUT_SCHEMA_INVALID | 1/terminal。act初回だけ回復profile |
| NORMALIZATION_ERROR（未知理由、normalizer自体の不整合） | MODEL_CONTRACT_FAILED | known stateを維持。failedまたはunknownならblocked。再送なし |
| MODEL_NOT_ALIVE | MODEL_UNAVAILABLE | 生成前なら0。blocked、別modelへ切替なし |
| TARGET_FIRST_OUTPUT_TIMEOUT（Runtimeが停止完了まで確認） | UPSTREAM_TRANSIENT | 1/terminal。act初回だけsame_request |
| TARGET_FIRST_OUTPUT_TIMEOUT（停止確認なし） | MODEL_GENERATION_OUTCOME_UNKNOWN | unknown。blocked。HTTP504をterminalと扱わない |
| DEGENERATE_OUTPUT | MODEL_OUTPUT_DEGENERATE | terminalならfailed、停止確認なしならunknown/blocked。自動retryしない |
| EMPTY_FINAL_CONTENT | EMPTY_FINAL_CONTENT | 1/terminal時だけact回復対象。reasoningだけと識別可能ならREASONING_ONLY |
| REASONING_ONLY | REASONING_ONLY | 1/terminal。act初回だけterminal_output_onceまたは許可されたsame_request |
| RAW_TOOL_MARKUP | RAW_TOOL_MARKUP | terminal時だけ回復対象。streamで早期検出したら生成停止確認までunknown |
| MODEL_OUTPUT_SCHEMA_INVALID | MODEL_OUTPUT_SCHEMA_INVALID | 同上。生成不完全な引数を救済実行しない |
| CONTEXT_LIMIT_EXCEEDED | CONTEXT_LIMIT_EXCEEDED | 0/not_startedまたは確認済みterminal。共通Compactionへ1回。同じpayloadをretryしない |
| BINDING_CHANGED | BINDING_CHANGED | 0/not_started。blocked。明示した新Run/configでのみ再評価 |
| INPUT_DIGEST_MISMATCH / REQUEST_DIGEST_MISMATCH | 同名 | 0/not_started。blocked。自動再計数で隠さない |
| UNSUPPORTED_CONTRACT / UNSUPPORTED_RECOVERY_PROFILE | 同名 | 0/not_started。blocked。能力不足を無視しない |
| AUTH_FAILED、認証済み経路での401/403 | AUTH_FAILED | 生成前拒否。blocked。別credentialへ自動切替なし |
| RATE_LIMITED（生成前の429を確認） | RATE_LIMITED | 0/not_started。act初回だけsame_request |
| finish_reason=length | LENGTH | terminal確認時incomplete。Tool全未実行。自動retryなし |
| finish_reason=incomplete | INCOMPLETE | 同上。状態不明ならunknown優先 |
| 途中EOF、SSE終端の欠落 | INCOMPLETEまたはMODEL_GENERATION_OUTCOME_UNKNOWN | 有効terminal receiptが既にあれば前者、なければ後者 |
| Modelが正規の拒否を返した | REFUSED | rejected。回避retryなし |
| CANCELLED / PERMIT_REVOKED | 同名 | cancelled。外部生成の実状態は別に保存 |
| BUDGET_UNVERIFIED | 同名 | blocked/unavailable。capacity超過とは別 |
| 未列挙code、hidden_retry=true、矛盾するreceipt | MODEL_CONTRACT_FAILED | stop。本文から都合のよいcodeを推測しない |

旧codeの詳細が足りない場合、strict normalizerに新しいreason判定を追加する。その判定で参照したupstream code、HTTP状態、停止receiptを私有Evidenceへ保存する。公開errorはsource_codeを併記できるがraw bodyを出さない。NORMALIZATION_ERRORを一律schema failureとしない。

## 3. Kernelへの結果

actでretry可能でもordinal=0・予算/期限・Tool未実行・control/binding不変を満たした場合だけF31が再試行する。2回目は元の分類で終える。DEGENERATE_OUTPUTは初回から再試行なし。`MODEL_OUTPUT_DEGENERATE→failed`、`MODEL_UNAVAILABLE→blocked`、`MODEL_CONTRACT_FAILED→failed`（契約不足系はblocked）。unknownは`blocked/MODEL_GENERATION_OUTCOME_UNKNOWN`。

Selection/Summaryは同じ正規化を使うがretryしない。通常の出力/一時Model失敗はEmergencyへ、strict/budget/authority不成立はunavailableまたはIntegrityBlocked、cancelはcancelled。unknownでもEmergencyの非生成処理は可能だが、未終了生成が残る間、次のact生成はしない。Runtimeの停止確認または明示再照会を必要とする。

`contracts/error_mapping.json`は表のcode網羅検査用。具体的な条件をこの本文と`examples/wire/error_cases.json`に記載する。private error文の部分一致だけでnot_startedに昇格しない。
