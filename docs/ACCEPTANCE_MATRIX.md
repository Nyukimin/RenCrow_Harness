# 受入試験と関数の対応

実行状態の正本はacceptance_matrix.json。全件not_run。文書/fixture検査を製品E2E合格へ転用しない。
## 試験

| ID | 条件 | 期待 | 関数 | 必須範囲 |
|---|---|---|---|---|
| H01 | no-tools要求にToolIntentまたはtool-call XML相当の不正出力を返す。LLM層からの契約違反を注入する。 新Harnessが実際に利用するChat strict pathとResponses既存pathを別々に試す。 | 不正本文を要約・実行成功へ採用しない。LLM ownerの正規化試験とHarnessのSummary拒否を別々に通す。 Chat側未適用を残さず、strict contract receiptを照合する。 | F09,F10,F15 | P6B |
| H02 | 既知のrequest型欠落、未知handle、hash/rangeを返さないモデル応答を投入する。 | 無効要求を送信前に拒否し、ID/hash/rangeはhost inventoryから構成する。LLMへの転記依存を残さない。 | F01,F08,F15 | P6B |
| H03 | metadataを省略・未知化し、rollback/過去checkpoint境界を含むfixtureをreplayする。 | 既知の等価正規化だけを許可。未知境界を推測通過せず、旧指示を新しい有効指示にしない。 | F02,F13,F17 | P6B |
| H04 | negative-validation用の撤回済み文字列を含む検証入力と、分離済みの生成入力を用意する。 | 生成要求へ撤回済み本文を漏らさず、再生成されたSummaryに復活しない。 | F08,F09,F10 | P6B |
| H05 | ContextLimit、length、incomplete、usage欠落を返す。 | completedへ丸めず、保持必須部分は切らない。usageはunknown。有限のEmergency/Blocked分類を行う。 | F05,F15,F19 | P6B |
| H06 | 合成source190件に対しモデルは188件相当しか言及しない。 | host inventoryは190件を保持する。重要参照の言及件数と全件coverageを区別し、適用対象はhostが照合する。 | F06,F08,F13 | P6B |
| H07 | 大量Tool結果・Work・reasoningと、次の通常要求を含むfixtureを用意する。 | 安全な参照化/要約後の実際の次要求を測る。圧縮表示だけで縮小成功にしない。 | F05,F11,F12 | P6B |
| H08 | 未実施テストをpassedとするSummaryを返す。 | 既知の証拠と矛盾するSummaryを拒否し、Task成功にしない。検証未実施を保持する。 | F10,F20 | P6B |
| H09 | 同thread内に大量の無関係Tool出力を置く。 | 明示された検証済みlinkだけを選別材料にする。選別input量とSummary input量を別計測する。 Completion linkの全文size、唯一性、origin、nonpartial/nonactive条件を検査する。 | F06,F07 | P6B |
| H10 | 末尾の段指示と、Gatewayが追加する注意を区別した要求を通す。 | 変換前後の意味順序をLLM ownerのreceiptで照合。極端に空疎な応答を正常Summaryにしない。 | F04,F09,F10 | P6B |
| H11 | metadataの省略と正式defaultの違いを含むcheckpointを保存・再読込する。 | 定義済みの等価性だけを正規化し、それ以外の改変はIntegrityBlocked。 | F02,F13,F14,F17 | P6B |
| H12 | 初回および許可された補助再送でも不正な要約を返す。 新Harnessが実際に利用するChat strict pathとResponses既存pathを別々に試す。 | 再試行を有限にし、Summaryへ不採用。NormalからEmergencyに遷移したことを記録する。 Chat側未適用を残さず、strict contract receiptを照合する。 | F09,F10,F12 | P6B |
| H13 | Compaction中に通常入力をqueueへ追加する場合と、context/control revisionを変更する場合を分ける。 | queueだけなら適合candidateを採用可能。真のrevision変更または取消なら拒否する。 | F13,F14 | P6B |
| H14 | origin不明、Hostの重複context、古いHuman/Automationメモを含む履歴を複数回圧縮する。 | originを推測しない。同source Hostは最新revisionへ投影。Human/Unknownの保護を維持する。 | F03,F04,F08 | P6B |
| H15 | 開始時の手順と最新の未完了手順が異なる長作業fixtureを用意する。 | 現在地・未完了・次作業を区別し、完了証拠なしに実施済みを捏造しない。通常再開で不要な開始手順反復を評価する。 | F09,F10,F20 | P6B |
| H16 | 隔離Summaryと候補のcache共有方式を同一入力・bindingで比較する。 | 既定は隔離方式。Backend上のcache再利用とprefillを測り、論理hashだけで高速化を主張しない。 | F04,F05,F09 | P6B |
| H17 | HumanとAutomationの訂正、受付hash/Thread違いを組み合わせる。 | AutomationからHumanを失効できない。originの証拠不一致はUnknown。常設制約を無根拠削除しない。 | F03,F08 | P6B |
| H18 | 通常実行用instructionが付いた選別要求と、段専用要求を比較する。 | production選別要求は段専用instruction＋data、toolsなし。詳細記録の対象がGPT120Bである点を保持する。 詳細原資料はGPT120Bであり、Qwenだけの固有故障とは説明しない。 | F07,F09,F15 | P6B |
| H19 | Normalが連続失敗し、excerpt markerと未要約Workが累積するfixtureを用意する。 | durable/semantic境界を分離し、検証可能な旧excerptを小さい参照へ投影。縮小不能ならCapacityBlockedでありNormal成功ではない。 | F11,F12,F13 | P6B |
| H20 | Emergency checkpointはあるがNormal stage receiptが欠けるfixtureを用意する。 | 原因はunknownとして集計。EmergencyをNormal成功にしない。新実装はstage receiptを相関できること。 | F19,F20 | P6B |
| H21 | Summary応答へ許可外role/itemを混入させる。 | 単一の許可された最終Summaryとして検証できなければ拒否。混入itemの種類を推測で補わない。 | F10,F15 | P6B |
| H22 | 手動・自動・resume経路に異なるfeature選択を与える。 | managed routeは唯一のdriverへ入り、旧方式へfallbackしない。切替対象外runtimeは明示区別する。 | F01,F17 | P6B |
| H23 | InputText配列、空part、改行、truncation、mixed mediaを投入する。 | text-onlyはversion付き決定的投影、raw/projection hashとrangeを区別。mixed mediaはprotected。 | F06,F11,F18 | P6B |
| H24 | canonicalにtextがありlive表示だけでは比較できないfresh結果と、既存markerを用意する。 | freshは検証済みcanonical projectionで比較。既存markerへ無条件fallbackしない。 | F05,F06,F11 | P6B |
| H25 | 明確な不良計測、曖昧なJSON破損、原本/checkpoint破損を別fixtureにする。 | owner allowlistだけ除外しmetrics_missing。原本不変。checkpointなしをparse成功から推定しない。 | F02,F14,F19 | P6B |
| H26 | index/組立candidateの不良と、raw/checkpointの不良を分けて注入する。 | 健全原本からの決定的派生再生成は最大1回。確定raw/checkpointの推測修復・古いcheckpointへの暗黙fallbackはしない。 | F12,F17,F19 | P6B |
| A01 | standaloneとCORE委譲を同じfixtureで開始/再開 | Canonical prefixが一致し、子Task関係を保持。resumeは同Taskかつ新Run/Trace。COREへのID照会なし | F01,F17,F22 | P6B |
| A02 | 許可外workspace/Tool/実行modeを要求 | 副作用0、FORBIDDENまたはPOLICY_REJECTED。CLI/Coreで同じ判定 | F01,F16,F30 | P6B |
| A03 | Shiro由来の四block＋input、通常Tool継続を投入 | 各区分・本文・順序を保持。flattenしたSystemPromptから由来を捏造しない | F04,F26 | P6B |
| A04 | exact/bound/estimated/unverifiedを注入 | 境界を跨ぐboundはexact再計数またはBudgetUnverified。manual/Emergencyでも迂回しない | F05,F24 | P6B |
| A05 | AutomationでHumanを失効させる候補 | 拒否しHuman原文は不変。正当なHuman後続撤回だけ失効可 | F03,F08 | P6B |
| A06 | 生成中のcontrol変更/履歴変更/通常queue追加 | control/履歴変更で古い候補拒否、pending queueだけなら誤staleなし | F13,F14,F23 | P6B |
| A07 | commit前後に接続を切り、同keyで再照会 | 同receiptを返し二重checkpointなし。成否不明ならrestart_required | F14,F17,F28 | P6B |
| A08 | length/EOF/二重終端/途中Tool引数 | Toolを一件も実行せずtyped失敗。provisionalをfinalにしない | F15,F21 | P6B |
| A09 | 同Model response/toolを再配信 | 同Actionを返す。同key異argsはconflict。Actionless経路なし | F16,F22 | P6B |
| A10 | 副作用後・結果保存前にkill | 照会不能ならEFFECT_OUTCOME_UNKNOWN。Tool再実行0 | F16,F17,F29 | P6B |
| A11 | 有効checkpointからprocessを再起動 | 原文/参照が同一、新Run/Trace、旧Runの改名なし | F02,F17,F22 | P6B |
| A12 | dispatch-startの前後でcancelを競合 | 前ならdispatch0、後なら開始済みとして停止/照会、取消済み偽装なし | F16,F23,F29 | P6B |
| A13 | UTF-8境界違反/大range/改ざん/別principal | INVALID_RANGE/Forbidden/Integrityを区別。許可rangeだけ返す | F11,F18 | P6B |
| A14 | step/deadline上限に達する | incomplete、保存済み進捗、resumable根拠。nil errorだけで完了扱いしない | F19,F20 | P6B |
| A15 | 最終本文と未実施/失敗の検査Evidence | Run終了とverificationを別表示。COREは証拠なしに親Task成功へしない | F10,F20,F26 | P6B |
| A16 | Ubuntu/Windows/macOS native artifact | CORE/Python/Nodeの必須起動なし。同じAPI/エラー/保存意味。GPU optionalとは分離 | F21,F28,F29,F30 | P6B |
| A17 | 同Thread/workspaceへ2process、遅着旧fence | 二重driver/変更を拒否。OS lock不取得時にTTLだけで乗取りしない | F02,F14,F16,F22 | P6B |
| A18 | secret/raw本文を含む失敗入力 | publicログに本文/keyなし。private Evidenceへ権限付きで保存 | F15,F18,F21,F28 | P6B |
| A19 | CORE未起動・CORE DB未配置でcoding | CLIで読取/編集/実テスト/保存/圧縮/resumeが成立。CORE process起動0 | F21,F22,F24,F28,F30 | P6B |
| A20 | 同じsynthetic課題をCLIとCOREで実行 | 同じServiceとKernel、同じ権限・終端・Compaction・保存契約 | F21,F22,F26 | P6B |
| A21 | 全methodの正例/未知field/誤type/unsupported version | 16 methodすべて厳密型で受理/拒否。Compact/Resume型の未定義なし | F01,F21 | P6B |
| A22 | 5namespaceと未提示/欠番/他stage handle | 配列順とSourceRefを一致。無効handle拒否。前回summaryは最大1 | F07,F09,F10 | P6B |
| A23 | Aを実装→Aはやめて、Bなし | revocationを有効な根拠として扱い、Aだけ失効、撤回文は保持 | F03,F07,F08 | P6B |
| A24 | verified最低候補が上限を超過 | CapacityBlocked直行、Summary生成0、Emergency呼出し0。Selection既実行回数と区別 | F05,F12,F19 | P6B |
| A25 | tools/role/bos等を含む実Backend prompt | measureと実prompt usageの定義を一致。declared値を実測へ昇格しない | F05,F24 | P6B |
| A26 | 実Chat strict経路へraw tool markup/reasoning-only | schema不合格をcommitしない。hidden retry/no-thinking変更0、receiptあり | F09,F10,F15,F24 | P6B |
| A27 | oversize/depth/重複key/batch/不正UTF-8 | parseで拒否。生成/Tool/保存mutationなし | F01,F21 | P6B |
| A28 | sealed raw、checkpoint、eventをUPDATE/DELETE | DB制約とdomain validatorが拒否。CURRENT pointerを単独変更しない | F02,F14,F28 | P6B |
| A29 | measure後にtemplate/Model/profile変更 | BINDING_CHANGED、古いcountで生成0。再measure有限、別Model fallbackなし | F05,F15,F24 | P6B |
| A30 | next_step/interrupt_current/next_turnを各競合点で受付 | queue/control/applicationを区別、入力を一回だけ適用、停止要求をordinary queueに埋めない | F03,F23 | P6B |
| A31 | Shiro keywordとnested subagentを含む委譲 | 新profileはnative clientへ到達し、旧Codex/toolloop/Actionless経路へ戻らない | F16,F26 | P6B |
| A32 | backup後にfile変更してからDB restore | 変更をDB復元で取り消したと見なさず、照会根拠なしに再実行しない | F17,F28,F29 | P6B |
| A33 | untrusted path、prompt内権限上書き、巨大skill | scope/信頼/sizeで拒否またはdata扱い。権限/Humanへの昇格なし | F03,F04,F27 | P6B |
| A34 | hash不一致/非一意match/symlink/reparse/case差 | 曖昧編集0。三OSのcontainment差を検査。外部writer完全CASは広告しない | F16,F29,F30 | P6B |
| A35 | isolated未配置で実行、trusted_hostを選択 | isolatedはunavailable、hostへの暗黙降格0。hostはOS隔離なしと表示 | F16,F30 | P6B |
| A36 | 有効checkpoint、active Tool、別ACL、file状態差 | 安全な履歴forkのみ、active効果copyなし、filesystem復元を偽装しない | F02,F18,F25 | P6B |
| A37 | 停止した旧threadのexport/absolute path/origin/opaque | 旧原本不変、コピー先隔離、未対応形式fail closed。新runtimeへ旧parser常駐なし | F02,F17,F28 | P7_when_requested |
| A38 | outbound queue飽和中にcancel/最終結果 | 制御優先、progress gapを明示、確定Eventは再取得可。二重実行なし | F20,F21,F23 | P6B |
| A39 | 出力上限/途中killでraw捕捉が欠落 | capture_complete=false、全文completion linkなし、metrics0補完なし | F07,F11,F16,F18 | P6B |
| A40 | CLIのみ/COREのみ/計数なしの実装を判定 | P6不合格。両入口と初回binding計数/Compaction/resumeの証拠が必須 | F20,F21,F24,F26,F30 | P6B |
| A41 | 初回actがreasoning-only、2回目成功/失敗の両方 | 同Action、新Attempt/Request、最大2回。3回目なし。失敗本文はContextへ入らない | F15,F31,F19 | P6B |
| A42 | terminal_output_once許可/不許可、対応/非対応、Qwenの形式補正 | 許可された変換のみ、新digestを計数し、同物理Modelを維持。適用内容receiptが一致 | F15,F24,F31,F30 | P6B |
| A43 | backoff中cancel/revoke/unknown、別binding、refusal | 新生成なし、typed結果。Unknownを未開始と見なさない | F19,F23,F31 | P6B |
| A44 | Step10、generation capの境界、retry待ちがdeadlineを超過 | 各生成予約を計上。retryはStepを増やさず試行枠を消費。cap枯渇を新Actionで回避しない | F22,F31,F35 | P6B |
| A45 | Selection/Summaryが空/不正出力。act回復profileが設定済み | Selection最大1+Summary最大1。失敗時追加生成0のEmergency、Preflight no-fitは直接Blocked | F09,F10,F12,F31 | P6B |
| A46 | 同本文/改行差/Unicode差/source差、CORE五区分・flatten経路 | 決定的revision、内部digest別、SourceRef hashと引用hashを混同しない | F04,F26,F32 | P6B |
| A47 | limits省略/過大/新deadline、同key異limits、新Run生成 | 省略と過大を拒否。新予算をreceipt返却、旧Run実績保持、再送で二重Runなし | F17,F22,F35 | P6B |
| A48 | valid/tampered/different audience/thread/key/expired/replayed proof | raw照合、定時間MAC検査、nonce一意。同key確定receiptは期限後も再取得可 | F03,F33,F34 | P6B |
| A49 | 対話/exec human明示/pipe/script、payload human claim、user_message混入 | 全入力がimmutable receipt。pipeを設定だけでHumanにしない。StartInput user blockを拒否 | F03,F04,F22,F34 | P6B |
| A50 | 既存full-chat measure APIか同一engine前処理、cold/hot/日本語/Tool/recovery | 非生成で同template/特殊tokenまで一致。別手書きtemplate・経験比率をexactにしない | F05,F24,F30 | P6B |
| A51 | call=2048/output=2048と片方2049、日本語UTF8、partial | 両2048はサイズ条件を通る。片方2049は拒否。合計2048へ誤制限しない。意味完了は別検査 | F07,F12 | P6B |
| A52 | strict receiptのbackend_attempts 0/1/null/2、通信結果不明 | 2を契約違反拒否。0/1/nullとgeneration_state整合。unknownを0へしない | F15,F17,F19,F31 | P6B |
| A53 | 実CORE五区分/revision/原文relay/子Taskとstandalone同課題 | producerとconsumerの両側を実装、旧Codex/Actionlessへ戻らず、CORE停止中もCLI成立 | F26,F32,F33,F34 | P6B |
| A54 | Switch参照9331c99bとEcoSystem16ea1009、実機source未確認 | 別recordに固定。参考を最新配備と偽装せず、旧Switchを自動更新しない | F30,F36 | P6B |
| A55 | metadata key/数値表現/null/回復profileを変更 | 宣言field集合だけCJ1+LP、expected echoと二重正規化なし | F15,F24,F37 | P6B |
| A56 | weights/template/profile/limit変更と同一profile再読 | 変化時拒否、同条件で安定、client再構成なし | F24,F37 | P6B |
| A57 | 旧codeとstrict code、停止証拠あり/なし | first-output unknownとDEGENERATE no retryを区別 | F19,F31 | P6B |
| A58 | 同RPC keyでmethod/proof/limits/null/順序変更 | 同義のwire順は同hash、意味変更はconflict | F01,F21,F34,F37 | P6B |
| A59 | 五区分、空白/改行、single_leading、Tool対 | 原文をtrimせずprefixだけcoalesce、後続instruction拒否 | F04,F15,F37 | P6B |
| A60 | checkpoint reload→次act、旧Summary+retained+tail+ref | 同golden、obsoleteなし、refでEvidence取得可能 | F02,F04,F13,F18,F37 | P6B |
| A61 | 5namespace、8000byte+日本語境界、2KiB excerpt | schemaとbyte条件、再連結で原文、段要求数不変 | F07,F09,F37 | P6B |
| A62 | 古いexcerpt/full、marker長さ、bytes減token増 | 一定順で全適格置換、全体verified tokenで採否 | F11,F12,F13 | P6B |
| A63 | 未登録/曖昧profile、scope escape、env継承 | closed registry、scope/limit交差、fail closed | F16,F27,F39 | P6B |
| A64 | AGENTS祖先/trust/size、SKILL YAMLとhook | 限定探索、bodyはEvidence load、hookで権限昇格なし | F18,F29,F39 | P6B |
| A65 | prefix/CJ/float/metadata/count/DB revisionを改変 | 正確なBLOB/hash、採用前後revisionを区別、拒否境界 | F02,F13,F14,F37 | P6B |
| A66 | 各type positive/unknown field/DB再取得 | Event.payloadとpayload_json一致、raw公開なし | F23,F38 | P6B |
| A67 | hex/CRLF/base64/別principal/ACL順変更 | 唯一形式、profile集合順の正規化、鍵情報をhashへ入れない | F03,F33,F34,F37,F39 | P6B |
| A68 | import main path、template commit未置換 | 正しいmodule/binary、未知pinの配備拒否 | F30,F36 | P6B |
| A69 | 旧keyword一致/不一致、Subagent有/無、native失敗 | 選択profileは全旧分岐を迂回、非対象は維持 | F26,F30 | P6B |
| A70 | args分割、id/index違反、terminal/DONE欠落 | 終端前Tool0、unknown/length/refusedを分離 | F15,F16,F31,F40 | P6B |
| A71 | 初回+retry、同操作候補再検査 | 各Attempt計数+生成、計数が生成/KVを起こさない | F05,F15,F24,F31 | P6B |

