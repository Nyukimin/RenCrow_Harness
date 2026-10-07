# RenCrow_Harness 設計資産

設計v0.2.2（2026-10-07 JST、設計責任: ルミナ）の資産をそのまま置いたもの。実装の基準であり、実装が済んだことを意味しない。読む順序は[START_HERE](START_HERE.md)、仕様は[仕様書](RenCrow_Harness_SPEC.md)と[実装仕様書](RenCrow_Harness_IMPLEMENTATION_SPEC.md)、作業は[WORK_ORDER](WORK_ORDER.md)。元のパッケージ説明は[PACKAGE_README.md](PACKAGE_README.md)。

## 取り込み元

設計パッケージzip `RenCrow_Harness_v0.2.2` の SHA-256:

```text
802a00297bf8c0ed01e4c0ce254f7663aadbf30da7163305e88625969f098c53
```

内容は変更していない。例外は下記「このrepositoryでの変更」の経路・link修正だけである。

## 配置対応表

文書本文が使う元パッケージの相対pathは、次の表でこのrepositoryの場所へ読み替える（本文中のpath表記は設計時のまま残してある）。

| 元パッケージ | このrepository |
|---|---|
| `*.md`（`README.md`と`IMPLEMENTER_BUNDLE.md`を除く） | `docs/` |
| `README.md` | `docs/PACKAGE_README.md`（`docs/README.md`は本書） |
| `acceptance_matrix.json` `work_packages.json` `review_resolution.json` `api_contract.json` `*_VALIDATION.json` `VALIDATION_SCOPE.json` `PACKAGE_STATUS.json` `SOURCE_BASELINE.json` | `docs/` |
| `templates/` | `docs/templates/` |
| `schemas/` | `schemas/` |
| `prompts/` | `prompts/` |
| `sql/001_initial.sql` | `migrations/001_initial.sql` |
| `examples/`（`examples/wire/`を含む） | `testdata/contract/examples/` |
| `contracts/` | `testdata/contract/contracts/` |
| `checks/`（Pythonの文書検査） | `tools/speccheck/` |
| `IMPLEMENTER_BUNDLE.md` | 複製しない（元文書を連結した生成物で別の正本ではない）。`python tools/speccheck/rebuild_bundle.py`で`docs/IMPLEMENTER_BUNDLE.md`として再生成でき、`.gitignore`で未追跡 |
| `SHA256SUMS.txt` | 複製しない（旧配置のhashで、このrepositoryの配置とは一致しない） |

## このrepositoryでの変更

配置を変えたことで壊れるものだけを直した。仕様の意味は変えていない。

- `docs/START_HERE.md`: 再生成scriptへのlinkを`../tools/speccheck/rebuild_bundle.py`へ。
- `docs/STORAGE.md`: DDLへのlinkを`../migrations/001_initial.sql`へ（表示文字は元のまま）。
- `docs/PACKAGE_README.md`: 複製しない`IMPLEMENTER_BUNDLE.md`へのlinkを説明文へ置換。
- `tools/speccheck/layout.py`（新規）と各scriptのpath解決: 元の平坦な配置を上表の配置へ写す。検査の中身は変えていない。
- `docs/DOCUMENT_VALIDATION.json`: 検査を再実行して再生成。`local_document_links`の件数だけ96から55に変わった（複製しない`IMPLEMENTER_BUNDLE.md`内のlinkが対象から外れ、本書のlinkが加わったため）。`CONTRACT_VALIDATION.json`と`WIRE_CONTRACT_VALIDATION.json`は元とbyte一致。

## 設計検査

```sh
python -m pip install -r tools/speccheck/requirements.txt
python tools/speccheck/validate_package.py
python tools/speccheck/validate_contract_details.py
python tools/speccheck/validate_wire_contracts.py
```

`tools/speccheck/`のPythonは設計文書・fixtureの検査専用で、製品runtimeの依存ではない。`contract_codec.py`と`projection_reference.py`は合成fixture用の参照実装であり、Go実装の期待値を作る道具ではない。Go実装は`testdata/contract/`の固定goldenに対して独立に合わせる。
