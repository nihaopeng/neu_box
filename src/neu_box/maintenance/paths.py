"""Installed private executable paths shared by setup and its diagnostics."""

from pathlib import Path


CTL_BIN = Path("/usr/libexec/neu-box/neuboxctl/neuboxctl")
RUNTIME_BIN = Path("/usr/libexec/neu-box/neu-box-runtime")
HOOK_BIN = Path("/usr/libexec/neu-box/neu-box-hook")
