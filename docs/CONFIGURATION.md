# 初回配置・設定契約

本書はRH-IMPL-001の付属仕様。全fieldは新設する実装契約。JSONのみを初回形式とし、未知key、重複key、不正UTF-8/不正surrogateを拒否する。環境変数の暗黙展開はしない。秘密は許可されたfile参照で解決し、Model/Tool引数へ複製しない。

## 必須field

| field | 意味 |
|---|---|
| config_version | rencrow-harness-config/v1 |
| data_root | 初期化済みstoreの絶対path。CORE DBではない |
| gateway.base_url | 初回はlocalhost/loopbackの既存RenCrow_LLM Gateway。物理Backendは禁止 |
| gateway.required_contract | harness-v1 |
| caller.principal | 起動profileに固定した主体。payloadから上書き不可 |
| caller.default_origin | human / automation。profileの許可上限。CLI receiptへ記録し、pipeやCORE生成文をこれだけでHumanにしない |
| caller.readable_session_owners | 読取可能owner。空をallow-allと解釈しない |
| caller.controllable_session_owners | 停止/再開可能owner。lock取得条件は別 |
| caller.relay_issuers | issuer/key_id/audience/key_fileのallowlist。未設定ならHuman relayを受けない。署名条件はCORE_INTEGRATION |
| workspaces | 許可された絶対root、実行mode、policy_ref |
| bindings | CLI/COREが選べるprofile名→Binding（PROTOCOL定義） |
| process_profiles | name、executable絶対path、shell明示、固定argv_prefix。trusted_hostでだけ使用 |
| limits | 全体deadline、step、1step tools、capture、max_generation_attemptsの上限。Start/Resumeは全部指定し、上限超過を拒否 |
| recovery | act-recovery/v1、max_attempts_per_act=2、許可profile集合。Run受付時に固定し、Modelが上書きしない |
| compaction.enabled | trueでもLLM strict/count未成立なら当該bindingはunavailable |
| compaction.trigger_ratio | 初期0.85、0より大きく1未満。実測値ではない |
| compaction.safety_margin_tokens | 非負の余裕。LLMのcountには含めずfit計算で一度だけ差引く |
| compaction.max_logical_stage_requests | 2固定 |
| storage.max_database_bytes | storeのquota。超過時は停止、原本を自動削除しない |
| storage.backup_root | 別媒体等、明示された絶対path |

Binding.profile_revisionはexecution selection profileのversionを示す。aliasではGateway Role profileと一致させる。model_routeではhostが管理する明示選択profileのrevisionを持ち、実際のModel/template/tokenizerのrevisionはmeasureのbinding_fingerprintで別に固定する。どちらもModel名の推測で解決しない。

初回はGatewayへのloopback接続を標準とする。Gatewayから各GPU hostのRuntimeへは既存認証付き経路を用いる。remote Gatewayを直接公開する利用はTLS/client認証とCaller contractの追加後に別profileとして有効化する。

## 起動検証

config/credential fileとdata_rootはAgentが通常編集するworkspaceの外に置く。これらのpathsへのfile Tool操作は常に拒否する。trusted_hostの任意processは完全隔離されていないため、同一OS userの悪意から守れるとは主張しない。

data_rootの存在、owner permission/ACL、local filesystem、schema version、quota、backup設定を検査する。未知schemaならmigration commandによる明示処理を要求し、起動時に黙って破壊的変換しない。

Gateway不在でもinspect/原本読取は利用可能。生成capabilityだけunavailableにする。strict/count未実装のbindingをreadyと表示しない。一方、それらを実装せずCLIも生成できない状態を初回release完了にはしない。

## Policyの最小実装

policy_refはhost管理registryから解決し、path、Tool名、実行mode、process profile、時間/出力上限を含む。structured_onlyはprocess.execを非公開/拒否。trusted_hostは許可された実行fileとargv_prefixに限定するが、そのprogramの全振る舞いをsandboxしたとは言わない。isolatedは対応するadapter evidenceがない限りunavailable。

Policyには任意の`verification`固定planを置ける。fieldとgrant条件は[HOST_ASSETS §1](HOST_ASSETS.md#1-policy_ref-registry)を正本とし、registry schemaの未知fieldは拒否する。planなしのpolicyは従来どおりで、effective policy revisionの既存canonical bytesも変わらない。

`rencrow-harness verification-digest --config <absolute-path> --policy-ref <id> --workspace <absolute-path> --mode trusted_host`は、実効planの`criteria_revision`だけをJSONで出力する。owner CLIはDBを開かず、argv・environment値・workspace pathを出力しない。COREはTask admission時にこの値を固定し、RunResultの実測criteriaと照合する。現行host設定から古いTaskの期待値を自動更新しない。

ren operatorがCORE由来のSessionを引き継ぐ場合、controllable_session_ownersにそのownerを明記し、旧clientがwriterを解放するまでBUSY。所有不明processのkillやTTLだけの乗取りをしない。

OS固有supervisorは標準runtimeの必須ではない。COREが子processを起動する場合は固定binary/config pathを使い、shell文字列で組み立てない。常駐service化をするdeploymentでは既存の予約PORT/owner/supervisor規約を適用するが、stdio標準自体はlistener PORTを持たない。

## 再試行と再開の設定解決

既定例のmax_model_steps=10、max_generation_attempts=32、deadline_seconds=1800は設計上の初期値で、実測最適値ではない。actの1retry、Selection/Summary、stale後の再試行もgeneration_attemptsへ計上する。初回Qwenの受入profileはsame_requestとterminal_output_onceを許す。Model/route/temperatureを変えるprofileは初回に追加しない。

Runのrecovery_policy_revisionはb"rencrow-recovery-policy/v1\0"に、LP(contract_version)、LP(decimal(max_attempts_per_act))、辞書順に整列したallowed_profiles各LPを連結したSHA-256とする。policy snapshotはruns.recovery_policy_jsonへ保存する。これとLLM側のprofile_revisionは別の版である。

Resumeでは保存limitsをCLIの初期表示に使ってよいが、送信前に現在上限と突き合わせ、完全なlimitsを必ず送る。新Runは新deadlineから始まる。旧Runの期限・消費を黙って消さない。

## v0.2.2追加fieldと参照解決

`policy_registry_path`（workspace外の絶対JSON path）、`env_profiles`（name/values配列、clean必須）、`extensions`（enabled/trusted_workspace_roots/skill_roots/hooks）を必須にする。未知keyは拒否し、詳細はHOST_ASSETS。policy_refを曖昧な別設定系へ委ねない。

鍵形式/principal/caller_profile_digestはBYTE_CONTRACTS§5、policy_revisionはHOST_ASSETS§1。モデル生成optionsはLLMのBindingDescriptor.stage_optionsから取得する。configに同名のモデル固有optionを重複追加しない。

