# Claudeへの実装作業指示

RH-WORK-001 / v0.2.2 / 設計責任: ルミナ / 作業担当: Claude
本書の工程は、設計の選択をClaudeへ委ねる一覧ではなく、決定済み仕様を実装する順序である。

## 1. 対象と成果物

新設RenCrow_Harness、RenCrow_LLM、RenCrow_Qwen38_Flash、RenCrow_CORE、RenCrow_EcoSystemの必要差分を実施する。実engineへのsource変更が必要ならprofileから追跡できるpatch/revisionとして納品する。RenCrow_Switch_Coreは参照のみを基本とし、P7移行が明示対象になった場合だけexport/import境界を変更する。

実装担当Claudeは、module ownerが違うという理由だけで作業を未割当にしない。アクセスや権限がない場合はその対象を特定し、無権限で更新しない。private logや実機sourceにアクセスできないことを、実証した事実として埋めない。

## 2. 着手順と依存

```text
WP00 baseline/source可用性 → WP01 共有契約反映
                              ├─ WP02 LLM正常化 → WP03 同一前処理計数 ─┐
                              ├─ WP04 Harness kernel/store/Tool ─────┼─ WP05 act/retry → WP06 Compaction
                              └─ WP07 CORE Context/relay ────────────┘                 │
                                                           WP08 CORE委譲client ←─────┘
                                                                    ↓
                                                         WP09 macOS両入口受入(P6A)
                                                                    ↓
                                                         WP10 三OS/正式release(P6B)
                                                                    ↓ 必要なthreadのみ
                                                         WP11 Switch移行(P7)
```

P1はWP02/03/04/07を並行に進められる。1人の作業者でも、fakeによるkernel試験とLLM実接続の阻害を分離する。LLM計数の未成立を最後まで隠さない。

## 3. 作業票

相対工数はルミナの**未校正の計画値**であり、時間やClaudeの所要日数ではない。1=小さな局所変更、3=複数部品、5=状態/境界を跨ぐ変更、8=外部engineまたは複数OSを跨ぐ高不確実作業。実時間はWP00以後の作業ログで記録し、数値を実績として扱わない。engine改変不要と判明したらWP03の残見積りを更新できるが、完了条件は変更しない。

| WP | 段階 / 相対工数 | Claudeが作るもの | 依存と完了証拠 |
|---|---|---|---|
| WP00 | P0 / 3 | worktree/HEAD/dirty一覧、参照pin・EcoSystem pin・実機binaryの区分、Qwen engine source/計数API可用性、対象route | read-only。アクセス不能を明記したbaseline report。全repo未読のまま設計しない |
| WP01 | P0 / 3 | AD-01〜12をCORE12正本の関係章、LLM caller/API、ID issuer、Harness store、EcoSystem登録へ反映するdiff | 設計ID→変更章対応。製品実装がない段階でrelease済みと広告しない |
| WP02 | P1/P3 / 5 | Chat/Responses共通normalization、strict単一生成、typed error/receipt、stage/recovery許可 | H01/H12/A26/A41〜45。既存無変更route回帰を含む |
| WP03 | P1/P3 / 8 | 最終prompt計数、Runtime→engine adapter、必要時の共用前処理handler、Qwen profile適合 | A25/A29/A50。同一requestのmeasure/生成token一致、非生成・無重複計数 |
| WP04 | P1/P2 / 8 | Go binary、全入口Service、SQLite/原本/lock、CLI/stdio、file/process、取消・resume/limits | mock Model＋実Tool/DB、kill/restore、dual writer拒否。P2だけで実モデル成功としない |
| WP05 | P3 / 5 | act/recovery policy、新Attempt・再measure・予算・provisional切替・receipt | A41〜45/A52。実Qwenの形式回復とunknown/cancelの拒否 |
| WP06 | P4 / 8 | Normal/Emergency、5結果、保持/撤回/handle、前回summary・範囲、session/fork | H01〜26、A22〜25/A51。repeated/cold resume、各2,048 bytes境界 |
| WP07 | P1/P5 / 5 | CORE typed Context snapshot/revision、HMAC issuer/鍵設定、Harness intake verify/receipt | A46/A48/A49。五区分、本文同一性、署名ベクトル、nonce/期限/別Thread拒否 |
| WP08 | P5 / 5 | CORE nativeharnessclient、親/子Task相関、明示backend選択、events/result/取消projection | A20/A31/A53。実入口Traceで旧Codex/Actionlessへ戻らない |
| WP09 | P6A / 5 | macOS隔離構成でCLIとCOREの同課題、Qwen実binding、backup/restore、連続圧縮、再開 | 両入口実行証拠、未実施OS表示。正式releaseではない |
| WP10 | P6B / 8 | Ubuntu/Windows/macOS native試験、session/fork、配布成果物とhash、LLM/profile版の組、正式受入report | 全必須試験。source/配布指定/稼働値を混同しない |
| WP11 | P7 / 5 | 必要な旧threadのexport/停止/import、元raw保全と後続履歴closure | 任意。対象sourceごとの実変換試験。P6B新規作業の完了に依存させない |

