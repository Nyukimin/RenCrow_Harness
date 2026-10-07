#!/usr/bin/env python3
"""Regenerate a reading bundle from the actual specification files."""
from pathlib import Path
from layout import DOCS as ROOT  # Markdown sources live in docs/; the generated bundle is not tracked
ORDER=['START_HERE.md','IMPLEMENTATION_DECISIONS.md','ARCHITECTURE_DECISIONS.md','REPOSITORY_MAP.md','RenCrow_Harness_SPEC.md','RenCrow_Harness_IMPLEMENTATION_SPEC.md','WORK_ORDER.md','PROTOCOL.md','LLM_INTEGRATION.md','RETRY_CONTRACT.md','CORE_INTEGRATION.md','INTERNAL_CONTRACTS.md','STORAGE.md','CONFIGURATION.md','REFERENCE_DECISIONS.md','REVIEW_RESOLUTION.md','ACCEPTANCE_MATRIX.md','SOURCES.md','BYTE_CONTRACTS.md','MODEL_PROJECTION.md','STAGE_DATA.md','CHECKPOINT_FORMAT.md','ERROR_MAPPING.md','STREAM_CONTRACT.md','HOST_ASSETS.md','EVENT_CONTRACT.md']
def main():
    content=['# RenCrow_Harness v0.2.2 実装担当向け一括読込\n\n元文書を連結した配布用生成物。別の正本ではない。schema・SQL・JSON台帳はZIPを併せて読む。\n']
    for name in ORDER:
        content.append('\n\n<!-- BEGIN '+name+' -->\n\n'+(ROOT/name).read_text(encoding='utf-8')+'\n<!-- END '+name+' -->\n')
    (ROOT/'IMPLEMENTER_BUNDLE.md').write_text(''.join(content),encoding='utf-8')
if __name__=='__main__':main()
