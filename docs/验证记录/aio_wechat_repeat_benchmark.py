#!/usr/bin/env python3
"""Read-only T04 repeat benchmark; never opens or submits a friend request."""
import re
import subprocess
import time
import pyatspi

QUERY = 'aacool22'


def run(*args, check=True):
    return subprocess.run(list(map(str, args)), capture_output=True, text=True, check=check).stdout.strip()


def walk(root, depth=0):
    if root is None or depth > 24:
        return
    try:
        yield root
        for child in root:
            yield from walk(child, depth + 1)
    except Exception:
        return


def frame():
    frames = []
    for app in pyatspi.Registry.getDesktop(0):
        if app and app.name and 'wechat' in app.name.lower():
            for child in app:
                if child and child.name == 'Add Contacts' and child.getRoleName() == 'frame' and child.getState().contains(pyatspi.STATE_SHOWING):
                    frames.append(child)
    if len(frames) != 1:
        raise RuntimeError(f'Add Contacts frame count={len(frames)}')
    return frames[0]


def atspi_attempt():
    f = frame()
    nodes = list(walk(f))
    fields = [n for n in nodes if n.name == 'Search' and n.getRoleName() == 'text' and n.getState().contains(pyatspi.STATE_SHOWING)]
    buttons = [n for n in nodes if n.name == 'Search' and n.getRoleName() == 'push button' and n.getState().contains(pyatspi.STATE_SHOWING)]
    if len(fields) != 1 or len(buttons) != 1:
        raise RuntimeError('search control is not unique')
    if fields[0].queryText().getText(0, -1) != QUERY and not fields[0].queryEditableText().setTextContents(QUERY):
        raise RuntimeError('could not set search text')
    if fields[0].queryText().getText(0, -1) != QUERY:
        raise RuntimeError('search text readback mismatch')
    r = buttons[0].queryComponent().getExtents(pyatspi.DESKTOP_COORDS)
    pyatspi.Registry.generateMouseEvent(r.x + r.width // 2, r.y + r.height // 2, 'b1c')
    deadline = time.monotonic() + 1
    while time.monotonic() < deadline:
        f = frame()
        names = [n.name for n in walk(f) if n.name and n.getState().contains(pyatspi.STATE_SHOWING)]
        field = [n for n in walk(f) if n.name == 'Search' and n.getRoleName() == 'text' and n.getState().contains(pyatspi.STATE_SHOWING)]
        status = next((v for v in ('Request sent', 'Add to Contacts') if v in names), None)
        if len(field) == 1 and field[0].queryText().getText(0, -1) == QUERY and status:
            return status
        time.sleep(.1)
    raise RuntimeError('profile status not found')


def x11_attempt():
    ids = run('xdotool', 'search', '--onlyvisible', '--name', '^Add Contacts$').split()
    if len(ids) != 1:
        raise RuntimeError(f'Add Contacts window count={len(ids)}')
    wid = ids[0]
    if run('xdotool', 'getwindowname', wid) != 'Add Contacts':
        raise RuntimeError('window title mismatch')
    geo = dict(line.split('=', 1) for line in run('xdotool', 'getwindowgeometry', '--shell', wid).splitlines() if '=' in line)
    if (int(geo['WIDTH']), int(geo['HEIGHT'])) != (360, 560):
        raise RuntimeError('window geometry changed')
    run('xdotool', 'mousemove', '--window', wid, 145, 69, 'click', 1)
    run('xdotool', 'key', 'ctrl+a')
    run('xdotool', 'type', '--clearmodifiers', '--delay', 20, QUERY)
    run('xdotool', 'mousemove', '--window', wid, 303, 69, 'click', 1)
    time.sleep(.3)
    image = subprocess.run(['import', '-window', wid, 'png:-'], capture_output=True, check=True).stdout
    field = subprocess.run(['convert', 'png:-', '+repage', '-crop', '236x42+25+49', '+repage', '-resize', '944x168', '-colorspace', 'Gray', 'png:-'], input=image, capture_output=True, check=True).stdout
    field_text = subprocess.run(['tesseract', 'stdin', 'stdout', '--psm', '7'], input=field, capture_output=True, check=True).stdout.decode('utf-8', 'replace').lower()
    normalized = re.sub(r'[^a-z0-9]', '', field_text)
    if QUERY not in normalized:
        raise RuntimeError('query OCR mismatch')
    button = subprocess.run(['convert', 'png:-', '+repage', '-crop', '165x45+100+288', '+repage', '-resize', '660x180', '-colorspace', 'Gray', 'png:-'], input=image, capture_output=True, check=True).stdout
    button_text = subprocess.run(['tesseract', 'stdin', 'stdout', '--psm', '7'], input=button, capture_output=True, check=True).stdout.decode('utf-8', 'replace').lower()
    status = 'Request sent' if 'request sent' in button_text else 'Add to Contacts' if 'add to contacts' in button_text else None
    if not status:
        raise RuntimeError('profile status OCR missing')
    return status


for i in range(1, 11):
    method, fn = ('M02 AT-SPI', atspi_attempt) if i % 2 else ('M04 X11', x11_attempt)
    start = time.perf_counter()
    try:
        status = fn()
        result = f'资料页已核验：{status}；申请已提交过，本轮不重复提交'
    except Exception as error:
        result = f'失败：{type(error).__name__}: {error}'
    elapsed = round((time.perf_counter() - start) * 1000)
    print(f'| {method} | T04 | {i}（优化后） | {elapsed} | {result} |', flush=True)
