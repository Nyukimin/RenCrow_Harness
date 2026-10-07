# CORE・CLIから供給するContextと由来

RH-CORE-001 / v0.2.2 / ClaudeがCOREとHarnessの両側を実装する。
本書のrevision生成、HMAC issuer、intake receiptは新設契約。既存COREに実装済みとは扱わない。

## 1. ContextBlock生成

公開fieldはkind/text/revision/sourceの4個。公開schemaにdigest/originを追加しない。sourceがある場合のoriginはSourceRefの値であり、認証済みの本文であるという証明にはならない。

F32 MaterializeContextRevisionをCOREの`internal/adapter/nativeharnessclient/context.go`（新設）とCLIの入力準備に置く。共通の純粋関数は`pkg/protocol/context_revision.go`に置いてよい。CORE内domainのimportはしない。

revisionは次の内容addressとする。バージョン順序や本人認証を意味しない。sourceなしはNULLタグを使い、空文字と区別する。

```text
revision = "ctx-v1:" + hex(SHA256(
  b"rencrow-context-block/v1\0" + LP(kind) + LP(text) + SOURCE(source)
))
LP(s) = uint64_big_endian(len(UTF8(s))) + UTF8(s)
SOURCE(null) = 0x00
SOURCE(ref)  = 0x01 + LP(owner) + LP(source_id) + LP(raw_hash)
             + LP(projection_version) + LP(decimal(range.start))
             + LP(decimal(range.end)) + LP(origin) + LP(decimal(sequence))
```

textの空白・改行・Unicodeはそのまま。request_id、受付時刻、進捗をCharacter/Stableへ混ぜない。Variableは必要時に本文が変わり、当然revisionも変わる。同一kind/text/sourceなら同じrevision。出所やscopeが変わる場合はsourceまたは対応するprofile revisionを変更する。

Harnessは同じ規則でrevisionを照合し、text_digest=SHA256(UTF8(text))を`PreparedContextBlock`へ独立に計算する。SourceRef.raw_hashは原本全体のhashなので、引用範囲のtext_digestと同一とは限らない。誤った比較をしない。logical revision一致だけでKV cache hitを宣言しない。

COREの実装作業は、既存assemblePromptContextのtyped messagesからそのまま5区分を取得し、sourceを付与し、revisionを生成し、immutable snapshotを委譲Actionの根拠として保存すること。renderSystemMessagesでflattenした文字列から分類を復元してはいけない。Shiroの既存経路にRecallがなかった場合、それをgoldenとして固定せず、今回の依頼に必要なCORE Recall投影を正規経路で供給する。

StartInput.inputが現在のUser原文の正本。context_blocksの入口ではCharacter/Stable/Recall/Variableの4種だけを許し、user_messageが含まれた要求はINVALID_REQUEST。HarnessがinputからUser区分を一回だけ構成し、内部の五区分を完成する。公開ContextBlock一般型は返却/共有のため五種を表現するが、StartInputはschemaでも四種へ制限する。

## 2. COREのHuman relay issuer

COREが生成した委譲依頼はAutomation。本人が入力した原文を中継する場合に限ってOriginProofを付ける。元原文のsource MessageID/ThreadID、受付sequence、raw hashをCORE受付原本へ照合した上で署名する。要約した依頼やAgent生成文へHuman proofを流用しない。

F33 SignOriginProofを新設し、CORE nativeharnessclientが利用する。設定はissuer、key_id、audience、秘密鍵file参照、TTLを持つ。HMAC-SHA256の鍵は32 bytesの暗号学的乱数、BYTE_CONTRACTS§5のhex64+任意LF形式でprivate配置。fixture鍵を実機へ使わない。鍵管理の初期化は明示手順で、LLMが秘密を生成・記述しない。

OriginProofの必須fieldとMACの順序を固定する。

```text
issuer, key_id, audience, origin, source_message_id, source_thread_id,
destination_thread_id, mutation_key, raw_hash, sequence,
issued_at, expires_at, nonce, mac

MAC入力 = b"rencrow-origin-proof/v1\0"
        + 上の順序（mac以外）の各値をLP(UTF8文字列)で連結
sequenceは先頭0なしの非負10進整数。
日時はYYYY-MM-DDTHH:MM:SSZのUTC秒精度。
mac = lowercase_hex(HMAC-SHA256(key, MAC入力))
```

