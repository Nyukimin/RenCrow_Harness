# Host設定・Policy・拡張資産

RH-ASSETS-001 / v0.2.2 / 設計: ルミナ / F27・F29・F39
以下は実装裁量にしない外部/保存契約。Goのprivate struct配置やhelper名はClaudeが選べる。

## 1. policy_ref registry

config.policy_registry_pathはworkspace外の絶対pathのJSONファイル。環境変数展開、相対path、ネットワークURLを禁止。schemaはpolicy_registry.schema.json。

root={format_version:"rencrow-policy-registry/v1",policies:[Policy]}。
Policyの必須fieldはid、allowed_modes、tools、read_prefixes、write_prefixes、process_profiles、env_profiles、limits。idは重複不可。policy_refはこのidへの完全一致で、ファイル名や任意URLとして解釈しない。未登録は起動/Run受付で拒否する。

prefixはworkspace相対の正規化segment列で、`.`はworkspace全域、空配列は一切許さない。絶対path、`..`、NUL、空segmentを拒否し、判定では実pathのworkspace containmentに加えsegment単位のprefixを照合する。大文字小文字、symlink/junction/reparseの処理はOS adapterで検証し、文字列startsWithだけで判定しない。read/search/create/editはそれぞれread/write prefixとTool許可を両方要する。

modeはSession要求∩workspace.allowed_modes∩policy.allowed_modes。拒否のとき別modeへfallbackしない。structured_onlyではprocess.execをcatalogから除外し、呼ばれても拒否。trusted_hostはOS sandboxではない。isolatedは実adapter受入までunavailable。

process.execはpolicyが許すprocess profileの中から、**同じ絶対executable、同じis_shell運用、argvが完全一致prefixを持つもの一件**へ解決する。0件拒否、複数件はPOLICY_AMBIGUOUS。argvを勝手に追加/並替しない。profileのargv_prefixは要求argvの先頭条件であり、Hostがもう一度prependする値ではない。shell解釈はis_shell=trueを明示したprofileだけ。

env_profile_refはconfig.env_profilesのnameへ完全一致。valuesは明示的な文字列mapで、ホスト環境を全継承しない。`clean`という空profileを必須にする。WindowsのSystemRoot等が必要ならinit時に確認した値を特定profileへ明示保存する。secret/tokenをenvへコピーしない。未知profileへのclean fallbackも禁止（引数が必須）。

実効limitsはhost config上限とpolicy上限の小さい方を上限とし、Start/Resumeの具体値が超えたら拒否。policy snapshotに解決したprocess/env定義も含める。
`policy_revision=D("rencrow-effective-policy/v1",{policy,process_profiles,env_profiles,workspace_root,mode})`。
process/env配列はnameのUTF-8順、policyの集合配列は重複を拒否してUTF-8順。prefix/Tool/許可profileの順序に権限優先順位を持たせない。Run中にファイルを再読して権限を緩めず、変更は明示した再開/制御更新にする。

## 2. AGENTS.mdの探索

config.extensions.enabled=trueかつworkspaceの実rootがextensions.trusted_workspace_rootsに**完全一致**した場合だけinstruction資産として探索する。`.git`の有無やファイルが存在することはtrustの証拠にしない。falseの場合、通常file.readでdataとして読める範囲は残すが、system/developerへ自動昇格しない。

探索はworkspace rootから作業cwdまでの祖先chainにある`AGENTS.md`だけ。rootの上へ歩かず、別workspace、HOME、ネットワーク、CLAUDE.md等を自動探索しない。初回は再帰的な全tree探索とJIT子directory規則の自動ロードをしない。必要な子directoryが変わる次Runでは新しいsnapshotを作る。

