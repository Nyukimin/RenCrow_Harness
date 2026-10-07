# RenCrow_Harness v0.2.2 設計・実装指示パッケージ

設計責任者はルミナ、実装・調査・試験担当はClaude。人向けCLIとCORE向けAPIが、同じ独立実行エンジン・作業履歴・Compaction・再開を使う製品を作る。

[START_HERE](START_HERE.md)から読み、[仕様書](RenCrow_Harness_SPEC.md)、[実装仕様書](RenCrow_Harness_IMPLEMENTATION_SPEC.md)、[作業指示](WORK_ORDER.md)へ進む。会話履歴と前版は不要。

IMPLEMENTER_BUNDLE.md（このrepositoryでは未追跡の生成物。`tools/speccheck/rebuild_bundle.py`で再生成）はMarkdownの一括読込用。schema・SQL・例・試験台帳を含むこのフォルダ全体を一緒に実装担当へ渡す。パッケージ内の値は設計契約であり、API配備済みやruntime受入済みを意味しない。

## 文書検査

```sh
python -m pip install -r checks/requirements.txt
python checks/rebuild_bundle.py
python checks/validate_package.py
python checks/validate_contract_details.py
python checks/validate_wire_contracts.py
```

requirementsは文書検査用Pythonだけの依存。Go製品runtimeの依存ではない。検査はschema・DDL・例・対応表・文書表・合成ベクトルを対象とし、製品を起動しない。全runtime試験はnot_runで納品する。

v0.2.2は未配備draftの改訂である。既存repositoryへの反映、製品コード、Go build、実Model/三OS試験、配備はClaudeのWORK_ORDERに含まれるが、このパッケージ作成で実行したとは扱わない。

## v0.2.2

実装前の17照会を閉じた版。[最終設計回答](IMPLEMENTATION_DECISIONS.md)から差分を読み、BYTE_CONTRACTS等の規範付属書とschemas/examples/wireを合わせて実装する。以前の曖昧なserializer指定は置換した。
