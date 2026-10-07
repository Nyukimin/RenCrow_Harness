# RenCrow_Harness v0.2.2 実装担当Claudeへの作業入口

2026-10-07 JST / 設計責任: ルミナ / 実装・調査・試験: Claude
**この版を実装の基準とする。前版を読むことも、この会話を知っていることも必須ではない。**

## 1. 今回作るもの

RenCrow_Harnessは、人がCLIから使い、RenCrow_COREも作業を委譲できる共通の実行ツールである。独立したGo実行プログラム`rencrow-harness`を作り、同じService/Kernelに対話CLI、`exec --json`、`serve --stdio`を接続する。COREを停止していても、設定済みRenCrow_LLMを利用して作業・保存・圧縮・再開できなければ未完了である。

COREはAgent、Persona、長期Memory、上位の割当を持つ。Harnessは受けた子Taskと作業実行履歴を所有する。RenCrow_LLMがモデル差を吸収する。Harnessにモデル別parserや別Gatewayを作らない。

## 2. 読む順序

1. 本書、[実装前最終回答](IMPLEMENTATION_DECISIONS.md)、[ARCHITECTURE_DECISIONS](ARCHITECTURE_DECISIONS.md)。17件への決定と裁量範囲。
2. [REPOSITORY_MAP](REPOSITORY_MAP.md)。初見向けのシステム説明、pin、読むsource。
3. [仕様書](RenCrow_Harness_SPEC.md)と[実装仕様書](RenCrow_Harness_IMPLEMENTATION_SPEC.md)。目的・owner・不変条件・状態機械。
4. [WORK_ORDER](WORK_ORDER.md)。Claudeが実施する作業、依存、具体的成果物、完了条件。
5. [PROTOCOL](PROTOCOL.md)、[LLM_INTEGRATION](LLM_INTEGRATION.md)、[RETRY_CONTRACT](RETRY_CONTRACT.md)、[CORE_INTEGRATION](CORE_INTEGRATION.md)、[STORAGE](STORAGE.md)、[CONFIGURATION](CONFIGURATION.md)、[INTERNAL_CONTRACTS](INTERNAL_CONTRACTS.md)。担当部分の詳細。
6. `schemas/`、`sql/`、`prompts/`、[試験対応](ACCEPTANCE_MATRIX.md)。本文と機械可読定義を一緒に実装する。

`IMPLEMENTER_BUNDLE.md`は上記Markdownを連結した生成物であり、別正本ではない。変更は元ファイルへ行い、[再生成script](../tools/speccheck/rebuild_bundle.py)で作り直す。

## 3. 設計を再度決め直す必要はない

接続はGatewayのみ、Go独立executable、Harness所有store、act最大2Attempt、Compaction各段1生成、初回対象Qwen38_Flash、CORE側revision/relay issuer追加、Resumeのlimits明示、が決定済みである。詳しくはAD-01〜16とQ01〜17を参照する。

Claudeはrepositoryのownerを理由に「LLM側で対応してください」と戻さず、許可された対象repositoryの必要変更も実施する。source上の反証・権限不足・実機不足は特定して報告する。推測の代替実装、未実装機能の広告、受入条件の削除はしない。

## 4. 最初の作業

WP00のbaseline照合を実施する。各worktreeのbranch、HEAD、未commit変更、AGENTS/作業規約を読む。既存変更をreset/checkoutで消さない。読取基準pinは再現用で、worktreeをそのcommitへ戻す指示ではない。

次に既存LLMのstrict/計数の短い実接続をWP02〜03で成立させる。この作業と並行してHarnessの純粋kernel/SQLiteを作る。CORE向けContextBlock/relay発行も早期に着手し、P5の直前まで未定義の供給物を残さない。

現状証拠が不足した場合は、取得したpath/commit、欠けたもの、影響範囲、次に必要な権限を記録する。単に「調査が必要」とだけ書かない。資料にない挙動を推測で埋めない。

## 5. 変更権限と設計不一致の処理

本パッケージはれんから委任されたルミナの設計指示である。ただしCORE/LLMの既存正本へはまだ反映していない。WP01で対象の共有契約へ差分を反映してから接続する。各変更に設計決定IDを付け、既存規約を黙って迂回しない。

契約を変えない内部実装の選択はClaudeが行う。公開契約・owner・経路・試行上限・安全保証・初回範囲に反証が出た場合、該当境界だけ止め、[設計照会ひな形](templates/design_question.json)に証拠を記録してルミナへ返す。無関係な独立作業は続ける。製品にapproval_waitを追加する意味ではない。

本番へのpush、既存service停止、モデル再起動、既存DB移行は、作業環境で与えられた権限に従う。製品No-Human-Gateを開発者の包括的変更権限に読み替えない。

## 6. 最短の実用受入

macOS上の隔離workspaceと初回Qwen bindingで、人向けCLIからファイル修正・検証・手動/自動Compaction・停止・cold resume・原本参照を実行する。同じ課題を隔離COREからstdio経由で実行する。二つの経路が同じService/Kernel・保存・検証契約を使うことをTraceで確認する。これをP6A先行受入とする。

Ubuntu/Windowsのnative入出力・保存・停止・CORE client、session/fork、全必須回帰を終えてP6B正式完成となる。MLXはMacの外部computeであり、三OSそれぞれでMLXを起動する要件ではない。P7旧Switch移行は必要なthreadだけ別実施する。

## 7. 報告

工程ごとに[作業報告ひな形](templates/work_report.json)を使い、変更、根拠、実行command、actual result、未実施、残るblockerを区別する。テストを減らしてpassedにしない。レビューの同意、仕様の存在、schema成功、mock成功をruntime成功へ転用しない。

本版には製品runtime実装は入っていない。`checks/`は設計資産の検査用である。実モデルを動かさず、全runtime試験の初期statusはnot_runとする。

## v0.2.2の必読追加

実装前の17照会を[IMPLEMENTATION_DECISIONS](IMPLEMENTATION_DECISIONS.md)で確定した。BYTE_CONTRACTS、MODEL_PROJECTION、STAGE_DATA、CHECKPOINT_FORMAT、ERROR_MAPPING、STREAM_CONTRACT、HOST_ASSETS、EVENT_CONTRACTは本文の規範付属契約。曖昧な既定値を実装者裁量で補う対象ではない。

変更されていないContext revision/HMAC/recovery policyの旧goldenも維持する。新しいwire fixturesはexamples/wire、追加検査はchecks/validate_wire_contracts.py。製品実装/実モデルE2Eは別のnot_run試験である。

