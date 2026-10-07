# 設計決定と作業権限

RH-ADR-001 / v0.2.2 / 2026-10-07 JST
設計責任者: ルミナ。実装・調査・試験担当: Claude。要求主体・運用権限者: れん。
状態: **本依頼に対してルミナが確定した実装指示。既存repositoryへの正本反映・製品実装・配備は未実施。**

## 1. 誰が何を決めるか

れんの「COREも利用し、れん自身もToolとして使う」を製品目的とする。ルミナは目的、範囲、所有権、公開契約、安全境界、工程の完了条件を決める。Claudeはこの決定に従い、対象repositoryの調査、契約の反映、コード、試験、証拠収集を行う。RenCrowを既に知っていることは前提にしない。

表にあるmodule ownerは**コードと運用責任の所在**であって、別の担当者へ仕事を保留する理由ではない。Claudeは、作業権限のあるCORE・LLM・profile・Harnessの必要変更を一つの計画として実施する。権限や実機アクセスがない部分だけを具体的な阻害条件として報告する。

Claudeが追加判断なしに決めてよいものは、契約を変えない内部関数分割、同等の標準API、テストfixtureの構成、ファイル名の軽微な調整である。公開field、owner、LLM経路、試行上限、保存意味、origin、初回範囲を変更してはいけない。

既存正本や実測が本指示を反証した場合、事実を隠して実装を強行しない。該当境界だけ停止し、`対象決定ID / source・行・commit / 再現条件 / 影響する受入 / 最小変更案`をルミナへ返す。他の独立した工程は続ける。通常の各作業やcommitのたびに、設計の再承認を要求する工程にはしない。本書は本番再起動・push・破壊的操作の包括許可ではない。

## 2. 今回確定する決定

| ID | 決定 | 採用理由と境界 |
|---|---|---|
| AD-01 | native Goの独立実行プログラム。CLIとCORE用stdio JSON-RPCは同じService/Kernelを使う | CORE専用ライブラリへ縮小しない。Go 1.25.0は参照COREとの初期互換基準で、性能優位の実測主張ではない |
| AD-02 | 推論は既存RenCrow_LLM Gatewayのみ。初回に物理Backend直結adapterを作らない | CORE非依存とGateway非依存を混同しない。モデル補正・計数の第二実装を増やさない。明示直結そのものがRH-012違反だからではない |
| AD-03 | Harnessが子Task、実行Run/Action/Attempt、作業原本、checkpointを所有 | COREは親の割当/成果採用、人格、長期Memoryを所有。既存IDの意味を維持してissuerを追加する |
| AD-04 | 各strict HTTP生成要求は最大1Backend生成。actの回復はHarnessが同Actionの新Attemptとして最大1回実施 | hidden retryをなくすが、回復手順はなくさない。RETRY_CONTRACTの許可表を唯一の規則とする |
| AD-05 | Selection/Summaryは各最大1生成、失敗時はEmergency。actの再試行を流用しない | Compaction正本の要求数/失敗分類を維持。段profileを先に適合させ、品質を実モデルで測る |
| AD-06 | 初回実bindingはRenCrow_Qwen38_FlashのQwen/MLX構成。最終入力計数とstrict正常化をLLM/profileの必須作業に含める | 別Modelで試験が通ってもこの対象の代用にしない。実機identityはWP00で記録し、hardcodeしない |
| AD-07 | 計数は既存の適合APIを優先。なければ同一engineの生成前処理を共用する非生成handlerを追加 | engine改変を最初から必須とはしない。手書きtemplateや経験比率をexact/boundと呼ばない。成立不能なら対象変更はルミナ判断へ戻す |
| AD-08 | ContextBlock公開fieldはkind/text/revision/sourceの4個。digestは受信Hostが計算する内部field | sourceのhashと本文digestとrevisionを別にする。CORE側revision供給を今回実装する |
| AD-09 | ResumeInput.limitsは必須。新Runのdeadline/試行予算を明示して保存 | 旧Run実績は保持。null/省略を暗黙継承としない。CLIは表示・解決してから送る |
| AD-10 | 全CLI/CORE入力にdurable intake receipt。COREのHuman relayはHMAC proofを作成・検証 | profile宣言の記録と本人性の完全証明は別。非対話inputをrole名だけでHumanへ昇格しない |
| AD-11 | 正式初回完成はP6B。P6AはmacOSの先行受入で、CLIとCORE両方を要求。Windows/Ubuntu受入とsession/forkもP6Bまでに実施 | 三OSを無断で削らない。P7旧Switch移行は独立した任意工程で、新規作業利用を妨げない |
| AD-12 | source参照pin、EcoSystem指定pin、実機binary/engine実体を別々に記録 | 参照用Switchを上げたことにして既存配備を動かさない。差分を解消するとは、意味と適用先を特定することである |

## 3. 一次根拠と設計判断

CORE/LLM/Switchの既存契約はREPOSITORY_MAPとSOURCESのpin付き原資料を根拠とする。論文は七要素とInterface/Session共通基盤の構造比較、Codexは入口・実行型・履歴の実装参考である。論文の記述をRenCrowの実測成功、特定モデルの優劣、特定言語の性能保証に転用しない。[P01 §2.3/15.6][O01]

直近Claudeレビューの「工数未見積り」「act再試行未定義」「CORE供給物未定義」は不足の根拠として採用した。採る方式は上表でルミナが決定した。Completion Linkの各2,048 bytesは新しい緩和ではなく、Switch付属A F12の明文を継承する。[S02]

未確認として残す事実は、対象engineの実機source到達性、配備binary一致、実モデル成功率、実工数である。これらの取得方法と不成立時の処理はWORK_ORDERに定義し、方式の未決と混ぜない。

[P01]: SOURCES.md#p01
[O01]: SOURCES.md#o01
[S02]: SOURCES.md#s02

## v0.2.2追加決定

| ID | 決定 | 担当する付属契約 |
|---|---|---|
| AD-13 | 論理input_digestとLLM所有request_digestを分離し、CJ1+LPで正規化 | BYTE_CONTRACTS |
| AD-14 | Shiro明示native coding profileを旧3分岐の前で切替 | IMPLEMENTATION_DECISIONS§3 |
| AD-15 | actはstream、Tool実行はstrict終端後 | STREAM_CONTRACT |
| AD-16 | 各Attemptのmeasure→generateを初回採用 | STREAM_CONTRACT§4 |
| AD-17 | 共通投影/段data/候補/イベント/Host資産をclosed契約とする | MODEL_PROJECTION、STAGE_DATA等 |

