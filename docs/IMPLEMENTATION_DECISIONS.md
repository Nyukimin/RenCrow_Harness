# Claudeへの実装前最終回答

RH-FINAL-DECISIONS-001 / v0.2.2 / 2026-10-07 JST
設計責任者: ルミナ。実装担当: Claude。ユーザーからの17点をQ01〜Q17に対応付けた。以下を実装指示として採用する。実機成功・実装済みの宣言ではない。

## 決定一覧

| 照会 | 決定 | 正本/資産 |
|---|---|---|
| Q01 request_digest | 論理input_digestと最終request_digestを分離。共通CJ1+LP式とfield集合を固定。Harnessは最終変換を再実装せずmeasure値をecho | BYTE_CONTRACTS§1〜2、llm schema、wire vectors |
| Q02 binding_fingerprint | 外部ではLLM所有opaque。変化条件、取得元、LLM内部の推奨生成形を固定 | BYTE_CONTRACTS§3 |
| Q03 errors | codeとgeneration stateを独立判定。first-output timeoutは停止確認なしならunknown、DEGENERATEは自動retryしない | ERROR_MAPPING、error_cases |
| Q04 mutation hash | 主体/method/全params（最上位keyのみ除外）のCJ1+LP式。CORE/Host共通vector | BYTE_CONTRACTS§4 |
| Q05 role | Character=system、Stable=developer、Recall/Variable=user data、Human=user exact。leadingだけcoalesce | MODEL_PROJECTION§1〜2 |
| Q06 live Context | retained chronology、boundary、source解決済みSummary、重要reference、未要約tailの一意の投影 | MODEL_PROJECTION§3〜4、projection golden |
| Q07 stage data | root/要素closed schema、8,000 UTF-8 byte分割、一段一要求、Host manifest対応 | STAGE_DATA、stage_data schema |
| Q08 Emergency | 古い適格Observationから全件ref-onlyへ。byte prefilter後に全promptのverified token/no-fit/shrink判定 | CHECKPOINT_FORMAT§3 |
| Q09 Policy | workspace外の明示JSON registry、closed schema、policy_ref完全一致 | HOST_ASSETS§1、policy schema |
| Q10 extensions | 限定root-to-cwd AGENTS、限定SKILL rootとsubset metadata、Evidence明示load、7つの固定callback | HOST_ASSETS§2〜4 |
| Q11 candidate_bytes | version付きprefix+CJ1、hash自身を含めない、DBとpayloadの採用前後revisionを区別 | BYTE_CONTRACTS§6、CHECKPOINT_FORMAT |
| Q12 Event | 13既存+2retryの15payloadを固定。公開Event.payloadとDB payload_json同一 | EVENT_CONTRACT、event schema |
| Q13 key/caller | hex64+任意LFの32byte key、principal grammar、secretを含まないcaller digest式 | BYTE_CONTRACTS§5 |
| Q14 module/manifest | github.com/Nyukimin/RenCrow_Harness、binary/Go/stdio、EcoSystem entry template | 本書§2 |
| Q15 WP08 route | 明示選択されたShiro native coding profileをExecute入口で処理し、旧3分岐へ入れない | 本書§3 |
| Q16 act stream | 初回からtrue。Tool args集約・terminal receipt・DONE確認後だけ採用 | STREAM_CONTRACT§1〜3 |
| Q17 measure | 各Attemptのmeasure→generateという2HTTPを許容。非生成計数、性能計測を分離 | STREAM_CONTRACT§4 |

## 2. moduleとEcoSystemの固定値

Go moduleは`github.com/Nyukimin/RenCrow_Harness`。main packageは`github.com/Nyukimin/RenCrow_Harness/cmd/rencrow-harness`、binary名はrencrow-harness（Windowsでは.exe）。Go 1.25.0互換を維持。pkg/clientはnative protocol clientだけを公開し、CORE内部をimportしない。

