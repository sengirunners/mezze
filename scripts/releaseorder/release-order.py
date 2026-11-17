#!/usr/bin/env python3
"""Set, unset or show this machine's own order of zm release repositories.

A zm built from one repository reads releases from the public repository first, then from the repository it was
built from (SPEC 2, "Release channel"). Once the public repository has releases, such a machine takes only those.
`set` puts the build repository first: it writes `update_repos` = [build repository, public repository] into this
machine's own config.json, both names read from this machine's zm (the built-in order `zm daemon update status`
prints on the default order), never typed. `unset` removes the key. `show` changes nothing. Each ends by printing
the order zm reads after it ran (docs/runbooks/zm-upgrade.md, "A machine's own order of release repositories").

It changes nothing but that one key: every other key keeps its value, the file keeps its mode, and the write is
one rename. It refuses, writing nothing, when it cannot read the home, the order or the file, when the file is not
a JSON object with unique keys, or when the order zm reads afterwards is not the one it wrote (it then puts the
file back). `set` on a machine already on its own order leaves it; after an update that changed the built-in
order, `unset` then `set` takes the new one. Made for `zm exec --machine <name> -- <this script> <verb>`, whose run
gets the daemon's own ZM_HOME; by hand it is the shell's. Either way the script asks zm which home it resolves.

Usage: release-order.py show|set|unset
Exit status: 0 done or nothing to do, 1 refused (nothing changed, unless its message says otherwise), 2 usage.
"""

import json
import os
import re
import shutil
import subprocess
import sys
import tempfile
from decimal import Decimal

KEY = "update_repos"
DEFAULT_TAG = " (default)"
CONFIG_TAG = " (config " + KEY + ")"
LINE = re.compile(r"^release sources on ([^:]+): (.+)$")
# GitHub's rules for an owner and a repository name, as zm checks update_repos (syntax only).
OWNER = re.compile(r"^[A-Za-z0-9][A-Za-z0-9-]{0,38}$")
NAME = re.compile(r"^[A-Za-z0-9._-]{1,100}$")


class Refused(Exception):
    pass


def zm_env():
    env = dict(os.environ)
    env.pop("ZM_HOST", None)  # this machine's own daemon, never a remote one
    env["ZM_NO_AUTOSTART"] = "1"  # never start a daemon: a home with none is refused
    return env


def zm(*args):
    exe = shutil.which("zm")
    if exe is None:
        raise Refused("no zm on PATH")
    try:
        p = subprocess.run([exe, *args], env=zm_env(), capture_output=True, text=True, timeout=120)
    except subprocess.TimeoutExpired:
        raise Refused("zm " + " ".join(args) + " did not answer in 120 s")
    return p.returncode, p.stdout, p.stderr


def home():
    # zm doctor exits 1 when any check fails; its JSON still lists the home check.
    _, out, err = zm("doctor", "--json")
    try:
        checks = json.loads(out)
    except ValueError:
        raise Refused("zm doctor --json gave no JSON: " + (err.strip() or out.strip())[:300])
    for c in checks if isinstance(checks, list) else []:
        if isinstance(c, dict) and c.get("check") == "home":
            d = c.get("detail", "")
            if c.get("status") != "ok":
                raise Refused("zm's home check is " + str(c.get("status")) + ": " + str(d))
            if not isinstance(d, str) or not os.path.isabs(d) or not os.path.isdir(d):
                raise Refused("zm's home check names no directory: " + str(d))
            return d
    raise Refused("zm doctor --json has no home check")


def is_repo(r):
    owner, sep, name = r.partition("/")
    return (sep == "/" and OWNER.match(owner) is not None and NAME.match(name) is not None
            and name not in (".", "..") and not name.lower().endswith(".git"))


def order():
    """The order this machine's zm reads: (status line, repos, set). set is False on the default order."""
    code, out, err = zm("daemon", "update", "status")
    # zm prints this machine's order as the last line, after every machine's last check.
    tail = [ln for ln in out.splitlines() if ln.strip()][-1:]
    lines = [ln for ln in tail if ln.startswith("release sources on ")]
    if code != 0 or not lines:
        why = (err.strip() or out.strip())[:300]
        raise Refused("zm daemon update status printed no release order: this zm predates the " + KEY +
                      " setting (update it first), or no local daemon answered" + (": " + why if why else ""))
    line = lines[-1]
    m = LINE.match(line)
    if m is None:
        raise Refused("cannot read: " + line)
    rest = m.group(2)
    if rest.endswith(DEFAULT_TAG):
        repos, is_set = rest[: -len(DEFAULT_TAG)].split(", "), False
    elif rest.endswith(CONFIG_TAG):
        repos, is_set = rest[: -len(CONFIG_TAG)].split(", "), True
    else:
        raise Refused("zm cannot read this machine's order: " + line)
    for r in repos:
        if not is_repo(r):
            raise Refused("not a GitHub repository (owner/name) in: " + line)
    return line, repos, is_set


def unique_object(pairs):
    seen = {}
    for k, v in pairs:
        if k in seen:
            raise Refused("config.json has the key " + json.dumps(k) + " twice")
        seen[k] = v
    return seen


def _no_constant(name):
    raise ValueError(name + " is not JSON")


