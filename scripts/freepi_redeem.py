#!/usr/bin/env python3
"""redeem a free-pi credit code and run the ad support, the way the released
free-pi-cli (0.6.11) does it.

/redeem FREEPI-XXXX-XXXX is one request:

    POST /me/redeem   {"code": "FREEPI-XXXX-XXXX"}
    authorization: Bearer <account jwt>
    content-type: application/json
    -> {"ok": true, "credits_added": n, "balance": n}
       or {"code": "not_found|not_yours|expired|already_redeemed|
                  rate_limited|guest_login_required|not_redeemable",
           "message": "..."}

a code works once, on the account it was issued to, so run it on the jwt that
owns it. the ad support the cli runs behind every turn is:

    GET  /ads/next?slot=banner|inline   (x-session-id, x-gravity-context)
         -> {ad_id, click_token, click_url, source, imp_url?} or 204
    GET  <imp_url>                      (only source=gravity, trygravity host)
    POST /ads/impression                {"ad_id","click_token","imp_fired":true}
    GET  /me                            credits + allowance meter

nothing is displayed and nothing is faked: the click_url is never requested
(the cli only opens it in a browser when a person clicks the ad card), and an
impression is only ever posted for an ad the server actually served.

usage:
    ./scripts/freepi_redeem.py FREEPI-Z54Q-QRC4
    ./scripts/freepi_redeem.py FREEPI-Z54Q-QRC4 --ads 3
    ./scripts/freepi_redeem.py --ads 3 --account main
    ./scripts/freepi_redeem.py --ads 1 --dry-run

the jwt is read from config.yml (freepi.accounts[].jwt) and never printed.
"""

import argparse
import json
import sys
import time
import urllib.error
import urllib.request
import uuid
from urllib.parse import urlparse

DEFAULT_BASE = "https://api.freepi.ai"
DEFAULT_VERSION = "0.6.11"
GRAVITY_HOSTS = ("trygravity.ai", "trygravity.com")


def load_account(path, name):
    try:
        import yaml  # noqa: F401
    except ImportError:
        sys.exit("pyyaml is required to read config.yml (pip install pyyaml)")
    import yaml

    with open(path, "r", encoding="utf-8") as fh:
        cfg = yaml.safe_load(fh) or {}
    accounts = (cfg.get("freepi") or {}).get("accounts") or []
    if not accounts:
        sys.exit("no freepi.accounts in %s" % path)
    for acct in accounts:
        if name and acct.get("name") != name:
            continue
        jwt = acct.get("jwt") or ""
        if not jwt:
            continue
        return acct.get("name") or "?", jwt
    sys.exit("no freepi account %s with a jwt in %s" % (name or "(any)", path))


def call(base, path, jwt, body=None, headers=None, timeout=20):
    url = base + path
    data = None
    hdrs = {"authorization": "Bearer " + jwt}
    if body is not None:
        data = json.dumps(body).encode()
        hdrs["content-type"] = "application/json"
    if headers:
        hdrs.update(headers)
    req = urllib.request.Request(url, data=data, headers=hdrs,
                                 method="POST" if data is not None else "GET")
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            raw = resp.read().decode("utf-8", "replace")
            return resp.status, raw
    except urllib.error.HTTPError as err:
        return err.code, err.read().decode("utf-8", "replace")
    except Exception as err:  # noqa: BLE001 - one line is the whole report
        return 0, str(err)


def as_json(raw):
    try:
        return json.loads(raw)
    except (ValueError, TypeError):
        return None


def brief(raw, cap=240):
    text = " ".join((raw or "").split())
    return text[:cap]


def credits_of(me):
    if not isinstance(me, dict):
        return None
    for key in ("remaining_credits_today", "bonus_credits", "credit_usd"):
        if isinstance(me.get(key), (int, float)):
            return me[key]
    return None


def show_me(base, jwt, label):
    code, raw = call(base, "/me", jwt)
    me = as_json(raw)
    if code != 200 or not isinstance(me, dict):
        print("  %-10s /me -> %s %s" % (label, code, brief(raw)))
        return None
    line = "  %-10s /me -> %s credits today" % (
        label, me.get("remaining_credits_today", "?"))
    if isinstance(me.get("allowance_credits"), (int, float)):
        line += " of %s" % me["allowance_credits"]
    for key, tag in (("bonus_credits", "bonus"), ("credit_usd", "bought usd"),
                     ("tier", "tier"), ("lane", "lane")):
        if me.get(key) is not None:
            line += "  %s=%s" % (tag, me[key])
    if me.get("refill_at"):
        line += "  refill=%s" % me["refill_at"]
    print(line)
    return me


def client_version(base, jwt, fallback):
    """GET /client-version, like the cli's catalog poll: x-client-version and
    x-session-id ride it, and `latest` is what every later request declares."""
    code, raw = call(base, "/client-version", jwt,
                     headers={"x-client-version": fallback,
                              "x-session-id": uuid.uuid4().hex})
    body = as_json(raw)
    if code != 200 or not isinstance(body, dict):
        print("  /client-version -> %s (using %s)" % (code, fallback))
        return fallback
    version = body.get("latest") or body.get("min") or fallback
    models = body.get("models") or []
    print("  /client-version -> %s, %d models (min %s)"
          % (version, len(models) if isinstance(models, list) else 0,
             body.get("min") or "-"))
    return version


