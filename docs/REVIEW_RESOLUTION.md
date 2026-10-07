# レビュー対応と設計決定

v0.2.2。CR01〜13は前回対応、CR14〜23は今回の確定。文書対応はruntime修正の検証成功ではない。

| ID | 指摘 | 決定 | 反映 | 試験 |
|---|---|---|---|---|
| CR01 | 独立Harness目的と所有権 | COREもCLIも初回利用。Harnessが実行/原本/再開を所有。COREは親Task/人格/Memory。 | SPEC§1〜6、IMPL§1〜2、STORAGE | A01, A19, A20, A40 |
| CR02 | Chat経路へno-tools保護がない | Chat strictを初回必須実装、Responses共有normalization、hidden retry無効。 | LLM_INTEGRATION§1〜4、IMPL§7 | H01, H12, A26 |
| CR03 | Preflightの遷移矛盾 | 最低候補no-fitはCapacityBlocked、Emergency禁止。budget不明も別分類。 | SPEC§11.3、IMPL§10.5 | A04, A24 |
| CR04 | budget_unverifiedの動作未定義 | 四状態と許可範囲を固定。初回bindingに非生成token計数の実装まで必須。 | SPEC§10、IMPL§8、LLM_INTEGRATION§2〜3 | A04, A25, A29, A40 |
| CR05 | Compact/Resume型の欠落 | 全field、5outcomeと7Run状態/control結果、async receiptを定義。 | PROTOCOL§5〜6、schema、IMPL§11 | A11, A21 |
| CR06 | Summary handle体系 | 5配列と0始まりhandle、前回accepted一件、instructionは参照data。 | IMPL§10.4、summary schema/prompt | A22 |
| CR07 | 撤回とcompletion条件 | revocation根拠をreplacement_handleで明示、quote exact。unique/full/size/partial制約を列挙。 | SPEC§11.2、IMPL§10.2〜3、selection schema | H09, A23, A39 |
| CR08 | 追加入力の分類field | dispositionとqueue/control/application revisionを定義。 | PROTOCOL InputAppendInput/InputReceipt、IMPL§5/12 | A06, A12, A30 |
| CR09 | 段専用prompt owner | Harnessが段promptを所有。Human別保持/data-not-command/no-tools/partial必須。 | SPEC§9、IMPL§7/10、prompts | H18, A22, A26 |
| CR10 | A試験の関数逆参照 | 全H/Aにfunctions、全Fにtestsを生成し双方向照合。 | acceptance_matrix.json、checks | A21, A40 |
| CR11 | 関数の工程割当不足 | 全30関数にphase。F02/F17=P2、F15=P3、F16=P2を明記。 | ACCEPTANCE_MATRIX、IMPL§14〜15 | A11, A40 |
| CR12 | source照合の補正未反映 | C14実物取得。five blocks、Actionless、retry、Kuro、12正本、命名を反映。実機未確認は残す。 | REPOSITORY_MAP、IMPL§2/6/7/14 | A03, A09, A26, A31 |
| CR13 | source pinとRuntime拒否境界 | L02 blob ecd1877313238515692060f9aabb1a2cd4f0f770取得。json_object+streamはRuntime拒否と明記。 | SOURCE_BASELINE L02、LLM_INTEGRATION§1 | A21, A26 |
| CR14 | Gatewayか直結かの食い違い | Gatewayのみを確定。CORE非依存とGateway非依存を区別。直結自体が暗黙fallback違反とはしない | AD-02 / SPEC§10 / LLM_INTEGRATION | A19, A26, A50 |
| CR15 | LLM/Backendのcritical pathと未見積り | 初回Qwenを固定し外部変更もClaudeの作業。相対工数と依存を表示。既存API優先、engine改変は必要時だけ | AD-06/07 / WORK_ORDER WP02〜05 | A25, A50, A54 |
| CR16 | hidden retry禁止後のact回復欠落 | act最大2Attempt、同Action新ID、モデル補正はLLM。Compactionは各段1生成のまま | AD-04/05 / RETRY_CONTRACT / schema / SQL | A41, A42, A43, A44, A45, A52 |
| CR17 | CORE revisionとHMAC issuerが未割当 | producer実装を作業化しrevision/HMAC byte定義とfixtureを作成 | CORE_INTEGRATION / WORK_ORDER WP07〜08 | A46, A48, A53 |
| CR18 | ContextBlock.digest不一致 | 公開4fieldを維持、computed digestは内部Prepared型 | AD-08 / PROTOCOL / INTERNAL_CONTRACTS | A46, A21 |
| CR19 | ResumeInput.limits欠落 | 全limits必須、新deadline/予算を保存返却。旧実績は保持 | AD-09 / PROTOCOL / RETRY_CONTRACT / SQL | A47, A44 |
| CR20 | Completion Linkの2048bytes解釈 | 付属A F12の各<=2048を確認。解釈変更でなく根拠と境界試験を追加 | SPEC§11.2 / IMPL§10.3 / S02 | A51 |
| CR21 | CLI intake receipt明記不足 | 全入口のdurable receiptを規定、profile宣言と本人性の限界を表示 | AD-10 / CORE_INTEGRATION§3 / PROTOCOL | A48, A49 |
| CR22 | EcoSystemとSwitch参照pinの差 | 参照/配布指定/稼働版を分離。新Harnessに旧Switch更新を要求しない | AD-12 / WORK_ORDER§5 / SOURCE_BASELINE | A54 |
| CR23 | 初回範囲の段階化 | P6A macOS両入口は先行受入。三OSとforkを残したP6Bが正式完成、P7移行は任意 | AD-11 / WORK_ORDER WP09〜11 | A16, A19, A20, A36, A37, A40 |

## 実装前最終確認Q01〜Q17

CR24〜CR40としてreview_resolution.jsonへ登録した。設計回答と規範付属書は[IMPLEMENTATION_DECISIONS](IMPLEMENTATION_DECISIONS.md)。各点にA55〜A71を一対一対応。実装試験は未実施。