詳細な関数割当と試験参照は`work_packages.json`と`acceptance_matrix.json`で検査する。相対工数の合計は生産性予測ではない。最長依存はWP02→03→05→06→08→09→10で、独立Harnessの完成を外部LLMの未割当作業にぶら下げたまま報告しない。

## 4. 各WPの進め方

最初に対象契約と実際のcallsiteを読む。次に同じ失敗を再現するfixture/negative試験を作り、実装後の差を見る。修正したsourceだけでなくproduction compositionの経路を照合する。privateの実運用入力は合成fixtureへ置換して公開し、必要な実rawは私有Evidenceへ保存する。

変更したschemaはPROTOCOLの表・例・client・server・テスト・DDL payload契約を同時に更新する。schemaだけ追加してproducerを未実装にしない。CORE供給物やLLM receiptのconsumerだけを実装してもWP完了ではない。

新しい正本ディレクトリをCORE側へ増やさず、既存12仕様の関連章を更新する。非該当の章は理由を記して無意味な変更を避ける。Harness内部の詳細文書は本パッケージの体系を使う。

## 5. baseline差分の固定処理

参考source: CORE `82472307…`、LLM `d2166b4d…`、Switch `9331c99b…`。EcoSystemの調査pin `56c1f607…`はSwitch `16ea1009da01f04354cdec329fe4b89dd3df7f61`を指定している。[E01]

これらは一致する値ではない。F36 VerifyReleaseSourcesで、(1)設計参照、(2)EcoSystem配布指定、(3)実機artifact/source、(4)新成果物のbuild sourceを別recordにする。今の稼働版は未確認のまま入力することを許すが、それでは配備受入は通さない。

新HarnessはSwitch binaryに依存しないので、P0で参考pinに合わせて旧Switchを更新しない。P7だけは実データを生成した版とexport契約を照合する。新moduleのrelease pinは実成果物を検証してから登録する。

## 6. 終了条件とルミナへの返却

設計確認のみ、buildのみ、fakeのみ、CLIのみ、COREのみの完了報告は不可。P6A/P6Bの区分と実行範囲を必ず示す。`not_run / failed / passed / blocked`を試験ごとに維持し、失敗が残ることを理由に対象を消さない。

工程報告にはWP、source/build/configの版、実装diff、実行command、actual結果、関係試験ID、未実施、次の独立作業、設計照会を含める。ルミナは証拠から採用/修正を判断する。Claudeによる実装合格報告と、設計責任者の受入判断は区別する。

[E01]: SOURCES.md#e01

## 7. v0.2.2最終指示

17照会の採用判断は[IMPLEMENTATION_DECISIONS](IMPLEMENTATION_DECISIONS.md)。WP02/03/05はBYTE_CONTRACTSとERROR_MAPPING/STREAM_CONTRACT、WP04/06はMODEL_PROJECTION/STAGE_DATA/CHECKPOINT_FORMAT/EVENT_CONTRACT/HOST_ASSETSを同時に実装する。codeとschemaのproducer/consumerを両方持つ。

WP08の対象はShiro shiro_native_coding_v1（新規受付）で、tryExecuteCodexWorkPathより前の一つの明示分岐。旧Subagent/Codex/plain Generateへ戻らない。WP00は対象選択でなくsource配置/可用性の確認である。

Go moduleはgithub.com/Nyukimin/RenCrow_Harness。F37〜F40とA55〜A71を追加した。初回着手時に参照codecで期待値を上書きして製品試験を通すことは禁止し、固定goldenに対してGo側を独立に実装する。