## 関数

| ID | 関数 | Phase | 試験 |
|---|---|---|---|
| F01 | ValidateRunInput | P1 | H02,H22,A01,A02,A21,A27,A58 |
| F02 | LoadSnapshot | P2 | H03,H11,H25,A11,A17,A28,A36,A37,A60,A65 |
| F03 | ClassifyOrigin | P1 | H14,H17,A05,A23,A30,A33,A48,A49,A67 |
| F04 | AssembleContext | P1 | H10,H14,H16,A03,A33,A46,A49,A59,A60 |
| F05 | EstimateBudget | P3 | H05,H07,H16,H24,A04,A24,A25,A29,A50,A71 |
| F06 | PrepareSources | P4 | H06,H09,H23,H24 |
| F07 | BuildSelectionDataset | P4 | H09,H18,A22,A23,A39,A51,A61 |
| F08 | ApplySelection | P4 | H02,H04,H06,H14,H17,A05,A23 |
| F09 | BuildSummaryInput | P4 | H01,H04,H10,H12,H15,H16,H18,A22,A26,A45,A61 |
| F10 | ValidateSummary | P4 | H01,H04,H08,H10,H12,H15,H21,A15,A22,A26,A45 |
| F11 | ProjectObservation | P4 | H07,H19,H23,H24,A13,A39,A62 |
| F12 | Emergency | P4 | H07,H12,H19,H26,A24,A45,A51,A62 |
| F13 | ValidateCandidate | P4 | H03,H06,H11,H13,H19,A06,A60,A62,A65 |
| F14 | CommitCheckpoint | P4 | H11,H13,H25,A06,A07,A17,A28,A65 |
| F15 | InvokeModel | P3 | H01,H02,H05,H18,H21,A08,A18,A26,A29,A41,A42,A52,A55,A59,A70,A71 |
| F16 | DispatchTool | P2 | A02,A09,A10,A12,A17,A31,A34,A35,A39,A63,A70 |
| F17 | ReconcileRun | P2 | H03,H11,H22,H26,A01,A07,A10,A11,A32,A37,A47,A52 |
| F18 | ReadObservation | P2 | H23,A13,A18,A36,A39,A60,A64 |
| F19 | ClassifyFailure | P1 | H05,H20,H25,H26,A14,A24,A41,A43,A52,A57 |
| F20 | BuildRunResult | P2 | H08,H15,H20,A14,A15,A38,A40 |
| F21 | ServeNative | P2 | A08,A16,A18,A19,A20,A21,A27,A38,A40,A58 |
| F22 | AdmitTask | P2 | A01,A09,A11,A17,A19,A20,A44,A47,A49 |
| F23 | ApplyQueuedInput | P2 | A06,A12,A30,A38,A43,A66 |
| F24 | MeasureFinalPrompt | P3 | A04,A19,A25,A26,A29,A40,A42,A50,A55,A56,A71 |
| F25 | ForkSession | P4 | A36 |
| F26 | DelegateFromCore | P5 | A03,A15,A20,A31,A40,A46,A53,A69 |
| F27 | LoadTrustedExtensions | P2 | A33,A63 |
| F28 | MigrateAndBackup | P2 | A07,A16,A18,A19,A28,A32,A37 |
| F29 | ReconcileProcess | P2 | A10,A12,A16,A32,A34,A64 |
| F30 | ValidateDeployment | P1 | A02,A16,A19,A34,A35,A40,A42,A50,A54,A68,A69 |
| F31 | PlanModelRetry | P3 | A41,A42,A43,A44,A45,A52,A57,A70,A71 |
| F32 | MaterializeContextRevision | P1 | A46,A53 |
| F33 | SignOriginProof | P5 | A48,A53,A67 |
| F34 | PersistIntake | P2 | A48,A49,A53,A58,A67 |
| F35 | ResolveResumeLimits | P2 | A44,A47 |
| F36 | VerifyReleaseSources | P0 | A54,A68 |
| F37 | EncodeCanonicalContract | P1 | A55,A56,A58,A59,A60,A61,A65,A67 |
| F38 | BuildTypedEvent | P2 | A66 |
| F39 | LoadHostAssets | P2 | A63,A64,A67 |
| F40 | AssembleStrictStream | P3 | A70 |