def redeem(base, jwt, code):
    print("redeem %s" % code)
    status, raw = call(base, "/me/redeem", jwt, body={"code": code})
    body = as_json(raw)
    if status == 200 and isinstance(body, dict) and body.get("ok") is True:
        print("  +%s credits, balance %s"
              % (body.get("credits_added"), body.get("balance")))
        return True
    if isinstance(body, dict) and body.get("code"):
        print("  refused: %s — %s" % (body["code"], body.get("message") or ""))
        return False
    print("  failed: %s %s" % (status, brief(raw)))
    return False


def gravity_pixel(source, imp_url):
    """the cli fires this itself and reports imp_fired; only a gravity ad on a
    trygravity host is ever fetched."""
    if source != "gravity" or not imp_url:
        return False
    host = (urlparse(imp_url).hostname or "").lower()
    if not host.endswith(GRAVITY_HOSTS):
        return False
    try:
        req = urllib.request.Request(imp_url, method="GET")
        with urllib.request.urlopen(req, timeout=3) as resp:
            resp.read(64)
    except Exception:  # noqa: BLE001 - a pixel that fails is still a fired view
        pass
    return True


def ad_cycle(base, jwt, version, session, cycles, dry):
    """banner once, then inline per cycle: what the cli does behind turns."""
    slots = ["banner"] + ["inline"] * max(0, cycles - 1)
    seen = set()
    posted = 0
    for slot in slots:
        path = "/ads/next?slot=" + slot
        headers = {"x-session-id": session}
        if dry:
            print("  GET  %s" % path)
            print("  POST /ads/impression  (imp_fired when source=gravity)")
            continue
        status, raw = call(base, path, jwt, headers=headers)
        if status == 204 or status != 200:
            print("  %-6s /ads/next -> %s (no ad)" % (slot, status or brief(raw)))
            continue
        ad = as_json(raw)
        if not isinstance(ad, dict) or not ad.get("ad_id") or not ad.get("click_token"):
            print("  %-6s /ads/next -> 200 with no ad body" % slot)
            continue
        if ad["click_token"] in seen:
            print("  %-6s /ads/next -> same ad again, skipped" % slot)
            continue
        seen.add(ad["click_token"])
        fired = gravity_pixel(ad.get("source"), ad.get("imp_url"))
        payload = {"ad_id": ad["ad_id"], "click_token": ad["click_token"]}
        if fired:
            payload["imp_fired"] = True
        code, body = call(base, "/ads/impression", jwt, body=payload,
                          headers=headers)
        posted += 1 if code == 200 else 0
        print("  %-6s ad %s source=%s pixel=%s impression -> %s"
              % (slot, ad["ad_id"][:12], ad.get("source") or "-",
                 "fired" if fired else "-", code or brief(body)))
        time.sleep(1.0)
    return posted


def main():
    ap = argparse.ArgumentParser(
        description="redeem a free-pi credit code and run the silent ad cycle")
    ap.add_argument("code", nargs="?", help="FREEPI-XXXX-XXXX from the email")
    ap.add_argument("--config", default="config.yml")
    ap.add_argument("--account", default="", help="account name (default: first)")
    ap.add_argument("--jwt", default="", help="use this jwt instead of config.yml")
    ap.add_argument("--base", default=DEFAULT_BASE)
    ap.add_argument("--ads", type=int, default=1,
                    help="ad cycles to run after redeeming (0 = none)")
    ap.add_argument("--session", default="",
                    help="x-session-id (default: one fresh id per run)")
    ap.add_argument("--client-version", default=DEFAULT_VERSION)
    ap.add_argument("--dry-run", action="store_true",
                    help="print the requests, send nothing")
    args = ap.parse_args()

    if args.jwt:
        name, jwt = "(flag)", args.jwt
    else:
        name, jwt = load_account(args.config, args.account)
    session = args.session or uuid.uuid4().hex
    base = args.base.rstrip("/")
    print("account %s · session %s · %s" % (name, session, base))

    if args.dry_run:
        print("dry run: nothing sent")

    show_me(base, jwt, "before")
    if args.code:
        if args.dry_run:
            print('  POST /me/redeem  {"code": "%s"}' % args.code)
        else:
            redeem(base, jwt, args.code)
            show_me(base, jwt, "after")
    elif args.ads == 0:
        print("nothing to do: pass a code, --ads, or both")

    if args.ads > 0:
        version = args.client_version
        if not args.dry_run:
            version = client_version(base, jwt, version)
        print("ad cycle (x-client-version %s)" % version)
        posted = ad_cycle(base, jwt, version, session, args.ads, args.dry_run)
        if not args.dry_run:
            print("  %d impression(s) posted" % posted)
            show_me(base, jwt, "after ads")


if __name__ == "__main__":
    main()
