"""Repository layout for the document checks (design assets only, not a product dependency).

The validators were written against the flat design-package layout. This module maps
those logical names to the places the files occupy in this repository so the
validator bodies stay unchanged.
"""
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
DOCS = ROOT / 'docs'
EXAMPLES = ROOT / 'testdata' / 'contract' / 'examples'
CONTRACTS = ROOT / 'testdata' / 'contract' / 'contracts'

_PREFIXES = (
    ('examples/', EXAMPLES),
    ('contracts/', CONTRACTS),
    ('schemas/', ROOT / 'schemas'),
    ('prompts/', ROOT / 'prompts'),
    ('templates/', DOCS / 'templates'),
)


def path(name: str) -> Path:
    """Resolve a logical design-package path (for example 'examples/wire/x.json')."""
    if name == 'sql/001_initial.sql':
        return ROOT / 'migrations' / '001_initial.sql'
    for prefix, base in _PREFIXES:
        if name.startswith(prefix):
            return base / name[len(prefix):]
    return DOCS / name  # top-level Markdown and ledger JSON
