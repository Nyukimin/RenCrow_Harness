# 根拠と参照範囲

設計本文の[Cxx]/[Lxx]/[Dxx]/[Sxx]/[Oxx]/[Pxx]/[Uxx]は以下を参照する。runtimeの現在値は資料と同一とは限らない。全文copyではなく、固定path・読取範囲・限界を示す。

<a id="c01"></a>
## C01

`Nyukimin/RenCrow_CORE` / `docs/04_アーキテクチャ概要.md`
読取ref: `824723072e57119942318d5a8ff0acb65d2e7777` / blob: `7f1dfee2fcaea6655aa60a140077729249445d9a`

[参照先](https://github.com/Nyukimin/RenCrow_CORE/blob/824723072e57119942318d5a8ff0acb65d2e7777/docs/04_%E3%82%A2%E3%83%BC%E3%82%AD%E3%83%86%E3%82%AF%E3%83%81%E3%83%A3%E6%A6%82%E8%A6%81.md)

読取範囲: 標準Go配布境界119〜172行を再取得。会話内では冒頭の責務境界等も読取。
支持する内容: native Go中心、三OS共通契約、外部compute隔離、例外契約。
限界: source読取。実機の配備・設定一致・動作は未検証。

<a id="c02"></a>
## C02

`Nyukimin/RenCrow_CORE` / `docs/README.md`
読取ref: `824723072e57119942318d5a8ff0acb65d2e7777` / blob: `cf680c902dea96440b5c34839a4f0e5eeec7071e`

[参照先](https://github.com/Nyukimin/RenCrow_CORE/blob/824723072e57119942318d5a8ff0acb65d2e7777/docs/README.md)

読取範囲: 全文
支持する内容: COREの現行正本優先順位、No-Human-Gate、Prompt Context Assembly、Common Raw Data、store規約の所在。
限界: source読取。実機の配備・設定一致・動作は未検証。

<a id="c03"></a>
## C03

`Nyukimin/RenCrow_CORE` / `internal/application/toolloop/loop.go`
読取ref: `824723072e57119942318d5a8ff0acb65d2e7777` / blob: `ebc68b354483383fee0248e6b1708ed5cf1e71e4`

[参照先](https://github.com/Nyukimin/RenCrow_CORE/blob/824723072e57119942318d5a8ff0acb65d2e7777/internal/application/toolloop/loop.go)

読取範囲: 全文
支持する内容: 既存LLM/Tool反復、取消、10step既定、Action接続、反復上限と一部blockedの本文＋nil返却。
限界: 確認した関数の挙動。実際にどのproduction routeから到達するかは全callsite未検査。

<a id="c04"></a>
## C04

`Nyukimin/RenCrow_CORE` / `internal/application/actionmanager/manager.go`
読取ref: `824723072e57119942318d5a8ff0acb65d2e7777` / blob: `3d266d55e2f9277f949b76a91421364a0003eec0`

[参照先](https://github.com/Nyukimin/RenCrow_CORE/blob/824723072e57119942318d5a8ff0acb65d2e7777/internal/application/actionmanager/manager.go)

読取範囲: 17〜117行を再取得。会話内では1〜220行も読取。
支持する内容: ActionID/AttemptID唯一の発行主体、CreateActionのtransaction。
限界: ToolIntentとの冪等結合やcheckpointとのcross-store atomicityの既存実装は確認していない。

<a id="c05"></a>
## C05

`Nyukimin/RenCrow_CORE` / `internal/domain/agent/prompt_context.go`
読取ref: `824723072e57119942318d5a8ff0acb65d2e7777` / blob: `5bed6bdb486ab0fcef1e3b1cd4ba00305dfbdb92`

[参照先](https://github.com/Nyukimin/RenCrow_CORE/blob/824723072e57119942318d5a8ff0acb65d2e7777/internal/domain/agent/prompt_context.go)

読取範囲: 98行以降の組立・hashを再取得。会話内では全文を読取。
支持する内容: 5区分の組立順、static hash、context provider。
限界: source読取。実機の配備・設定一致・動作は未検証。

<a id="c06"></a>
## C06

`Nyukimin/RenCrow_CORE` / `internal/domain/llm/provider.go`
読取ref: `824723072e57119942318d5a8ff0acb65d2e7777` / blob: `980090290acefffc7c346df38de1230787b2aab7`

[参照先](https://github.com/Nyukimin/RenCrow_CORE/blob/824723072e57119942318d5a8ff0acb65d2e7777/internal/domain/llm/provider.go)

読取範囲: 全文
支持する内容: 内部Generate/Chat、ToolCalling、PromptContextType、ResponseFormatの現在field。
限界: source読取。実機の配備・設定一致・動作は未検証。

<a id="c07"></a>
## C07

`Nyukimin/RenCrow_CORE` / `modules/llm/contracts.go`
読取ref: `824723072e57119942318d5a8ff0acb65d2e7777` / blob: `a7d06c5d9acac10c4d5ba45f9393d62d32f1c59e`

[参照先](https://github.com/Nyukimin/RenCrow_CORE/blob/824723072e57119942318d5a8ff0acb65d2e7777/modules/llm/contracts.go)

読取範囲: 全文
支持する内容: 公開Generate/Provider契約。内部型とのfield差。
限界: source読取。実機の配備・設定一致・動作は未検証。

<a id="c08"></a>
## C08

`Nyukimin/RenCrow_CORE` / `internal/application/subagent/manager.go`
読取ref: `824723072e57119942318d5a8ff0acb65d2e7777` / blob: `f5a2480091b9d743465b032878f500f1703b984a`

[参照先](https://github.com/Nyukimin/RenCrow_CORE/blob/824723072e57119942318d5a8ff0acb65d2e7777/internal/application/subagent/manager.go)

読取範囲: この会話内で1〜180行を読取。ConversationEngineのengine.goも読取済み。
支持する内容: SubagentManagerからtoolloopへの接続、Task/Run context、Tool registry/記録の依存。
限界: 全caller・全Agent経路は未確認。ConversationEngineとCompaction同等性を主張しない。

<a id="c09"></a>
## C09

`Nyukimin/RenCrow_CORE` / `docs/architecture/identity/IDENTITY_CANONICAL.md`
読取ref: `824723072e57119942318d5a8ff0acb65d2e7777` / blob: `0e7893c715e1385d523296f5b5b380346bc607d8`

[参照先](https://github.com/Nyukimin/RenCrow_CORE/blob/824723072e57119942318d5a8ff0acb65d2e7777/docs/architecture/identity/IDENTITY_CANONICAL.md)

読取範囲: 本版で固定commitの1〜460行を読取。Run更新条件とprefix/UUID規約も確認。
支持する内容: 一つの意味に一つのID、owner、状態/順序とIDの区別、恒久alias/dual-write禁止。
限界: mainリンクは可変。読取blobを記録する。全IDの後半詳細規則は本書で再定義せずowner正本へ委譲。

<a id="c10"></a>
## C10

`Nyukimin/RenCrow_CORE` / `config/durable-stores.json`
読取ref: `824723072e57119942318d5a8ff0acb65d2e7777` / blob: `536ec841403de2713f9f00661db9fedd34bd6d8d`

[参照先](https://github.com/Nyukimin/RenCrow_CORE/blob/824723072e57119942318d5a8ff0acb65d2e7777/config/durable-stores.json)

読取範囲: 全文
支持する内容: core.conversation_l1の既存store、CORE writer、private、fail_closed、backup/restore。
限界: manifest内の2 storeを確認。全CORE stateのstore一覧とみなさず、Harness保存への適合は要審査。

<a id="c11"></a>
## C11

`Nyukimin/RenCrow_CORE` / `go.mod`
読取ref: `824723072e57119942318d5a8ff0acb65d2e7777` / blob: `f08bcbe1efdde9852155eacd22ad9577cb361976`

[参照先](https://github.com/Nyukimin/RenCrow_CORE/blob/824723072e57119942318d5a8ff0acb65d2e7777/go.mod)

読取範囲: 本版で固定commitの1〜70行を読取。採用libraryも確認。
支持する内容: CORE基準はGo 1.25.0。
限界: 依存libraryの採否・最新性を一般推薦したものではない。Harnessのビルドは未実施。

<a id="c12"></a>
## C12

`Nyukimin/RenCrow_CORE` / `README.md`
読取ref: `824723072e57119942318d5a8ff0acb65d2e7777` / blob: `445ac6fba8d904ce4ad6537b1a8781878d426873`

[参照先](https://github.com/Nyukimin/RenCrow_CORE/blob/824723072e57119942318d5a8ff0acb65d2e7777/README.md)

読取範囲: この会話内のmain snapshot全文。
支持する内容: Agent/Role/Memory/Tool所有、production data root、Storage Proposal、No-Human-Gate。
限界: readmeの記載であり現行hostの照合ではない。mainリンクは可変でblob SHAを併記。

<a id="l01"></a>
## L01

`Nyukimin/RenCrow_LLM` / `docs/10_Gateway_Runtime_Backend_Model責務仕様.md`
読取ref: `d2166b4dffa9c21341dd2cace8c39cba690288c0` / blob: `3e41db0a78cb053c3169654981dccf8ad27cda82`

[参照先](https://github.com/Nyukimin/RenCrow_LLM/blob/d2166b4dffa9c21341dd2cace8c39cba690288c0/docs/10_Gateway_Runtime_Backend_Model%E8%B2%AC%E5%8B%99%E4%BB%95%E6%A7%98.md)

読取範囲: 1〜155行
支持する内容: Gateway/Runtime/Backend/Model/profileのowner分担、正規化、json_object non-stream、Tool境界、ContextLimit。
限界: source読取。実機の配備・設定一致・動作は未検証。

<a id="l02"></a>
## L02

`Nyukimin/RenCrow_LLM` / `docs/06_Public_API仕様.md`
読取ref: `d2166b4dffa9c21341dd2cace8c39cba690288c0` / blob: `ecd1877313238515692060f9aabb1a2cd4f0f770`

[参照先](https://github.com/Nyukimin/RenCrow_LLM/blob/d2166b4dffa9c21341dd2cace8c39cba690288c0/docs/06_Public_API%E4%BB%95%E6%A7%98.md)

読取範囲: 本版で固定commitの1〜120行を取得。その他は既読snapshotとC14に基づく。
支持する内容: 既存/v1/chat/completions、/v1/responses、alias/model routeとmetadata、本文/Tool変換の範囲。
限界: Public APIのsource。新harness contract/measureは未実装。実機検証なし。

<a id="l03"></a>
## L03

`Nyukimin/RenCrow_Qwen38_Flash` / `README.md`
読取ref: `source snapshot identified by blob` / blob: `8dbe97d64f298d9e7444a92c6546195a97e3d056`

[参照先](https://github.com/Nyukimin/RenCrow_Qwen38_Flash/blob/main/README.md)

読取範囲: この会話内のmain snapshot全文。
支持する内容: repositoryはexternal-runtime profile。起動/health/host tuning、共通正規化ownerはRenCrow_LLM。
限界: モデル名は資料表記。現行実機readyや同commitの配備成功を確認したとは扱わない。

<a id="d01"></a>
## D01

提供資料: `COMPACTION_SPEC.md` / SHA-256: `de248496f845db79221e59c12d228f60159347d1df4b44d1f11b4195fd4fce8c`

読取範囲: 6,003行。上位/実装/current-stateの該当範囲と、2026-09のFailure Knowledgeを読取。
支持する内容: 既存Compactionの優先順位、原本保全、Normal/Emergency、semantic/durable boundary、受入範囲。
限界: 文書の記録が根拠。参照する私有raw log・実行receiptの再実行/実機検査はしていない。

<a id="d02"></a>
## D02

提供資料: `COMPACTION_HISTORY.md` / SHA-256: `5d609f339e221d1b46102f5739b51d61668c0166ce622d70d87528d4ec2362ac`

読取範囲: 全文82行。26行の時系列tableを機械照合。
支持する内容: 不具合と対策26項目、確認できた到達点と残件。
限界: 文書の記録が根拠。参照する私有raw log・実行receiptの再実行/実機検査はしていない。

<a id="d03"></a>
## D03

提供資料: `20261005_143617_Compactionレイヤー別復旧_提案.md` / SHA-256: `5bc212dfe1d1294cf5aaf932f022ce717ce0c545658a2a53088c6cde43c71b49`

読取範囲: 全文118行。
支持する内容: レイヤー別復旧は提案。計測以外の共通復旧全体は未採用。
限界: 文書の記録が根拠。参照する私有raw log・実行receiptの再実行/実機検査はしていない。

<a id="c13"></a>
## C13

`Nyukimin/RenCrow_CORE` / `internal/domain/agent/shiro.go`
読取ref: `824723072e57119942318d5a8ff0acb65d2e7777` / blob: `d8ed57e453021a758b32e18f0fc5d2a6b532b797`

[参照先](https://github.com/Nyukimin/RenCrow_CORE/blob/824723072e57119942318d5a8ff0acb65d2e7777/internal/domain/agent/shiro.go)

読取範囲: 88〜134行
支持する内容: ShiroのCodexWorkPath早期returnがSubagent/toolloopより前にある。
限界: 関数の分岐を確認。稼働hostでどの分岐を利用中かは未検証。

<a id="c14"></a>
## C14

`Nyukimin/RenCrow_CORE` / `docs/調査/20261006_183358_RenCrow_Harness仕様v0.1_現行コード照合.md`
読取ref: `824723072e57119942318d5a8ff0acb65d2e7777` / blob: `043c46385e3691393636786ed41f049afec6bf54`

[参照先](https://github.com/Nyukimin/RenCrow_CORE/blob/824723072e57119942318d5a8ff0acb65d2e7777/docs/%E8%AA%BF%E6%9F%BB/20261006_183358_RenCrow_Harness%E4%BB%95%E6%A7%98v0.1_%E7%8F%BE%E8%A1%8C%E3%82%B3%E3%83%BC%E3%83%89%E7%85%A7%E5%90%88.md)

読取範囲: 全文87行
支持する内容: v0.1の現行code照合、9補正点、報告時のsource位置
限界: 報告自身はread-only、Switch/runtime未検証。報告をE2E成功に昇格しない。

<a id="l04"></a>
## L04

`Nyukimin/RenCrow_LLM` / `gateway/internal/httpapi/handler.go`
読取ref: `d2166b4dffa9c21341dd2cace8c39cba690288c0` / blob: `053ce3ac83c436aaaa8765fd03adce80a328b975`

[参照先](https://github.com/Nyukimin/RenCrow_LLM/blob/d2166b4dffa9c21341dd2cace8c39cba690288c0/gateway/internal/httpapi/handler.go)

読取範囲: 会話内で1〜690、800〜1135、1190〜1450の該当範囲を読取
支持する内容: Responses専用再送とChat completeNetworkの差、empty/tool検査、現行route
限界: C14のsource照合とも一致。node内部の全normalizationを単独全文検査したという意味ではない。

<a id="s01"></a>
## S01

`Nyukimin/RenCrow_Switch_Core` / `COMPACTION_SPEC.md`
読取ref: `9331c99b1f6b33a3b6287253c89feb382e9d745e` / blob: `b3126772bf3b2e680742448ccaafc41e73f58ea7`

[参照先](https://github.com/Nyukimin/RenCrow_Switch_Core/blob/9331c99b1f6b33a3b6287253c89feb382e9d745e/COMPACTION_SPEC.md)

読取範囲: 1460〜1558行、§14〜18
支持する内容: Completion link、Preflight禁止遷移、Summary必須指示を最新sourceでも照合
限界: 提供D01全体と最新commitの全差分一致を主張しない。

<a id="o00"></a>
## O00

`openai/codex` / `codex-rs/app-server/README.md`
読取ref: `c0c230e6730b3b3c9101b8aff4b9aea4027cea5b` / blob: `a40c0c9d2581c517010fefbd11a9ae6c3a5f70f0`

[参照先](https://github.com/openai/codex/blob/c0c230e6730b3b3c9101b8aff4b9aea4027cea5b/codex-rs/app-server/README.md)

読取範囲: 250〜400行のstored attachment/session関連、同pinのcore/src/session directoryも確認
支持する内容: 実装参考の固定source入口。個別機能の説明はO01/O02を併用
限界: 全Codex sourceを読了したとは主張しない。次のLLMが担当機能のsourceとtestを追う。

<a id="o01"></a>
## O01

[参照先](https://developers.openai.com/codex/app-server)

読取範囲: 公式公開docs、thread/turn/steer/interrupt/historyの該当節
支持する内容: 人/プログラム利用、typed操作、session継続APIの参考
限界: 可変web資料。native Harnessとのwire互換性を意味しない。

<a id="o02"></a>
## O02

[参照先](https://developers.openai.com/codex/noninteractive)

読取範囲: 非対話、JSONL出力の節
支持する内容: 人向け表示とプログラム向けevent出力の参考
限界: Harnessの新CLIは別実装。

<a id="o03"></a>
## O03

[参照先](https://www.jsonrpc.org/specification)

読取範囲: JSON-RPC2.0のrequest/response/notification/error
支持する内容: native stdioのメッセージ意味。NDJSON framingは本Harnessの選択
限界: JSON-RPC適合はACP/MCP/Codex互換の証明ではない。

<a id="p01"></a>
## P01

提供資料: `2609.00006v1.pdf` / SHA-256: `e81b5a4855adda8cc5e46db0a05cd69e0d0d542bd49cec6b157c17e6001395d7`

読取範囲: 提供本文。特に§2.3 p4図1/p5、§6/7/9/13、§15.6、§16 p67〜72
支持する内容: 7subsystem、Interface/Session substrate、中央provider層、18推奨、限界
限界: 論文のsource比較でありruntime統制実験ではない。市場動向/非公開source/自報告速度を本設計の実証根拠にしない。

<a id="u01"></a>
## U01

読取範囲: 利用者の本会話の明示要求
支持する内容: COREも利用し、Ren自身もToolとして初回から利用。repository初見のLLMが読める仕様/実装仕様。
限界: この利用者要求自体は言語/protocol/storageの方式を指定しない。方式は現版のARCHITECTURE_DECISIONSでルミナが決定する。

## v0.2.2追加読取と要求

<a id="l05"></a>
### L05

参照: [Nyukimin/RenCrow_LLM / gateway/internal/httpapi/handler.go](https://github.com/Nyukimin/RenCrow_LLM/blob/d2166b4dffa9c21341dd2cace8c39cba690288c0/gateway/internal/httpapi/handler.go)。blob `053ce3ac83c436aaaa8765fd03adce80a328b975`。

範囲: 475-545: Responses stream raw markup/reasoning-only retry。支持する内容: 基準sourceの自動再送の存在。strictの明示回復は今回設計。限界: 現行実機の配備/改善率を証明しない。

<a id="l06"></a>
### L06

参照: [Nyukimin/RenCrow_Qwen38_Flash / scripts/env.sh](https://github.com/Nyukimin/RenCrow_Qwen38_Flash/blob/22bb43498d46174516e378261a8f6fbdb7da100a/scripts/env.sh)。blob `4a88e7b421372e17dc05188ee589aa554db77c32`。

範囲: mainのscripts/env.sh全文とhead観測。支持する内容: MLX実行binary/modelのhost path、設定sourceの所在。限界: engine source可用性・実機稼働・性能評価は今回未確認。

<a id="e01"></a>
### E01

参照: [Nyukimin/RenCrow_EcoSystem / ecosystem.yaml](https://github.com/Nyukimin/RenCrow_EcoSystem/blob/56c1f6078210c0cf9c3b4792a98491b3bc336fcc/ecosystem.yaml)。blob `b7d28fbc5b79bb2b5f9832c7700db6e670347ceb`。

範囲: 640-735 switch_core指定、830-末尾 runtime_policy。支持する内容: Switch pin=16ea1009da01f04354cdec329fe4b89dd3df7f61、標準Go/三OS。限界: 配布manifestの指定であり実機source/binary一致の証明ではない。

<a id="s02"></a>
### S02

参照: [Nyukimin/RenCrow_Switch_Core / COMPACTION_SPEC.md](https://github.com/Nyukimin/RenCrow_Switch_Core/blob/9331c99b1f6b33a3b6287253c89feb382e9d745e/COMPACTION_SPEC.md)。blob `b3126772bf3b2e680742448ccaafc41e73f58ea7`。

範囲: 3664-3698: 付属A F12 build_completion_links。支持する内容: call/output各<=2048の明文。linkは成功証明ではない。限界: 今回Rust実装の実行はしていない。

<a id="u02"></a>
### U02

範囲: 最新の依頼: ルミナが主、Claudeが作業をする人。支持する内容: 設計決定はルミナ、Claudeは調査/実装/検証。役割を文書に固定。限界: 本番の包括的変更許可や試験成功は含まない。

## C15

RenCrow_CORE `824723072e57119942318d5a8ff0acb65d2e7777`、`internal/domain/agent/shiro.go` lines 70–225、blob `d8ed57e453021a758b32e18f0fc5d2a6b532b797`。2026-10-07読取。CodexWorkPath早期return→Subagent→Generateのsource配置を確認。稼働版確認ではない。

## L07

RenCrow_LLM `d2166b4dffa9c21341dd2cace8c39cba690288c0`、`gateway/internal/messagewire/system_messages.go` lines 1–240、blob `544d68db5e36b5d4a47a8a5e1004feaef3ab2d99`。2026-10-07読取。既存single_leading_instructionは全体走査・trimを行い、strictのprefix限定byte保全と同等ではない。

