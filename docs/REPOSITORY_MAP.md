# RenCrowの概要とrepository読取地図

## 1. 初見の実装担当に必要なシステム理解

RenCrowは、人格を持つAgentによる会話、作業の割当、記憶・Recall、Tool、監査・UI表示をまとめたシステムである。Agent identityと実際に推論するModelは別物である。たとえばShiroはAgent、WorkerはExecution Role、QwenはModelであり、モデル交換は人格の交換ではない。[C12][L01]

現行の通常経路は次である。これはHarness導入前の説明である。

```text
CMD / PORTAL → CORE（Agent、意味ルーティング、Role、Persona、Memory、Tool）
                         ↓
               RenCrow LLM Gateway（alias / binding / queue）
                         ↓
               RenCrow LLM Runtime（Backend・Model対応、正規化）
                         ↓
               Backend（llama.cpp / MLX等）→ Model
```

`rencrow-llm-node`のnodeは既存artifact名でありNode.jsの意味ではない。GatewayとRuntimeはGoで、Model/engine/GPUは外部computeである。モデル別repositoryはexternal-runtime profileであり、もう一つのPublic LLM APIやAgent runtimeではない。[L01][L03]

COREの会話・長期Memoryと、Harnessの作業履歴は目的が違う。COREは人格の継続と上位Taskを管理する。Harnessは引き受けた作業を実行し、その事実・原本・再開状態を保存する。COREはHarnessの記録から利用者向け結果やMemory候補を採用するが、Harnessのtool履歴を別の正本として再発行しない。

## 2. 調査基準

| repository | 本版の読取基準commit | 本版での用途 |
|---|---|---|
| Nyukimin/RenCrow_CORE | `824723072e57119942318d5a8ff0acb65d2e7777` | 共有規約、既存Agent/実行/ID/store、v0.1照合報告 |
| Nyukimin/RenCrow_LLM | `d2166b4dffa9c21341dd2cace8c39cba690288c0` | 既存API・変換・Runtime境界 |
| Nyukimin/RenCrow_Switch_Core | `9331c99b1f6b33a3b6287253c89feb382e9d745e` | Compaction正本、Rust参考実装、履歴 |
| openai/codex | `c0c230e6730b3b3c9101b8aff4b9aea4027cea5b` | interfaceとsession、Tool、再開・取消の実装参考 |

COREはv0.1の基準`e2dd0b16…`から1commit進んでいるが、差分は照合報告の追加と公開doc allowlistだけで、既読実行sourceの変更はない。LLMの基準commitは同一だった。これを実機のbinary/config/DB一致の証明にはしない。[C14]

本版が参照するCompaction提供snapshotは6,003行の`COMPACTION_SPEC.md`、82行の`COMPACTION_HISTORY.md`、118行のレイヤー別復旧提案である。v0.2作成時の基準repositoryの§14〜18とも照合した。全Rust sourceと私有logの再検証を行ったとは主張しない。[D01][D02][D03][S01]

## 3. 読む順序と調べる対象

### CORE: 最初に読む

| path | 読み取ること | 本版の扱い |
|---|---|---|
| `README.md`、`docs/README.md` | 概要、正本一覧、No-Human-Gate、標準Go配布 | 現状の入口 |
| `docs/04_アーキテクチャ概要.md` | 標準Go配布、Prompt Context Assembly、CORE/LLM境界 | Harness独立呼出しを追加する共有仕様 |
| `docs/05_設定リファレンス.md` | 設定、storage追加判断、manifest、backup、OS配置 | Harness所有store追加の採用先 |
| `docs/architecture/identity/IDENTITY_CANONICAL.md` | ID名・prefix・issuer・Run更新境界 | owner拡張を明記する採用先 |
| `docs/RenCrow_Atlas_Backlog_Implementation_Lifecycle_仕様.md` | Task/採用/実装のライフサイクル | CORE親TaskとHarness子Taskの採用境界を照合 |
| `docs/調査/20261006_183358_RenCrow_Harness仕様v0.1_現行コード照合.md` | 経路ごとの差とv0.1の落し穴 | 調査報告。製品契約や試験成功の正本ではない |

