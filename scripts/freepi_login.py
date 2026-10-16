#!/usr/bin/env python3
# freepi login helper (stdlib only). runs the github device flow and saves
# the jwt to a local file for freepi_probe.py.
# usage: python3 scripts/freepi_login.py [--out freepi_jwt.txt]
import argparse, json, os, sys, time, urllib.request, urllib.error

BASE = "https://api.freepi.ai"
CONSENT_VERSION = "2"


def api(method, path, body=None, token=None):
    req = urllib.request.Request(BASE + path, method=method,
        data=json.dumps(body).encode() if body is not None else None,
        headers={"Content-Type": "application/json",
                 **({"Authorization": f"Bearer {token}"} if token else {})})
    try:
        with urllib.request.urlopen(req, timeout=20) as r:
            raw = r.read()
            return r.status, json.loads(raw) if raw else {}
    except urllib.error.HTTPError as e:
        raw = e.read()
        try:
            return e.code, json.loads(raw) if raw else {}
        except Exception:
            return e.code, {"_raw": raw.decode(errors="replace")}


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--out", default="freepi_jwt.txt",
                    help="jwt file (probe reads it; --into-config flow ignores it)")
    ap.add_argument("--into-config", default="",
                    help="config.yml path: append the jwt as a named account")
    args = ap.parse_args()
    out, into_config = args.out, args.into_config

    st, dev = api("POST", "/auth/github/device", {})
    if st != 200 or "session_id" not in dev:
        sys.exit(f"freepi login: device start failed (HTTP {st}): {dev}")
    sid, interval = dev["session_id"], dev.get("interval", 5)
    print(f"open {dev['verification_uri']} and enter code: {dev['user_code']}")
    input("sign in with github in the browser, then press enter here... ")

    st, res = api("POST", "/auth/consent",
                  {"session_id": sid, "consent_version": CONSENT_VERSION})
    if st != 200:
        sys.exit(f"freepi login: consent failed (HTTP {st}): {res}")

    fails = 0
    while True:
        try:
            st, res = api("POST", "/auth/token", {"session_id": sid})
            fails = 0
        except Exception as e:
            fails += 1
            if fails >= 6:
                sys.exit(f"network unreachable: {e}")
            time.sleep(max(interval, 5))
            continue
        if st == 403 and res.get("code") == "consent_required":
            sys.exit("server did not record consent, restart login.")
        if st != 200:
            sys.exit(f"token poll failed: {st} {res}")
        if "token" in res:
            break
        time.sleep(max(interval, res.get("retry_after", 0)))

    # owner-only: the jwt is a 90-day credential. skip the standalone file
    # when the jwt goes into config.yml, so `./nim freepi auth` does not
    # drop a stray freepi_jwt.txt in the cwd.
    if not into_config:
        fd = os.open(out, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
        with os.fdopen(fd, "w") as f:
            f.write(res["token"])
        print(f"saved jwt to {out} (mode 600)")

    st, me = api("GET", "/me", token=res["token"])
    print(f"token check: HTTP {st} {me if st != 200 else me.get('handle')}")

    if into_config:
        save_into_config(into_config, res["token"],
                          me.get("handle") if st == 200 else "")


def save_into_config(path, jwt, handle):
    """Append the jwt as a named freepi account. creates the section."""
    import re
    try:
        with open(path) as f:
            s = f.read()
    except OSError as e:
        sys.exit(f"cannot read {path}: {e}")
    name = re.sub(r"[^a-z0-9-]", "-", (handle or "acc").lower()).strip("-") or "acc"
    # avoid name clash: acc, acc-2, acc-3...
    base, n = name, 2
    while re.search(r'^-?\s*name:\s*"%s"' % re.escape(name), s, re.M):
        name = "%s-%d" % (base, n)
        n += 1
    block = '\n    - name: "%s"\n      jwt: "%s"\n' % (name, jwt)
    if re.search(r"(?m)^freepi:\s*$", s):
        # section exists: append under accounts: (create it if missing).
        # located by line index within the freepi: block: the old lazy
        # regex ran to eof and spliced the jwt under the wrong section
        # when accounts: was followed by a section with no blank line.
        lines = s.split("\n")
        try:
            fstart = next(i for i, l in enumerate(lines) if re.match(r"^freepi:\s*$", l))
        except StopIteration:
            fstart = -1
        fend = next((i for i in range(fstart + 1, len(lines))
                     if re.match(r"^\S", lines[i])), len(lines))
        ai = next((i for i in range(fstart + 1, fend)
                   if re.match(r"^  accounts:\s*$", lines[i])), None)
        if ai is None:
            lines[fend:fend] = ["  enabled: true", "  accounts:", block.rstrip("\n")]
        else:
            aj = next((i for i in range(ai + 1, fend)
                       if re.match(r"^  \S", lines[i])), fend)
            lines[aj:aj] = [block.rstrip("\n")]
        s = "\n".join(lines)
    else:
        s = s.rstrip() + ("\n# --- freepi: ad-supported free models (see docs/reference/freepi-api.md) ---\n"
                          "freepi:\n  enabled: true\n  accounts:\n" + block)
    with open(path, "w") as f:
        f.write(s)
    try:
        os.chmod(path, 0o600)
    except OSError:
        pass
    print(f"added freepi account {name} to {path}")


if __name__ == "__main__":
    main()
