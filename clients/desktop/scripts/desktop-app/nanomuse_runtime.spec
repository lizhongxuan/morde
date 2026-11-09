# -*- mode: python ; coding: utf-8 -*-
"""PyInstaller spec for the runtime nanoMuse Desktop carries: one folder with a `nanomuse`
executable that serves the app, talks to the hub and moves this computer's hands.

    python -m PyInstaller --noconfirm --clean scripts/desktop-app/nanomuse_runtime.spec

Built by scripts/desktop-app/build-runtime.py, which puts the folder where electron-builder
picks it up (harness/desktop/runtime/). One-folder rather than one-file: a one-file build
unpacks itself on every start, which is slow for a server that starts with the app.
"""

import os
import sys

from PyInstaller.utils.hooks import collect_all, collect_data_files, collect_submodules

ROOT = os.path.abspath(os.path.join(SPECPATH, "..", ".."))
sys.path.insert(0, ROOT)

hidden = []
hidden += collect_submodules("nanomuse")
hidden += collect_submodules("uvicorn")
hidden += collect_submodules("websockets")
hidden += collect_submodules("anyio")
hidden += ["pydantic.deprecated.decorator", "tzdata", "PIL.Image", "PIL.ImageDraw", "PIL.ImageFont"]

datas = [
    (os.path.join(ROOT, "nanomuse", "server", "static"), os.path.join("nanomuse", "server", "static")),
    (os.path.join(ROOT, "nanomuse", "skills", "builtin"), os.path.join("nanomuse", "skills", "builtin")),
    (os.path.join(ROOT, "config", "config.example.toml"), "nanomuse"),
]
binaries = []

# packages that read their own data at run time
for pkg in ("certifi", "ddgs", "tzdata", "mcp", "html2text", "qrcode", "pywebpush", "py_vapid"):
    try:
        d, b, h = collect_all(pkg)
    except Exception:  # noqa: BLE001 — optional on some platforms
        continue
    datas += d
    binaries += b
    hidden += h

# the hands: pyautogui and friends pull in what they need lazily
for pkg in ("pyautogui", "pyscreeze", "pymsgbox", "pytweening", "mouseinfo", "pyperclip", "mss"):
    try:
        d, b, h = collect_all(pkg)
    except Exception:  # noqa: BLE001
        continue
    datas += d
    binaries += b
    hidden += h

excludes = ["tkinter", "matplotlib", "numpy", "scipy", "pandas", "IPython", "jupyter", "pytest", "playwright"]

a = Analysis(
    [os.path.join(ROOT, "scripts", "desktop-app", "runtime_entry.py")],
    pathex=[ROOT],
    binaries=binaries,
    datas=datas,
    hiddenimports=sorted(set(hidden)),
    hookspath=[],
    runtime_hooks=[],
    excludes=excludes,
    noarchive=False,
)
pyz = PYZ(a.pure)

exe = EXE(
    pyz,
    a.scripts,
    [],
    exclude_binaries=True,
    name="nanomuse",
    debug=False,
    bootloader_ignore_signals=False,
    strip=False,
    upx=False,
    console=True,
    disable_windowed_traceback=False,
    argv_emulation=False,
    target_arch=None,
    codesign_identity=None,
    entitlements_file=None,
)
coll = COLLECT(
    exe,
    a.binaries,
    a.datas,
    strip=False,
    upx=False,
    name="nanomuse",
)
