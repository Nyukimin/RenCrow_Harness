# 参考資料からの採否

論文P01はソース比較で、共通条件の速度・信頼性実測ではない。以下は著者の推奨を参照した**RenCrow側の設計判断**であり、論文がRenCrowの構成を実証したという意味ではない。

p.4図1のInterface Layer、p.5のSession substrateを、CLI/CORE双方の利用と共通履歴に採用する。七要素は役割のチェックリストとし、OrchestrationやExtensibilityの最大構成を全部実装する指示にはしない。

| 推奨 | 資料中の主題 | 本版の採否 | 判断 | 資料位置 |
|---|---|---|---|---|
| R01 | 小さなloop、独立したturn policy | 採用 | pure transitionとdriver。初回から無制限workflow frameworkを導入しない。 | §16.1 |
| R02 | provider条件を中央の層で維持 | 採用 | 既存RenCrow_LLMに集約。HarnessのModel名分岐は禁止。 | §16.2 |
| R03 | Toolは必要なものから | 適用調整 | 既知のcoding/保持故障があるためfile/process/Evidenceを初回に用意する。bashだけへ退行しない。 | §16.3 |
| R04 | Tool数増加時はdeferred loading | 限定採用 | 初回の小さなbuilt-in集合では全schema。Skillはmetadataのみeager。大catalogは予算に基づく別段階。数15を絶対閾値にしない。 | §16.3 |
| R05 | edit contractを用途/Modelへ合わせる | 保守的な選択 | 初回はunique exact＋preimage hash。fuzzy repairは実測根拠なしに加えない。 | §16.4 |
| R06 | 階層Markdown context | 採用 | trusted roots/AGENTSとscope/digestを保持。無差別home探索やauthority昇格はしない。 | §16.5 |
| R07 | threshold/増分summary/overflow共通 | 採用調整 | Normal/Emergencyを共通核。Human原文と原本保全は既存RenCrow契約を優先し、単純tail保持だけにしない。 | §16.5 |
| R08 | code retrievalは決定的探索 | 初回採用 | file.searchから開始。COREの会話Memoryにあるembedding等を削除する根拠にはしない。 | §16.5 |
| R09 | developer向けapproval modes | そのままは不採用 | No-Human-Gateとの整合から同期policyと明示設定。approval_waitを初回runtimeへ持たない。 | §16.6 |
| R10 | shared/automated用途のOS sandbox | 要件として区別 | 初回trusted_hostは非隔離の明示mode。隔離が必須なら実adapter受入までunavailable。policyで代用したと称さない。 | §16.6 |
| R11 | policyをdataとして保つ | 採用 | runtime権限をpromptから変更不可。profileとhost上限で同期判断。 | §16.6 |
| R12 | 必要になるまでsingle-agent | 採用 | COREの協調は上位。Harness内spawnは初回なし。将来も親権限の部分集合。 | §16.7 |
| R13 | ACP等outward interface | 目的を採用、protocolは延期 | 初回はnative stdio/Go client。ACP互換を偽装せず、将来adapterで同じServiceを公開する。 | §16.7 |
| R14 | SkillsとMCPを役割別に | 限定採用 | trusted Skillと既存Tool adapter。MCP serverをToolという語から必須化しない。 | §16.8 |
| R15 | 汎用agentic frameworkを使わない | 初回採用 | 自前の短い明示Kernelを持つ。論文の不採用観察を普遍的優劣の証明にしない。 | §16.9 |
| R16 | vector code RAGを初期導入しない | 初回採用 | code検索は決定的。Domain固有必要性は別比較で評価。 | §16.9 |
| R17 | SaaS全APIを1:1 Tool化しない | 採用 | 仕事に必要な契約を絞る。Tool実行ownerを複製しない。 | §16.9 |
| R18 | 安価な停止/反復上限 | 採用 | step/deadline/output/同一失敗の有限上限。全文LLM監視を常時追加しない。 | §16.9 |

## Codexを参考にする範囲

公式のApp Server/execの入口、typed event、Thread/Turn、steer/interrupt、stored-session操作を、公開contractとsourceの両方から参考にする。[O01][O02] RenCrowのAPIや保存形式をCodex互換と称さず、同じThread/Turnという語でもCanonical IDの意味はRenCrowを優先する。

Codex全体を「shell executor」として呼び出して、さらにHarnessのModel/Tool loopで包む構成は初回に採用しない。CodexのAgent RuntimeをLLM inferenceと同一視しない。独立した委譲capabilityとして利用する場合は別契約にする。

既存SwitchのP0〜P4、原本・Human・Observation、Normal/Emergency/Capacity/Integrity/Persistenceの契約を継承する。v0.2.2の独立store、入口、typedJSON summaryは新Harnessの設計であり、既存Switchの実装済み挙動と混同しない。

論文に含まれるvendor市場動向、非公開source snapshot、自己報告benchmark数値は本設計の性能根拠や実装素材として使用しない。sourceを再利用する場合は対象commitのLICENSE/NOTICEを確認し、必要な帰属を保つ。参考にしたこととcodeを移植したことを区別する。


[O01]: SOURCES.md#o01
[O02]: SOURCES.md#o02