### CORE: 機能抽出・接続時に読む

| path / symbol | 観測・注意 | 新Harnessへの接続 |
|---|---|---|
| `internal/application/toolloop/loop.go` / `Run` | LLM→tool反復。10stepと4,096出力既定、low推論。上限/一部blockedは文字列＋nil。ActionはtaskScoped時だけ | 意味をコピーせずtyped終端へ変更。既存Toolの副作用部品を調査 |
| `internal/application/subagent/manager.go` | toolloopの直接caller、SystemPrompt等を渡す | 既存経路全体を無条件置換しない。委譲の明示分岐を追加 |
| `internal/domain/agent/shiro.go` | CodexWorkPathがsubagentより先にreturn | 新profile選択時は旧Codex経路に入らないことを入口で確認 |
| `internal/domain/routing/codex_work_path.go` | keywordに基づく旧経路判定 | mode移行と意味routeを混同しない |
| `internal/domain/agent/prompt_context.go` | 5区分組立。`renderSystemMessages`はsystem本文を一文字列へ潰す | typed blocksを境界で保持。flattenした文字列から由来を復元しない |
| `internal/domain/llm/provider.go` / `modules/llm/contracts.go` | 内部/公開型に差。ChatにToolChoice/ResponseFormatなし | HarnessはCORE Providerを通さずGatewayへ直接接続する。CORE既存経路の改修範囲は別 |
| `internal/infrastructure/llm/providers/rencrowllm/provider.go` | 現行COREはChat Completions経路 | Responses側だけの保護を利用できると仮定しない |
| `internal/application/actionmanager/manager.go` | 現行Action/Attempt発行と保存。ToolIntent冪等結合は未実装 | 検証・保存の考えを再利用。CORE内subtool発行をHarness委譲へ重ねない |
| `internal/infrastructure/tools/` | command、file、subagent、codex.run等 | deterministicなファイル/実行部品だけを抽出候補にする。Toolが別Agent loopを起動する場合は別capability |
| `cmd/rencrow/runtime_tool_runtime.go` / `internal/infrastructure/persistence/toolharness` | 既存`ToolHarness`がある | 新module名と混同しない。CORE client名は`nativeharnessclient` |
| `config/durable-stores.json` | 記録された2store。全storeの網羅を意味しない | CORE DBを別processからwriteしない |

上記の未掲載細部はC14のsource照合報告にもpathと行がある。特にActionなしnested tool経路をnative Harnessへ持ち込まない。

### LLM: 初回に必要な変更を実装するために読む

1. `docs/README.md`、`docs/06_Public_API仕様.md`、`docs/10_Gateway_Runtime_Backend_Model責務仕様.md`、`docs/13_モデル直指定経路仕様.md`。
2. `gateway/internal/httpapi/handler.go`の`chatCompletion`、`completeNetwork`、`openAIExtension`、Responses stream retry。
3. `gateway/internal/adapter/responses/raw_tool_markup.go`とconverter、`gateway/internal/messagewire/`。
4. `gateway/internal/nodehttp/handler.go`と`gateway/internal/nodeconfig/`、Backend adapter契約。
5. 選んだModel profileの起動・template・engine revisionを調査する。Qwenは`RenCrow_Qwen38_Flash`、RX6800の別profileは`RenCrow_Qwen38_27B_RX6800`。名称を同一と扱わない。

確認済みの重要差: raw tool記法の検出/再送はResponses経路で、COREが使うChatに同等処理がない。json_object+stream拒否はRuntime。tokenizer APIは未実装、catalogはdeclaredであり実測ではない。本版はこれらをLLM_INTEGRATIONの新契約で解決する工程を必須にする。[L02][L04][C14]

### Switch: Compactionを実装する際に読む

`COMPACTION_SPEC.md`第1〜2部を優先し、第3部は日付付きの実装/配備状況として読む。`COMPACTION_HISTORY.md`の26項目は本パッケージのH01〜H26に対応する。原本のprivate logはrepositoryから取得できるとは限らない。

