# byte・digest・serialization契約

RH-BYTES-001 / v0.2.2 / 設計決定: ルミナ / 実装: Claude
この文書は新設契約。既存Gateway、CORE、Codexがこの表現を使用するという主張ではない。
`checks/contract_codec.py`は合成fixture用参照実装であり製品依存ではない。Go実装を同じベクトルへ独立に合わせる。

## 1. 共通のCJ1とLP

`LP(b)=uint64_big_endian(len(b))+b`。文字列のbはUTF-8。長さは文字数でなくbyte数。
`D(domain,v1,...,vn)=lowercase_hex(SHA256(LP(UTF8(domain))+LP(CJ1(v1))+...+LP(CJ1(vn))))`。
このDではdomain末尾にNULを追加しない。既存Context revision/HMAC/recovery_policy_revisionは以前の専用式を維持し、Dへ置換しない。

CJ1はJSON値を以下の一意なUTF-8 JSONへ直列化する。RFC 8785への準拠は主張しない。

1. object keyは**復号後のkeyのUTF-8 byte列の辞書順**。全階層へ適用。array順序は保持。object/arrayは空でも`{}`/`[]`を出す。nullは`null`。未指定keyとnullは別であり、schemaのrequiredを満たさない要求はhash前に拒否する。
2. key/valueの文字列はUnicode scalarだけ。BOM、不正UTF-8、unpaired surrogate、復号後に重複するkeyを拒否する。Unicode正規化、改行変換、trimはしない。
3. `"`は`\"`、`\`は`\\`、U+0000〜001Fは必ず小文字hexの6byte表記`\u00xx`。`\n`/`\t`等の短縮表記はCJ1出力に使わない。その他はUTF-8のまま。`/ < > & U+2028 U+2029`をescapeしない。JSON token間の空白と末尾改行は0。
4. 数値は入力の10進lexemeを正確に解析する。float64へ丸めない。指数を展開した通常10進表記にし、小数末尾0と不要な小数点を除く。`1`/`1.0`/`1e0`は`1`、`-0`/`-0.0`は`0`、`1e-3`は`0.001`。NaN/Infinityは拒否。整数fieldは整数性とschema上限を別途検査する。
5. 数値lexeme最大1,024 ASCII bytes、入力指数および小数桁を含めて正規化した10進指数の絶対値、展開後文字列は最大4,096。深さ最大64、全体は各transport/資産のbyte上限。展開前に上限を調べ、巨大指数をメモリへ展開してから拒否しない。
6. JSON文字列として受け取ったTool `function.arguments`の内側を勝手にcanonicalizeしない。それはまず一つの文字列。完全JSONとしての検証は別途行う。schemaの数値制約も欠かさない。

Goではjson.Decoder.UseNumberに加えてduplicate-key/UTF-8/depth検査を行う。encoding/jsonの既定marshalだけをCJ1とは呼ばない。map順が一致してもescape/数値の規則が異なり得る。scope内の正規化は常に同一版。

## 2. Model入力の二つのdigest

### 2.1 input_digest: HarnessとGatewayが照合する論理入力

`input_digest=D("rencrow-model-input/v1", LogicalInput)`。
LogicalInputの**全field集合**は以下。元要求は`llm_contract.schema.json#/$defs/ChatRequest`を満たし、列挙外のtop-level/metadata fieldは拒否する。

```text
model, messages, tools, tool_choice, response_format,
stream, stream_options, max_tokens, temperature, top_p, seed, stop,
routing={agent_id,execution_role,execution_alias},
harness={contract_version,stage,max_backend_attempts,
         recovery={profile_id,profile_revision}}
```

すべて送る。nullable生成optionはnullで「固定済み基本profile値を利用」を指定し、具体値の解決はRuntime。`stop=[]`は追加stopなし。`tools=[]`、`content:null`等は別値のまま。stream=trueならstream_options={include_usage:true}、falseならnull。

routingの3fieldはrencrowから取り、model_routeではnullを許す。運用metadata `request_id/trace_id/task_id/session_id/initiator/caller/purpose`、全`expected_*`、`retry_of_request_id/trigger_code`はLogicalInputから除外する。これらをModelへ渡したり前処理の選択へ使ったりしない。stage/profileは**含める**。回復profile選択変更、出力上限、Tool schema、履歴原文が変われば入力digestが変わる。

Harnessはmeasure前にinput_digestを計算する。measure応答のinput_digestが一致しない場合、生成しない。generateに`expected_input_digest`を設定し、Gatewayは元Chat要求から再計算して一致を検査する。単なる「要求を送った先が同じ」という照合ではない。

### 2.2 request_digest: Runtimeが所有する最終入力

