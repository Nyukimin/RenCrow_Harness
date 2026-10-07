# act streamと計数呼出しの固定契約

RH-STREAM-001 / v0.2.2 / 決定AD-15・AD-16 / 実装: Claude

## 1. 採用する呼出し方式

初回からactは**stream=true**。Selection/Summaryはstream=false。act要求はstream_options={include_usage:true}、stage要求はstream_options=null。未対応bindingをnon-streamへ自動fallbackしない。CLIもCOREも同じstreamを消費する。Backend自体が非streamの場合はRuntimeが完全結果からstrict streamを生成できるが、偽の逐次token速度を報告しない。

HarnessはChatのprotocol fragmentを組立てる。Qwen XMLやthinking外装の解析はRuntime。ToolIntentは全体を受信・終端検証するまで実行しない。Modelのreasoningを人向けdeltaへ出さない。

## 2. strict SSE

UTF-8 SSE。通常data frameはChat chunk object（choicesはindex=0のみ）。最大frame1MiB、集約最終出力1MiB、Tool arguments合計1MiB、最大128callかRun上限の小さい方。CRLFとLFのSSE行区切りは受理するが、data文字列の内容は変換しない。複数data行はSSE規則のLF連結、commentは無視する。重複JSON key、unknown必須field、UTF-8不良は拒否。

正規化後のdeltaはrole=assistant（初回だけ、省略も可）、content文字列、reasoning_content文字列、tool_callsのいずれか。content=null/field省略は追加0文字。refusalを検出した場合Runtimeはterminal outcome=refusedにする。複数choice、過去responseと異なるid、順不同index、重複終端はcontract failure。

新しいtool_calls[index]は0から連続。最初のfragmentに完全なid、type=function、function.nameを入れる。後続は同じindexとfunction.argumentsの追加文字列。id/type/nameが繰り返される場合は完全一致のみ許し、nameを重複連結しない。Backendが名前をfragment化する場合、Runtimeがheaderを確定するまでbufferする。argumentsは順に連結し、途中の部分JSONから実行候補を作らない。

Runtimeは**一つの終了frame**を返す。

```text
event: rencrow.terminal
data: {StreamTerminal型の単一JSON}

data: [DONE]

```

StreamTerminalのschemaはllm_contract.schema.json。outcome、finish_reason、code、provider_response_id、usage、harness_receiptを持つ。errorはoutcome=error、code非null。completedはcode=null、finish_reasonはstopまたはtool_calls。usageがないfieldはnull。usage取得成功を作り出さない。

通常chunkのfinish_reasonの後にusage-only chunkを許すが、rencrow.terminalの後は[DONE]以外禁止。Harnessはterminalと[DONE]の両方を確認してから成功受理。EOFが先なら出力全体を不採用とし、terminal receiptを観測済みなら生成状態terminalを維持しつつINCOMPLETE、未観測ならunknown。実引数がvalid JSONでもlength/incompleteなら一件もToolを実行しない。

生成前の拒否は非2xx JSONのStrictError。同じreceiptを付ける。送信後errorはSSEでoutcome=error、実generation_stateを返す。Gatewayがstopを依頼しただけではterminal receiptを作らない。未停止の場合unknownで返す。

## 3. 人/COREへの進捗

contentだけをprogress/deltaでprovisional公開。ordinalはAttempt単位0始まり。retryではprogress/resetで旧Attemptの表示を明示的に破棄し、新Attemptへ切替。確定Eventとは別のqueueで、欠落はprogress/gap。Tool argsを実行途中の事実として表示しない。元の出力は診断Evidenceへ保存できる。

reader・writer・control受付を分け、同じgoroutineで長いModel readの間cancel受付を止めない。Context cancelでupstream HTTPを中断するだけでなく、Runtimeが生成停止を扱える契約へ接続する。確認不能はunknown。初回unknownの自動再送はしない。

## 4. 毎生成のmeasure: 採用決定

**新しい各Attemptにつき、1回のmeasure→1回のgenerateという二つのHTTP要求を初回の標準として許容する。** 再試行も別Attemptなので再measureする。人向け応答性より無制限安全化という意味ではなく、最終入力の同一性と予算を先に確定する具体的な初回構成である。

measureはtokenizer/templateまで、LLM forward/prefill/decode/KV確保を実行しない。HTTPが二回であることと、推論を二回行うことは別。計数時間、HTTP待機、queue、generationを別計測し、実測無しに軽い/速いとは主張しない。

同一操作中の不変PromptPlanについて、Preflight/候補検査/commitが同じmeasure証拠を再利用するのは可。ただしinput_digest、request_digest、binding_fingerprint、stage/recovery、出力予約、safety margin、対象context/control revisionを全て一致させる。次Attemptへ跨ぐcacheは初回採用しない。candidateメタデータを変えただけで、modelに見せるJSONまで変わった場合は再計数する。

計数完了から生成開始までにbindingが変わればBINDING_CHANGED。digestが違えばREQUEST_DIGEST_MISMATCH。古い証拠で通さない。最適化（Runtime prepare handle、計数cache、単一requestでのadmission）を将来追加する場合は、同じ保証の実証後に契約更新する。初回の実装者判断で省略しない。