Rust側では正本が示す`codex-rs/core/src/compact_rencrow.rs`、`commit_rencrow_checkpoint`、canonical loader、Observation projection、`codex-rollout`、`rencrow-compaction`を調べる。pathが変わった場合は`git grep -n`でsymbolを探し、読取commitを記録する。GoへRustファイルをそのままコピーする指示ではない。

### Codex公式: 参考設計として読む

`codex-rs/app-server/`、`app-server-protocol/`、`core/src/session/`、`exec/`、`tui/`、`protocol/`、tool実行/保存関連crateを、役割→公開型→呼出し→試験の順に追う。実在確認した入口はapp-server READMEとsession directory。個々のsymbol/fileの位置は基準commitで確認する。

公式App ServerのThread/Turn/Item、イベント、steer/interrupt、execのJSON出力を参考にする。ただしnative RPCはCodex互換APIではない。CodexのOAuth、cloud、モデルcatalog、opaque compaction、approval待ち、独自code modeをそのまま採用しない。[O01][O02]

## 4. 言明を取り違えやすい箇所

- Kuro/HeavyのCORE実装はGenerateを使用し、Kuro自身にCodex分岐はない。Gateway bindingの先がCodex Agent Runtimeになることとは別の話である。現行live bindingは本版では未確認。
- Qwen profileのsourceがあることと、EcoSystem登録・現在ready・同commit配備は別である。
- 元論文の七要素は分類法。各機能を全て最大規模で実装する要求ではない。本文の多モデル最適化は専用層を置く参考であり、実モデルの品質保証ではない。
- 「通常経路はCOREからだけ」という現行LLM規約は、standalone Harness導入時に正式に変更する。既存規約を見落として迂回するのではなく、CallerとしてHarnessを追加する。

## 5. 設計差分を反映する正本

CORE 01概要、02機能、03Agent、04architecture、05config/store、06API、07safety、08roadmap、09運用、10log、Atlas、Identityを差分レビュー対象にする。全12ファイルを無意味に変更する要求ではない。該当章へowner/caller/Task/Run/receipt境界を追記し、非該当は差分台帳へ理由を記す。

LLMはPublic API、owner規約、Backend適合試験、node設定を更新。EcoSystemはplanned moduleを登録し、artifactが未生成ならrelease済みとしない。profileはBackend tokenization補助の配備sourceを持つがPublic APIを増やさない。Switchの現行threadは変更せず、移行exportだけを別受入する。


[C12]: SOURCES.md#c12
[C14]: SOURCES.md#c14
[D01]: SOURCES.md#d01
[D02]: SOURCES.md#d02
[D03]: SOURCES.md#d03
[L01]: SOURCES.md#l01
[L02]: SOURCES.md#l02
[L03]: SOURCES.md#l03
[L04]: SOURCES.md#l04
[O01]: SOURCES.md#o01
[O02]: SOURCES.md#o02
[S01]: SOURCES.md#s01

## 6. v0.2.2の追加作業とsourceの区分

本書に残る4repositoryの参照pinは、v0.2作成時に観測した設計基準である。本版で全mainを再調査したという意味ではない。初回対象profileはRenCrow_Qwen38_Flash、参照headは22bb43498d46174516e378261a8f6fbdb7da100a。scripts/env.shはengine binary/modelのhost pathを示すが、engine sourceの可用性を保証しない。[L06]

EcoSystemは56c1f6078210c0cf9c3b4792a98491b3bc336fccのecosystem.yamlでSwitch_Coreを16ea1009da01f04354cdec329fe4b89dd3df7f61へpinしており、設計参照9331c99bとは別である。F36は指定値と稼働値を別に記録する。これを理由に旧Switchを更新しない。[E01]

Claudeが追加するproducerは、CORE nativeharnessclientのContext revision builder、authenticated Human intakeからHMACを発行するissuer、Start/Resumeの完全limits解決。LLM側はstrict共通normalization、単一生成receipt、explicit recovery profile、最終Chat計数。全てWORK_ORDERの作業に含め、consumerだけ実装して完了としない。

[L06]: SOURCES.md#l06
[E01]: SOURCES.md#e01