`request_digest=D("rencrow-model-request/v1", NormalizedRequest)`。
NormalizedRequestの形はschemaの同名型に固定する。

```text
format_version="rencrow-normalized-request/v1"
backend_api="chat/completions"
payload=<physical model、最終messages/tools/optionsを含む、engineへ渡す完全なJSON object>
runtime_overrides=<HTTP payload外でtoken列/生成を変える実効optionのobject。なければ{}>
reserved_output_tokens=<有効max_tokens>
effective_recovery={profile_id,profile_revision,applied_transformations}
```

payloadから`rencrow`、認証、transport相関ID、expected値を除く。engineへ実際に送る残りのfieldは**すべて**含める。宣言されていない隠れた生成optionを追加してはならない。Runtimeの内部context/cancellation handle等、Modelに影響しない実行資源は含めない。デフォルトは共用の前処理で解決し、payloadまたはruntime_overridesへ実効値を入れる。max_tokensを黙ってclampせず、stage descriptorの上限を超す要求は生成前拒否。

Gateway→Runtime→Backendで複数の前処理がある場合、全変換後のNormalizedRequestをRuntimeが確定し、measureとgenerationの両方が同じ関数を使う。generation時は計数時のexpected_request_digestと比較する。input_digestとrequest_digestを等しいと仮定しない。

**Harnessはモデル固有変換を再実装してrequest_digestを計算しない。** Runtimeが返した値を、input_digest・binding_fingerprintと組にして保持し、次のgenerateでechoする。Gateway内でさらに前処理があるなら、それもRuntimeの共通処理から観測できる最終値に含める。これにより両側が独自にQwen templateを複製する構成を避ける。

measure要求外側のsafety_margin_tokensは生成内容を変えず、両digestに含めない。BudgetReportに保存しfit判定へ一度だけ利用する。計数と実行で値が変わったらHostが再判定する。

## 3. binding_fingerprintはLLM側のopaque値

外部契約は「等しいか比較し、measure→generate→receiptでechoする値」。Harness/COREは再構成・分解しない。初回LLM実装は`"bfp-v1:"+D("rencrow-binding/v1", IdentityDescriptor)`に固定する。外部clientはこの生成形を依存契約にしない。

IdentityDescriptorの必須field: model_weights_revision、backend_build_revision、tokenizer_revision、template_revision、normalization_revision、base_profile_revision、effective_context_limit。model_weights_revisionにはmodel identityとweights manifestのdigestを含める。stage defaults・回復profile定義の変更はbase_profile_revisionの変更で捕捉する。回復profileを要求ごとに**選ぶ**だけならfingerprintは同じ。

取得元はRuntimeのloaded inventoryとprofile ownerが配備時に確定したmanifest。weights/tokenizer/templateは、実際にloadするfilesまたはartifactのcontent digestのmanifestから得る。mtime、path文字列、model名、token数広告だけをcontent revisionとしない。毎要求でweightsを全読込するのではなく、load時のverified manifestを利用し、ファイル差替えはreloadとepoch更新を要する。epochが変わる場合、同じfileでもfingerprintを更新してよいが、古いmeasureを受理しない。

取得不能ならそのbindingはverified計数不可。適当なrevisionを発明しない。API token、URL、PID等は公開fingerprintの構成資料に出さない。nonceを毎呼出し変えて同じbindingの計数を常に無効化する実装にもしてはいけない。

## 4. mutation_payload_hash: native mutationの同一性

`mutation_payload_hash=D("rencrow-mutation/v1",principal,method,params_without_top_level_idempotency_key)`。
`idempotency payload digest`と呼んでいたものは、この同じ値。二種類を作らない。SQLite receipts.payload_hashとIntakeRecord.mutation_payload_hashに同値を置く。

principalは認証済みconnection profileの値。CORE clientは設定済みの同じprincipalを使って事前検査できるが、payload内の自己申告から取らない。methodは正確なAPI名。paramsはschema検査後の完全object、トップレベルidempotency_keyだけを除く。**OriginProof内のmutation_key/mac/nonce/期限、nullable field、limits、expected revisionは含む**。ネスト内の同名fieldを一括削除しない。

RPC id/jsonrpc、サーバ受付時刻、現在のpolicy snapshotは含めない。新規受付における権限は別途チェックする。同じkeyの再取得は、現在の認証/読取ACLを検査した後、過去receiptを調べ、変更前revisionやproof期限を再検査しない。キーは(principal,idempotency_key)で一意、methodもdigestに入るため別operationへの再利用はconflict。

最初の要求を保存して再送する。profileのscope変更後も同principal・同payloadのreceipt参照は許可範囲内で可能だが、新たな実行権を得ることはない。新proofで同じkeyを再送した場合はdigestが変わり拒否する。