EcoSystemのcomponents.harnessはrepository=Nyukimin/RenCrow_Harness、workspace_path=./RenCrow_Harness、required=false、distribution=binary。runtime.primary={implementation:"go",artifact:"rencrow-harness",status:"development"}、companions=[]。LLM依存は利用機能の契約として記録し、COREをrequired companionにしない。初回はnetwork listener/user_systemd不要、stdioで起動する。

deployment.go_binariesは上記module/main_package/installed_path=%h/.local/bin/rencrow-harness/version=実build commitを一件持つ。Windowsの実installed artifact pathは.exe付きで別OSのdeployment reportに記録する。templates/ecosystem_harness_entry.jsonは**未配備template**。`${HARNESS_COMMIT}`を実測40桁source SHAへ置換し、既存EcoSystem schemaで検査してからpinする。偽のSHA、未buildをrelease済みとして登録しない。ソース採用前の計画登録と、実binary配備を区別する。

## 3. WP08の対象route（設計確定）

初回の対象は**Shiroの明示execution profile `shiro_native_coding_v1`**。LLM modelのprofile名とは別のCORE実行backend選択である。COREのadmission/orchestratorが認証済み設定からこのprofileを選び、許可workspace、既存Shiro用Gateway binding、limitsを付ける。本文のkeywordやModelの出力からこのprofileを選ばない。

`ShiroAgent.Execute`の**tryExecuteCodexWorkPathより前**に、型付きの受理済みbackend selectionを確認する分岐を追加する。[C15] 選ばれていればnativeharnessclientへ1回だけ委譲し、typed resultを返す。選択済みRunは、成功/失敗を問わず、CodexWorkPath、SubagentManager/toolloop、plain Generateのいずれにも入らない。

したがって「旧CodexWorkPathだけを置換」でも「toolloopだけを置換」でもない。**選択済みprofileのShiro実行全体を、旧内部分岐の前で切り替える**。未選択の既存Shiro/他Agent/Advisor/codex.run routeは維持し、失敗時fallbackとして使わない。

切替対象scopeは、新規のcoding/workspace作業としてCOREが当該profileに明示admitしたもの。既存進行中Run、全Shiro会話、すべてのAdvisor呼出しを一括置換しない。workspaceまたは必要Tool capabilityを供給できない場合、admissionを明示rejected/blockedにし旧経路へ戻さない。複数候補からどのrouteを選ぶかをClaudeへ残さない。

CORE新設定の論理項目は{profile_id:"shiro_native_coding_v1",agent_id:"shiro",backend:"native_harness",harness_binary,harness_config,workspace_ref,binding_ref,limits}。既存configの拡張場所、struct/helper名はClaudeが選べるが、このfieldの意味・選択時点・early returnは変えない。harness_configのcaller.principal=core:local、origin=automation、Human relayは許可issuerからだけ。

必要試験: keywordが旧CodexWorkPathへ一致する入力と一致しない入力の両方、subagent設定あり/なしの両方、native client失敗、取消/再開。全ケースで対象profileが旧分岐/Actionlessを通らず、同じHarnessをCOREなしCLIでも使えることをTraceで示す。非対象profileの旧経路回帰は別試験。

## 4. 裁量と作業開始

Claudeはprivate helper名、Go packageの細分化、DB query最適化、fixture runner実装を選べる。byte合同、wire field、Modelへ提示する内容、権限/保持/計数/復旧規則は本版で固定した。既存実装から契約違反や成立不能の反証が出た場合だけ、AD/Q番号、source、再現、影響、最小修正案を設計者へ返す。適用できる独立作業は進める。

WP01/02/03/04/05は本版の共有vectorとschemaから着手できる。これは実機source/権限があることを保証するものではない。Claudeの実装検査で新しい反証が出たら、過去validator合格で押し切らない。

[C15]: SOURCES.md#c15

## 5. COREの返却型への注意

現行ShiroAgent.Executeのstring/errorだけで新RunResultを丸めない。nativeharnessclientが得るRunResultをCOREの委譲結果projectionへ保持し、legacyのstring/errorへ表示変換する場合もcompleted以外をnil errorによる成功にしない。実装内部の関数signature変更とcaller更新はClaudeの作業であり、既存のpublic APIに新しい型が既にあるとは扱わない。
