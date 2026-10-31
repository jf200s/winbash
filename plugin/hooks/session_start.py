#!/usr/bin/env python3
"""SessionStart hook: print the one-page summary shipped next to this plugin."""

from pathlib import Path
import sys

summary = Path(__file__).resolve().parent.parent / "context" / "startup.md"
sys.stdout.write(summary.read_text(encoding="utf-8"))
