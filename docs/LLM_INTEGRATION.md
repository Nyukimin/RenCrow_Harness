# RenCrow_LLM接続の実装契約

RH-LLM-001 / v0.2.2 / 設計責任: ルミナ / 実装: Claude
追加API・profileは新設対象。RenCrow_LLMの既存APIが既に対応しているとは扱わない。

## 1. 現状と固定する経路

基準LLM commitは`d2166b4dffa9c21341dd2cace8c39cba690288c0`。既存入口はchat/completions、responses、embeddings、models/status/health。参考snapshotのraw tool markup検出・回復再送はResponses経路にあり、Chat経路は同等ではない。json_object+stream拒否点はRuntime。token計数APIは未実装でcatalogはdeclared。[L01][L02][L04][C14][L05]

HarnessはGatewayの正規APIのみを呼ぶ。COREの内部Providerは経由しない。初回は設定base_urlを`http://127.0.0.1:<configured-port>/v1`とし、末尾の`/`だけを除去して`/chat/completions`や`/context/measure`を連結する。`/v1/v1`や別Backend URLへ自動修正しない。Gatewayから各GPU hostのRuntimeへは既存認証付き経路を使う。

今回Caller許可にHarnessを追加する。CMD/PORTALが物理Backendへ接続してよいという変更ではない。旧clientは新contractを送らない限り既存動作を維持し、対応していないclient/経路を一律に停止しない。

## 2. 公開する能力とstrict要求

### 2.1 Capability

`GET /v1/status`の任意top-level field`contracts`へ実装済みcontractだけを載せる。

```json
{"contracts":{"harness-v1":{"normalization":"strict-v1","measure":true,"generation_retry":"disabled","attempt_receipt":true,"explicit_recovery":true}}}
```

この広告は入口の機能だけで、全bindingの適合証明ではない。bindingごとのmeasure応答、profile対応、実identityを検査する。欠落はUNSUPPORTED_CONTRACT。

### 2.2 一要求一生成

既存Chat要求の`rencrow.harness`に、contract_version、stage、expected_binding_fingerprint、expected_input_digest、expected_request_digest、max_backend_attempts=1、recoveryを載せる。

```json
{"contract_version":"harness-v1","stage":"act","expected_binding_fingerprint":"<measured>","expected_request_digest":"<measured>","expected_input_digest":"<computed-and-verified>","max_backend_attempts":1,"recovery":{"profile_id":"same_request","profile_revision":"builtin-v1","retry_of_request_id":null,"trigger_code":null}}
```

recoveryは初回にもsame_requestで明示する。retryではRETRY_CONTRACTに従いretry_of_request_id/trigger_codeを埋める。新しいActionIDを作って上限を迂回しない。

Gateway/Runtime/Backend adapterは一回のHTTP要求から追加生成を行わない。既存Responsesのno-thinking/plain-text自動再送をstrict要求へ流用しない。HTTP接続失敗も勝手に再生成しない。単なるTCPレベル再送は別である。

Runtimeは通常生成とmeasureに共通の前処理を使う。Model identity、binding revision、request digest、context limitを検証し、不一致なら生成開始前に拒否する。BINDING_CHANGEDでは新しいModelやtemplateを自動採用しない。停止して設定更新または新しい明示resumeで再評価する。

### 2.3 Attempt receipt

成功時は通常Chat responseへ`rencrow.harness_receipt`を加える。error envelopeと最終SSE eventにも、取得できた同じ情報を含める。詳細shapeは`schemas/llm_contract.schema.json`のGatewayAttemptReceipt。

必須: contract_version、stage、binding_fingerprint、input_digest、request_digest、logical_requests=1、backend_attempts(0/1/null)、generation_state(not_started/terminal/unknown)、hidden_retry=false、recovery_profile/revision、applied_transformations、usage_complete。

正常に生成完了ならbackend_attempts=1/generation_state=terminal、生成前の拒否なら0/not_started。通信切断等で開始・終了が分からなければnull/unknown。回数0とモデル完了を混ぜない。receipt欠落やhidden_retry=trueはMODEL_CONTRACT_FAILEDであり、成功として採用しない。