rootから深いdirectoryの順、1file<=32KiB、合計<=128KiB、UTF-8/最大path4096。超過はCONTEXT_ASSET_TOO_LARGEで対象Run受付を拒否し、無言truncateしない。fileとcontext snapshotのhashを保存する。指示として使う本文は`stable_runtime_context`のscope付きHost block（JSON envelope）にする。scopeより外へ適用せず、Humanやhost policyを上書きする権限は与えない。実行中にModelが編集したAGENTS.mdは同Runへ自動reloadしない。

## 3. SKILL.mdの探索と明示load

探索rootは、trusted workspaceの`.agents/skills`と、config.extensions.skill_rootsの明示された絶対path。後者もhostが許可したprivate/managed rootでありworkspace外を勝手に走査しない。各root直下の1directory内の`SKILL.md`だけ。symlink escape拒否、UTF-8 path辞書順、重複nameは曖昧としてそのcatalogを拒否。

1file最大64KiB、frontmatter最大8KiB、最大128skill。先頭行`---`、閉じ行`---`、間はYAML mapping。required name（ASCII小文字・数字・単一hyphen、1〜64、先頭末尾hyphen/連続hyphen不可）、description（1〜1024 UTF-8 bytes）。optionalはlicense/compatibility各<=1024 bytes、metadataは文字列→文字列map最大32要素。未知key、duplicate、alias/anchor/tag/merge、多document、非string scalarを拒否する。gopkg.in/yaml.v3等の既存採用libraryを使ってもこのsubset検査を省略しない。全Agent Skills仕様互換とは広告しない。

発見時は安全上限までfileを読み取り、検証した全bytesをsealed Evidenceへ保存するが、Modelへは{name,description,evidence_id,projection_version:"text/v1",total_bytes}のmetadata catalogだけをcontext dataとして渡す。Modelが必要なskillを**evidence.readで明示取得**する。新しいskill Toolや任意スクリプト実行は追加しない。本文内のscript linkは手順dataで、process権限を付与しない。これで既存6Toolのまま遅延prompt投入を成立させる。

## 4. 固定hook

初回のhookは**製品内の固定callback**で、外部command/script/ネットワーク/任意DLLを読み込まない。config.extensions.hooksは[]または["audit_metadata"]のみ。空でもKernel安全処理は省略しない。

HookPoint一覧: before_model、after_model、before_tool、after_tool、before_compact、after_compact、run_terminal。
HookInputの全field: hook、thread_id、run_id、action_id|null、stage|null、context_revision、control_revision、evidence_ids[]、code|null。本文/credential/raw argsを含めない。
HookResult={decision:"continue"|"deny",code:string|null}。

before_model/before_tool/before_compactだけdeny可能。denyは同期blocked/HOST_HOOK_DENIED。after系/run_terminalはcontinueのみ、完了済み副作用を否定/巻戻したことにしない。audit_metadataは全点continueのみで、私有receiptへmetadataを記録する。callbackは通常10msの設計予算、実I/Oの無限待ち禁止。予算超過/不正resultはbefore系でblocked/HOST_HOOK_FAILED、after系は確定事実を保持し診断を残す。Goのthreadを強制killしたと偽装せず、純粋/有限callback以外を登録しない。

hookはPrompt、Tool args、Model、policy、checkpoint候補を変更できない。外部hookが必要になった場合は別契約。CLIのフック名を別vendorから丸ごと互換実装する作業は今回行わない。

## 5. 鍵とcaller

鍵file形式、principal、caller_profile_digestはBYTE_CONTRACTS§5を参照する。CORE issuer設定は{issuer,key_id,audience,key_file,ttl_seconds}。recipient profileのallowlistとの整合を起動時に検査する。key_idの使い回しで鍵を変更しない。機密値とfixture keyをconfig例から実機へコピーしない。

HookInput/HookResultとSkillMetadataの機械可読型は`schemas/host_assets.schema.json`。YAMLのduplicate/anchor禁止やUTF-8 byte上限、hookごとのdeny可否はschemaに加えてloaderで検査する。metadataのkey/valueも各1,024 UTF-8 bytes以下。