audienceは受信Harnessのconfigured principalと一致させる。destination_thread_idとmutation_keyを、そのAPI要求に一致させる。これにより別Thread/別operationへの使い回しを拒否する。originはhumanまたはautomation。受信側はissuer/key_id allowlist、MAC定時間比較、raw_hash、時刻、nonce、source関係を全て検査する。

TTLは最大300秒、未来のissued_at許容は30秒。now>expires_atは拒否。同じidempotency keyの既存receipt検索は期限/nonce再検査より先に行い、既に受理した同一payloadの再取得は期限切れでも可能にする。同key・異payloadは拒否。新規operationでは(issuer,key_id,nonce)重複を拒否する。

再送に際して新しい期限/nonce/proofへ書き換えるとpayloadが変わる。CORE clientは最初のpayloadを保持して同じkeyで再送する。未受理のまま期限が切れた場合、receipt/getで不成立を確認できてから新keyで新proofを作る。成立不明なら二重委譲しない。

## 3. 全入口のIntakeReceipt

CLIを含め、受信後に同じF34 PersistIntakeを呼ぶ。受付ではraw sealed Evidence、MessageID、受付順序、IntakeReceipt、operation receipt、必要なqueue/control更新を一つのlocal transactionで確定する。既存のitems.metadata_jsonとreceipts.result_jsonに保存でき、新しい独立正本を増やさない。

IntakeReceiptはreceipt_id/message_id/thread_id/evidence_id/principal/entrypoint/caller_profile_digest/declared_origin/effective_origin/proof_basis/proof_digest/accepted_sequence/accepted_atを持つ。型はPROTOCOLとschema。proof_basisはdeclared_local/verified_relay/automation/unknown。MAC自体は共有logへ出さない。

| 入口 | originの扱い |
|---|---|
| chatの行入力、または人が明示するexec --origin human | human profileが許可した場合declared_local。操作した物理人物を証明したとはしない |
| stdin pipe/非対話script/CORE生成依頼 | automation。human profileの既定値だけで昇格しない |
| COREの原文relay | 有効proofならverified_relay。それ以外のhuman claimは拒否 |
| 根拠のないimport/旧記録 | unknownとして保護。署名を後付けしない |

TUIかpipeかの自動判定だけを本人認証としない。`--origin human`はoperatorの明示宣言であり、configured profileの上限を越せない。CLIとCOREで出所の信頼モデルは異なるが、原本保持とreceiptの構造は共通にする。

## 4. CORE委譲・予算・取消

F26はCOREの親Task/Run/委譲Actionを作り、子Taskのresult/Receiptを参照する。Harnessの内部Tool ActionはCORE側で再実行しない。nativeharnessclientはStart/Resumeに完全なLimitsを渡す。RunResult.completedだけでCORE親Taskを成功採用しない。

Human relayの追加と通常Automation委譲をどちらも初回実装する。relayが作れないCORE経路を対象外としてHuman保持試験を削除しない。実際にraw原本がない経路はAutomation/Unknownとして扱い、その制約を利用者へ表示する。

COREの取消はturn/interruptへ伝え、receipt受領と外部Tool停止完了を分ける。client終了/再接続時も同keyでreceiptを取得し、二重start/resumeしない。

## 5. 成果物と試験

WP07でrevision builder、issuer、設定/鍵ローテーション手順、shared contract更新、既存sourceの到達性試験を作る。WP08はIMPLEMENTATION_DECISIONS§3のShiro shiro_native_coding_v1を、旧分岐より前でnative clientへ接続する。A46(Context)、A48(HMAC)、A49(CLI intake)、A53(CORE real wiring)を必須にする。付属ベクトルと同じ出力をGo側で独立に再現する。

## 6. 共通byte契約とcaller境界

mutation_payload_hashとcaller_profile_digestはBYTE_CONTRACTSを共用する。署名HMACの入力式は既存のLP式を維持する。最初のRPC payloadを保存して再送し、nonce/期限/limitsを勝手に更新しない。ContextBlockのrole投影はMODEL_PROJECTIONで定義し、COREは分類済み四blockと原文inputを渡すだけ。

WP08の対象選択と早期分岐はIMPLEMENTATION_DECISIONSで確定済み。CORE clientの内部struct名は裁量だが、旧CodexWorkPathに一致するkeywordがあってもnative選択を上書きしない。