Harnessはこのwire receiptにcanonical Action/Attempt/Request/Evidenceを結合し、ModelAttemptReceiptへ保存する。下位層にHarnessのID生成責任を移さない。unknownのまま同じ生成を並列再送しない。

## 3. モデル固有回復profile

RETRY_CONTRACTが回数と選択を所有し、LLMが補正の内容を所有する。profile descriptorは固定id/revision、対応stage、対応code、許可変換、対象の基本profileとの関係を持ち、既存LLM profile設定/適合結果からRuntimeが提供する。measureで非対応profileを指定したらUNSUPPORTED_RECOVERY_PROFILE。

same_requestは形式補正なし。terminal_output_onceでは、可視の最終本文または宣言済みToolCallを要求する形式suffixと、基本profileが許可した場合のthinking無効化だけを行う。QwenのタグやフラグをHarnessに書かない。suffixの挿入位置はLLM ownerの正常化で決め、最後のUser指示や保持原文を移動・削除しない。

初回Qwen profileではterminal_output_onceを実装・受入する。基本のrole thinking policyは通常要求では維持し、回復変更は採用済みprofileの明示例外として追加する。profileにないtemperature変更、Model変更、Tool集合変更、要約指示の除去は拒否する。実際に適用した変換名をreceiptへ返す。

Selection/Summaryのprofileは初回要求の前に段専用設定として固定する。事後のretry用profileはact以外では受理しない。段失敗はEmergencyへ返す。

## 4. 最終入力の計数API

新設`POST /v1/context/measure`を既存Gatewayへ追加する。requestは`{contract_version:"harness-v1",request:<Chat request>,safety_margin_tokens:integer}`。計数対象Chatは回復profileも含む。expected_*は計数時には省略してよい。

responseのclosed fieldは既存`schemas/measure.schema.json`に合わせる: contract_version、state、prompt_lower/upper、effective_context_limit、reserved_output_tokens、safety_margin_tokens、input_digest、request_digest、binding_fingerprint、evidence_ref、reason。

prompt countはtemplate、tools、generation prefixを含む最終token列全体。exactはlower=upper、boundは実装根拠のあるlower<=actual<=upper。過去のtoken/byte比の最大値を数学的上界としない。cached tokensは総promptの内数であり、cache hitだからprompt数を減らさない。

input_digest/request_digest/binding_fingerprintの所有者、取得元、構成はBYTE_CONTRACTS§2〜3。Harnessはinput_digestのみ独立に計算し、変換後request_digestはRuntimeから受領・echoする。base bindingと回復要求の差を混同しない。

stateがverified_exact/verified_boundでも、countが境界を跨ぐ場合はexact再計数、不能ならBUDGET_UNVERIFIED。生成送信前だけでなくPreflight/candidate commit前も同じ根拠で検査する。手動/自動/Emergencyを通じてunknown計数で安全な容量内と偽装しない。

## 5. 初回Qwen構成と実装順序

対象repositoryは`RenCrow_Qwen38_Flash`。参照scriptはMac上のMLX実行バイナリとモデルpathを指定する。scriptの存在はengine sourceがrepository内にある証拠でも、同binaryが今稼働している証拠でもない。[L06]

ClaudeはWP00で起動script、実engine path/build、tokenizer/templateの取得元とhash、現在のRuntime identity、sourceへの到達性を確認する。既存作業機で再起動を勝手に行わず、同じsource/modelを使う隔離ポートの試験を優先する。

計数の実現方法は次の**優先順で判定して実装**する。利用者へ選択肢を丸投げしない。

1. 既存engine APIが生成と同じ完全Chat入力を非生成で計数できる場合、そのAPIへRuntime adapterを接続する。messagesの単純text tokenizeだけでは不合格。
2. 上記がない場合、実engineの生成前処理関数を共用して、同じloopback ingressへ非生成handlerを追加する。内部pathは`/rencrow/tokenize-chat`。前処理を別の手書きtemplateとして複製しない。
3. 両方がsource/権限不足で成立しない場合、具体的なsource不足をルミナへ返す。別Modelを勝手に初回対象へ置換しない。Harnessの独立kernel/保存/CORE供給物は進めてよい。