## 5. caller_profile_digest・鍵・principal

principalの文法は`^[a-z][a-z0-9_-]{0,31}:[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`。例 user:ren / core:local。大文字小文字を区別しtrimしない。OS user名の自動転用やLLMによる発行は禁止。既存AgentIDと別の認証主体label。

`caller_profile_digest=D("rencrow-caller-profile/v1",C)`。
C={principal,default_origin,readable_session_owners,controllable_session_owners,relay_issuers}。
owner配列は重複を拒否しUTF-8辞書順、relayは{issuer,key_id,audience}だけを取り、同3値の重複を拒否してその順の辞書順。key_fileのpathと秘密鍵bytesは含めない。鍵を変更するときは必ずkey_idも変更する。profile変更とTool policy変更は別。実行に効くpolicy/workspace/envは別のpolicy_revisionで凍結する。

HMAC key_fileは**64文字の小文字hex（32 bytes）＋任意の末尾LF1個**。BOM、CRLF、空白、大文字、raw binary、base64は受理しない。曖昧な自動判別をしない。秘密ファイルはworkspace外、ownerだけ読取可能なpermission/ACL。API token等はこの鍵形式と混同しない。署名計算のMAC入力はCORE_INTEGRATIONの従来LP式を維持する。

## 6. checkpointとその他のdigest

### 6.1 verification criteria revision

`criteria_revision=lowercase_hex(SHA256(CJ1(criteria)))`。`criteria`は次の閉じたobjectで、値はeffective policyから解決したものを使う。

```json
{
  "format_version": "rencrow-verification-criteria/v1",
  "plan": {"format_version":"rencrow-verification-plan/v1","process_profile_ref":"…","executable":"…","argv":[],"cwd":"…","env_profile_ref":"…","timeout_seconds":1,"pass_condition":"exit_zero"},
  "process_profile": {"name":"…","executable":"…","is_shell":false,"argv_prefix":[]},
  "env_profile": {"name":"…","values":{}},
  "workspace_root": "…",
  "resolved_cwd": "…",
  "policy_ref": "…",
  "mode": "trusted_host",
  "policy_revision": "…"
}
```

Object keysをCJ1で直列化してからSHA-256する。argvの順序とenvironment key/valueは意味を保持する。`resolved_cwd`はreal workspace rootとplan cwdを結合し、path separatorを現在OSの形式にしたclean absolute pathである。`policy_revision`は`rencrow-effective-policy/v1`の現行式で、verification planがあるpolicyだけ`policy.verification` canonical objectを追加する。planなしのeffective policy objectは従来bytesを維持する。CLIやRPCはこのhashだけを公開し、argvやenvironment valueは公開しない。

candidate_bytes=`UTF8("rencrow-checkpoint-candidate/v1")+0x00+CJ1(CheckpointCandidate)`。
candidate_hash=lowercase_hex(SHA256(candidate_bytes))。改行やBOMなし。checkpoint payloadの中にcandidate_hash自体を含めない。再encodeした別bytesで保存せず、検証したbytesをそのままBLOBへ渡す。

snapshot_digest=`D("rencrow-context-snapshot/v1", {thread_id,context_revision,control_revision,writer_epoch,policy_revision,binding_revision,applied_sources,latest_durable_checkpoint_id,latest_semantic_checkpoint_id})`。applied_sourcesは適用順のSourceRef配列、checkpoint_idなしはnull。pending queueは含めずcontrol変更は含む。

raw_hash=SHA256(raw bytes)、projection_hash=SHA256(正規のtext projection bytes)、text_digest=SHA256(UTF8(text))は従来どおりprefixなし。用途を取り違えない。Tool args_hash=SHA256(CJ1(検証済みarguments object))。file.edit expected_hashはfileの**raw bytes**でありCJ1を適用しない。

この表にない内部ハッシュは目的別domainを内部モジュール内で固定してよい。ただしwire相互運用、保存互換、権限判断へ追加する場合は新規契約として設計照会する。

## 7. operationの適用範囲

mutation式はidempotency_keyを持つsession/open、session/fork、turn/start、input/append、turn/interrupt、run/resume、context/compactに適用する。service/shutdownはconnection/process lifecycleの冪等要求で、受理済みshutdown状態を再返却するだけ。新Task/Tool操作を作らず、native schemaどおりkeyは持たない。読取/initializeへmutation hashを強制しない。


expected_input_digest/expected_request_digest/expected_binding_fingerprintはmeasureでは省略可、generateでは全て必須。measure要求内へ以前のexpected値を付ける場合も照合を行い、不一致を黙って更新しない。最初のmeasureはexpected fieldを省略するのが既定。