def parse(text):
    # Decimal: numbers compare by value, so a rewrite that only respells one is still the same file.
    try:
        doc = json.loads(text, object_pairs_hook=unique_object, parse_float=Decimal, parse_constant=_no_constant)
    except ValueError as e:
        raise Refused("config.json is not JSON: " + str(e))
    if not isinstance(doc, dict):
        raise Refused("config.json is not a JSON object")
    return doc


def stamp(path):
    st = os.stat(path)
    return (st.st_ino, st.st_size, st.st_mtime_ns)


def write(path, before, doc):
    """Replace path with doc in one rename, keeping its mode; refuse if it changed since it was read.
    Returns the new file's stamp."""
    try:
        text = json.dumps(doc, indent=2, sort_keys=True, ensure_ascii=False, default=_num) + "\n"
        text.encode("utf-8")
    except (ValueError, UnicodeError) as e:  # e.g. an escaped lone surrogate, which UTF-8 cannot hold
        raise Refused("config.json cannot be written back as it reads: " + str(e))
    got = parse(text)
    if got != doc:
        raise Refused("the rewritten config.json would not read back the same")
    st = os.stat(path)
    fd, tmp = tempfile.mkstemp(dir=os.path.dirname(path), prefix=".config.json.release-order-")
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as f:
            f.write(text)
            f.flush()
            os.fsync(f.fileno())
        os.chmod(tmp, st.st_mode & 0o7777)
        if stamp(path) != before:
            raise Refused("config.json changed while this ran: run it again")
        os.replace(tmp, path)
        tmp = None
    finally:
        if tmp is not None:
            os.unlink(tmp)
    return stamp(path)


def _num(v):
    if isinstance(v, Decimal):
        return float(v)  # parse compares the result by value: a number float cannot hold refuses the write
    raise TypeError(type(v).__name__)


def put_back(path, raw, written):
    """Put the original bytes back, unless something rewrote the file since this script did."""
    changed = "config.json changed again after this wrote it: not put back; read it and fix it by hand"
    if stamp(path) != written:
        raise Refused(changed)
    fd, tmp = tempfile.mkstemp(dir=os.path.dirname(path), prefix=".config.json.release-order-")
    try:
        with os.fdopen(fd, "wb") as f:
            f.write(raw)
            f.flush()
            os.fsync(f.fileno())
        os.chmod(tmp, os.stat(path).st_mode & 0o7777)
        if stamp(path) != written:
            raise Refused(changed)
        os.replace(tmp, path)
        tmp = None
    finally:
        if tmp is not None:
            os.unlink(tmp)


def main(argv):
    if len(argv) != 2 or argv[1] not in ("show", "set", "unset"):
        print("usage: release-order.py show|set|unset", file=sys.stderr)
        return 2
    verb = argv[1]
    h = home()
    line, repos, is_set = order()
    print("zm: " + shutil.which("zm") + ", home " + h)
    if verb == "show":
        print(line)
        return 0
    if verb == "set" and is_set:
        print("unchanged: this machine is already on its own order (unset, then set, takes this build's order)")
        print(line)
        return 0
    if verb == "unset" and not is_set:
        print("unchanged: this machine is on the default order")
        print(line)
        return 0
    path = os.path.join(h, "config.json")
    if os.path.islink(path):
        raise Refused(path + " is a symbolic link: edit the file it points to by hand")
    try:
        with open(path, "rb") as f:
            st = os.fstat(f.fileno())
            raw = f.read()
    except OSError as e:
        raise Refused("cannot read " + path + ": " + e.strerror)
    if st.st_uid != os.geteuid():
        raise Refused(path + " belongs to another user: run this as the user that runs the daemon")
    before = (st.st_ino, st.st_size, st.st_mtime_ns)
    try:
        doc = parse(raw.decode("utf-8"))
    except UnicodeDecodeError:
        raise Refused("config.json is not UTF-8")
    want = None
    if verb == "set":
        if len(repos) != 2 or repos[0].lower() == repos[1].lower():
            raise Refused("this build's order is " + ", ".join(repos) + ": not a public repository then the "
                          "repository it was built from, so there is no build repository to put first")
        if any(k.lower() == KEY for k in doc):
            raise Refused("config.json has a key like " + KEY + " that zm does not read as it: fix it by hand")
        want = [repos[1], repos[0]]
        doc[KEY] = want
    else:
        if KEY not in doc:
            raise Refused("zm reads this machine's own order, but config.json at " + path + " has no " + KEY)
        del doc[KEY]
    written = write(path, before, doc)
    try:
        line, repos, is_set = order()
        ok = is_set and repos == want if verb == "set" else not is_set
    except Refused as e:
        line, ok = str(e), False
    if not ok:
        put_back(path, raw, written)
        raise Refused("zm did not read the order written (" + line + "): config.json is put back as it was")
    print(("set: " + ", ".join(want)) if verb == "set" else "unset: back on the default order")
    print(line)
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main(sys.argv))
    except Refused as e:
        print("release-order: refused: " + str(e), file=sys.stderr)
        sys.exit(1)
    except OSError as e:  # a write that failed part way: say so, and what to read
        print("release-order: failed: " + str(e) + " (read config.json and zm daemon update status)", file=sys.stderr)
        sys.exit(1)
