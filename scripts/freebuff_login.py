#!/usr/bin/env python3
# freebuff login helper. mints a login url, waits for the browser click,
# verifies the token, writes it into config.yml (managed block at eof).
# usage: python3 freebuff_login.py [--config config.yml] [--name acc1]
import argparse, json, os, re, sys, tempfile, time, uuid, urllib.request, urllib.parse, urllib.error

BASE = "https://freebuff.com"          # login host (real client uses this)
API = "https://www.codebuff.com"       # session/chat/ads host
BEGIN = "# --- freebuff (managed by freebuff_login.py) ---"
END = "# --- end freebuff ---"


def api(method, url, body=None, token=None):
    req = urllib.request.Request(url, method=method,
        data=json.dumps(body).encode() if body is not None else None,
        headers={"Content-Type": "application/json",
                 **({"Authorization": f"Bearer {token}"} if token else {})})
    try:
        with urllib.request.urlopen(req, timeout=20) as r:
            return r.status, json.loads(r.read() or b"null")
    except urllib.error.HTTPError as e:
        try:
            return e.code, json.loads(e.read() or b"null")
        except Exception:
            return e.code, {}
    except (urllib.error.URLError, TimeoutError) as e:
        sys.exit(f"network error: {e}")


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--config", default="config.yml")
    ap.add_argument("--name", default="acc1")
    args = ap.parse_args()
    cfg, name = args.config, args.name

    fp = uuid.uuid4().hex
    st, code = api("POST", BASE + "/api/auth/cli/code", {"fingerprintId": fp})
    if not (st == 200 and "loginUrl" in code):
        sys.exit(f"login code failed: {st} {code}")
    print("open this url in your browser (expires in 1h):")
    print(code["loginUrl"])

    qs = urllib.parse.urlencode({"fingerprintId": fp,
        "fingerprintHash": code["fingerprintHash"], "expiresAt": code["expiresAt"]})
    while time.time() * 1000 < code["expiresAt"]:
        time.sleep(5)
        st, res = api("GET", BASE + f"/api/auth/cli/status?{qs}")
        user = (res or {}).get("user") or {}
        if st == 200 and user.get("authToken"):
            break
    else:
        sys.exit("timed out waiting for login.")

    tok = user["authToken"]
    st, sess = api("GET", API + "/api/v1/freebuff/session", token=tok)
    if not (st == 200 and "status" in sess):
        sys.exit(f"token verify failed: {st} {sess}")
    print(f"logged in as {user.get('email')} (session: {sess['status']})")

    # merge into managed block in config.yml (stdlib only, no yaml dep).
    # single atomic write: the old append-then-rewrite duplicated the file
    # every login. any pre-existing hand-written top-level freebuff: block
    # is dropped first (a duplicate key breaks yaml uniqueKeys and used to
    # trigger total config loss on restart).
    try:
        text = open(cfg).read()
    except FileNotFoundError:
        text = ""
    accts = {}
    m = re.search(re.escape(BEGIN) + r"(.*?)" + re.escape(END), text, re.S)
    if m:
        for nm, tk in re.findall(r'-\s*name:\s*"([^"]+)"\s*\n\s*token:\s*"([^"]+)"', m.group(1)):
            accts[nm] = tk
        text = text[:m.start()] + text[m.end():]
    text = re.sub(r"(?m)^freebuff:\n(?:^[ \t]+.*\n?)+", "", text)
    accts[name] = tok
    lines = [BEGIN, "freebuff:", "  enabled: true", "  test_port: 5420",
             "  accounts:"] + \
        [f'    - name: "{n}"\n      token: "{t}"' for n, t in accts.items()] + \
        ["  show_ads: true", "  ad_rotation_sec: 60", "  session_poll_sec: 30",
         "  model_refresh_sec: 300", "  timeout_ms: 20000", END]
    if text and not text.endswith("\n"):
        text += "\n"
    text += "\n".join(lines) + "\n"
    fd, tmp = tempfile.mkstemp(dir=os.path.dirname(os.path.abspath(cfg)) or ".")
    with os.fdopen(fd, "w") as f:
        f.write(text)
    os.chmod(tmp, 0o600)
    os.replace(tmp, cfg)
    print(f"saved account {name!r} to {cfg} (hot-reloaded, no restart)")


if __name__ == "__main__":
    main()
