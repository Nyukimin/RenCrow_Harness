# Windows private作成のowner不一致（2026-10-09 JST）

## 文書情報

- 引き継ぎ元: RenCrow_Harness完全完了作業のWindows独立検証。
- 契約正本: `docs/STORAGE.md`、`internal/fsperm`。private objectは現在のprocess userだけがアクセスできること。
- 根拠: 非公開の `Tmp/test-runtime/_runs/windows-acl-verify/native-acl-evidence.json`、`post-smoke-token-acl.json`。receipt、実SID、SDDL、実機pathはGit対象外。
- 本文は既知証拠の記録。追加調査を実施していない。

## 結論

Windows実機の新規store初期化は、owner-onlyな親の下で作る `locks` のownerがprocessのTokenOwner（Administrators）になるため拒否された。processのTokenUserとTokenOwnerは異なる。checkerの拒否は契約どおりであり、新規private objectの作成側を修正する必要がある。

## 実測

| 対象 | 結果 |
| --- | --- |
| 旧baselineのowner-only初期化 | ACL未実装として拒否 |
| 現行sourceのnative build | 成功。290個のGo sourceを実機snapshotと照合し、差分なし |
| 現行CLIのUnicode private root初期化 | exit 1。`locks`のみ作成、DB・staging未作成 |
| 親data root | 現在userがowner。現在userだけへの継承可能FullControl ACE |
| 作成されたlocks | 現在userへの継承ACEを持つが、ownerはAdministrators |
| 既存fixture 12 object | 実行前後のowner／SDDLに変更なし |
| 不在config・fileをdirectoryとして扱う要求 | 拒否。private pathの漏洩なし |
| 正常read・Everyone許可拒否・link検証 | 未実施。初期化失敗後、同条件の再試行を止めた |

snapshotの初回buildではHEAD版とworking tree版の混在による重複symbolがあった。実sourceへ同期しmanifestを照合した後に解消した検証setupの失敗であり、productのACL不具合とは分ける。

## Failure Knowledge

- **Failure:** Windows nativeの新規store初期化が途中で失敗した。
- **Problem:** 書込みを始めるprocess userと、新しく作るprivate objectのownerが一致しない。
- **Cause:** Windowsの新規object ownerはTokenOwnerに依存し、TokenUserや親directoryのownerと同一とは限らない。GoのUnix mode指定だけではその差を閉じられない。
- **Lesson:** cross build／vetの成功ではnativeの作成・継承・SQLite補助fileの権限を実証できない。
- **Invariant:** checkerは現在のprocess userとの一致と非ownerへの許可なしを維持する。既存owner／ACLを自動修復しない。新規private作成でownerとprotectedな継承を確定し、保証不能なら書込み前に拒否する。
- **Enforcement:** 修正方針はCLI開始時の自process TokenOwner初期化と、fsperm所有の新規private作成API。User／groups／privileges／TokenDefaultDacl／OS policyを変更しない。既存rootの安全な継承条件と既存SQLite補助fileを検査する。実装と実機保証は未確認。
- **Tests:** standard userとelevated userのfresh init、DB／WAL／SHM、backup、reopen、既存ACL不変、継承不足・他owner・広いACLの拒否を必要条件にする。`.test.exe`ではなく正規CLIでnative receiptを取得する。

## 未解決・未確認

- 新規private作成の修正と独立検証は未完了。
- 自process TokenOwner変更が対象実機で可能か、SQLite／backup／子processまで保証が維持されるかは未確認。
- 本記録はWindows gateの完了、P6A／P6Bの受入、配備完了を宣言しない。