engine改変が不要な場合はその理由を証拠付きで記録する。MLX等の依存は外部compute profileだけへ閉じ、Goの標準Harness/CORE起動にPythonを必須化しない。実装物はLLM、profile、engineの各diffと版を一組で納品する。

## 6. 共通normalization

既存Responsesのraw記法検出は、責務が同じ共有normalizerへ抽出しChatとResponsesから使う。RuntimeがModel外装を認識し、Gatewayは共通結果を公開する。直接Backend targetを使う既存routeがある場合は、strict対象を必ず適合Runtime経路へ配置し、機能が存在するだけで通過していると仮定しない。

no-tools段はtools=[]、tool_choice=none、json_object、stream=false。構造化ToolCallや実行用raw記法は拒否。正しい引用内のXMLを無条件に削除しない。単一json fenceの外装除去だけは既存規約の範囲で許すが、括弧補完・field生成・意味修正はしない。

Actor base instructionは段専用要求へ入れない。model_instructionsは形式補正で、Codex人格や実行権限を注入しない。logical→Gateway→Runtime→実promptの各段をfixtureで観測し、保持原文の順序/出所を照合する。

## 7. 品質の受入

計数と生成で同じ入力を使った実証をWP03で取得する。fixtureには日本語、tools、長いTool結果、Unicode/改行、thinking通常/回復、no-tools、cache cold/hotを含める。fixture ID・入力hash・計数値・生成prompt usage・定義差の説明を保存する。差があればverified_exactを広告しない。

WP05はreasoning-only/raw markupの強制fixtureと実Qwenを分けて検査する。既存経路との比較は同じ課題、Model/Backend revision、入力条件を記録し、異なるハーネスの条件差を併記する。平均速度だけで不正な原文喪失やunknown再生成を相殺しない。Normal率だけでなく、その後の作業継続と未完了の保持を確認する。

[L01]: SOURCES.md#l01
[L02]: SOURCES.md#l02
[L04]: SOURCES.md#l04
[L05]: SOURCES.md#l05
[L06]: SOURCES.md#l06
[C14]: SOURCES.md#c14

## 8. 読取descriptor・wire・エラー

GET /v1/statusのcontracts.harness-v1にbindings配列を追加し、各要素はllm_contract.schemaのBindingDescriptor。configured selector、固定profile_revision、fingerprint、各stage_options、利用可能recovery_profilesを返す。readyでないbindingは配列へ適合済みとして入れず、従来status側にunavailable理由を残す。

stage_optionsはLLMの基本profileに保存されたmax_tokens/temperature/top_p/seed/stop。未設定のmax_tokensは既存target.max_output_tokensを継承し、正値がなければstage unavailable。temperature/top_p/seedは明示nullでengine既定値を選べるが、最終NormalizedRequestには解決済み値/overrideを含める。Harnessが4096やthinking値を内部で別定義しない。

ChatRequest/MeasureRequest/GenerationRequest/NormalizedRequest/StrictError/StreamTerminalの全fieldはllm_contract.schemaに追加した。stage/recoveryと測定を迂回する未定義fieldをProviderOptionsへ詰めない。物理targetのsingle_leading_instructionはMODEL_PROJECTION§2のprefix限定strict処理を行い、全履歴のsystem/developerを巻き取らない。

エラー正規化はERROR_MAPPING、SSEはSTREAM_CONTRACT。contract/stageさえparseできない不正JSONでは一般INVALID_REQUESTを返してよいが、便宜上stage=actなどを発明したstrict receiptを作らない。その応答をHarnessが再試行可能な生成前拒否と誤認しない。


RecoveryProfile descriptorのcodesはERROR_MAPPINGで正規化したcode集合。terminal_output_onceはREASONING_ONLY/EMPTY_FINAL_CONTENT/RAW_TOOL_MARKUP/MODEL_OUTPUT_SCHEMA_INVALIDの対応したものだけ、same_requestはその対応形式codeと許可された一時通信codeを列挙する。コードが集合にないprofileを暗黙に使わない。
