"""Manual live SMTP acceptance test using only an inbox the operator controls.

Creates and deactivates a temporary MEA account. Reads only messages addressed
to its unique plus alias. Passwords, cookies and link tokens stay in memory.
No AWS helper or database token access is used.
"""

import argparse
from datetime import datetime, timezone
import email
from email import policy
import http.cookiejar
import imaplib
import json
from pathlib import Path
import re
import secrets
import ssl
import time
from urllib import error, request


def env_password(path):
    for line in Path(path).read_text(encoding="utf-8-sig").splitlines():
        if line.lstrip().startswith("#") or "=" not in line:
            continue
        key, value = line.split("=", 1)
        if key.strip().removeprefix("export ") != "PASSWORD":
            continue
        value = value.strip()
        if len(value) >= 2 and value[0] == value[-1] and value[0] in "\"'":
            value = value[1:-1]
        if value:
            return value
    raise ValueError("The private env file needs a nonempty PASSWORD")


class Client:
    def __init__(self, origin):
        self.origin = origin.rstrip("/")
        self.http = request.build_opener(request.HTTPCookieProcessor(http.cookiejar.CookieJar()))
        self.csrf = ""

    def call(self, path, method="GET", body=None, expected=200):
        data = None if body is None else json.dumps(body).encode()
        headers = {"Content-Type": "application/json", "Origin": self.origin, "X-CSRF-Token": self.csrf}
        req = request.Request(self.origin + "/api" + path, data=data, headers=headers, method=method)
        try:
            response = self.http.open(req, timeout=30)
        except error.HTTPError as e:
            response = e
        with response:
            if response.status != expected:
                # Do not print response bodies, cookies or request data.
                raise RuntimeError(f"{method} {path} returned {response.status}; expected {expected}")
            result = json.load(response)
        if "csrf" in result:
            self.csrf = result["csrf"]
        return result


def receive(imap, recipient, subject, origin, page):
    deadline = time.monotonic() + 150
    while time.monotonic() < deadline:
        imap.noop()
        status, values = imap.uid("search", None, "UNDELETED", "TO", f'"{recipient}"', "SUBJECT", f'"{subject}"')
        if status != "OK":
            raise RuntimeError("IMAP search failed")
        for uid in reversed(values[0].split()):
            status, raw = imap.uid("fetch", uid, "(BODY.PEEK[])")
            if status != "OK":
                continue
            payload = next((item[1] for item in raw if isinstance(item, tuple)), None)
            if payload is None:
                continue
            message = email.message_from_bytes(payload, policy=policy.default)
            if recipient not in str(message.get("To", "")) or str(message.get("Subject", "")) != subject:
                continue
            part = message.get_body(preferencelist=("plain",))
            text = part.get_content() if part else ""
            match = re.search(re.escape(origin.rstrip("/") + "/" + page + "#token=") + r"([A-Za-z0-9_-]+)", text)
            if match:
                return match.group(1), {
                    "uid": uid.decode(),
                    "messageId": str(message.get("Message-ID", "")),
                    "from": str(message.get("From", "")),
                    "dkimSignaturePresent": "DKIM-Signature" in message,
                    "authenticationResults": str(message.get("Authentication-Results", "")),
                }
        time.sleep(3)
    raise TimeoutError("Transactional message did not reach the test alias within 150 seconds")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--password-file", required=True, help="Private env file containing PASSWORD; never pass the password itself")
    parser.add_argument("--mailbox", required=True, help="Owned mailbox supporting plus aliases")
    parser.add_argument("--server", required=True)
    parser.add_argument("--origin", default="https://miseenaby.me")
    parser.add_argument("--output", default="test-results/smtp-live.json")
    args = parser.parse_args()
    if not args.origin.startswith("https://"):
        parser.error("Live mail tests require HTTPS")
    local, domain = args.mailbox.rsplit("@", 1)
    suffix = str(time.time_ns())[-15:]
    recipient = local + "+mea-test-" + suffix + "@" + domain
    client = Client(args.origin)
    password, replacement = secrets.token_urlsafe(32), secrets.token_urlsafe(32)
    imap = imaplib.IMAP4_SSL(args.server, 993, ssl_context=ssl.create_default_context(), timeout=15)
    imap.login(args.mailbox, env_password(args.password_file))
    imap.select("INBOX", readonly=True)
    report = {"time": datetime.now(timezone.utc).isoformat(), "origin": args.origin, "recipient": recipient, "checks": []}
    registered = False
    active_password = password

    def passed(label):
        report["checks"].append(label)
        print(label, flush=True)

    try:
        result = client.call("/auth/register", "POST", {"Username": "meamail" + suffix, "Email": recipient, "Name": "MEA SMTP acceptance test", "Password": password}, expected=201)
        registered = True
        if not result["verificationRequired"]:
            raise RuntimeError("Email verification was unexpectedly disabled")
        client.call("/desktops", "POST", {}, expected=403)
        passed("Unverified account cannot launch a desktop")
        token, report["verificationMessage"] = receive(imap, recipient, "Verify your Mise En Abyme account", args.origin, "verify")
        passed("Verification email received through real SMTP and IMAP")
        client.call("/auth/verify", "POST", {"Token": token})
        if not client.call("/me")["user"]["verified"]:
            raise RuntimeError("Delivered link did not verify the account")
        client.call("/auth/verify", "POST", {"Token": token}, expected=400)
        passed("Delivered verification link works once")
        client.call("/auth/forgot", "POST", {"Email": recipient})
        token, report["resetMessage"] = receive(imap, recipient, "Reset your Mise En Abyme password", args.origin, "reset")
        passed("Password-reset email received through real SMTP and IMAP")
        client.call("/auth/reset", "POST", {"Token": token, "Password": replacement})
        active_password = replacement
        client.call("/me", expected=401)
        client.call("/auth/reset", "POST", {"Token": token, "Password": replacement}, expected=400)
        client.call("/auth/login", "POST", {"Email": recipient, "Password": password}, expected=401)
        client.call("/auth/login", "POST", {"Email": recipient, "Password": replacement})
        passed("Reset revokes old session, rejects replay and old password, and permits new login")
    finally:
        try:
            if registered:
                client.call("/auth/login", "POST", {"Email": recipient, "Password": active_password})
                client.call("/account", "DELETE", {"Password": active_password})
                passed("Temporary account deactivated")
        finally:
            imap.logout()
            output = Path(args.output)
            output.parent.mkdir(parents=True, exist_ok=True)
            output.write_text(json.dumps(report, indent=2) + "\n", encoding="utf-8")


if __name__ == "__main__":
    main()
