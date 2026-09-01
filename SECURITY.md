# Security

## Reporting a vulnerability

Please report security issues privately through GitHub's [private
vulnerability reporting](https://docs.github.com/en/code-security/security-advisories/guidance-on-reporting-and-writing-information-about-vulnerabilities/privately-reporting-a-security-vulnerability)
on this repository, rather than opening a public issue.

This is a single-maintainer hobby project, so please do not expect a
same-day reply. Include what you did, what happened, and what you
expected instead.

## What the threat model is

The phone remote is the only network-facing part, and it is built for a
private network:

- It binds to localhost and, when the machine has one, its Tailscale
  address. It never listens on your LAN or the internet unless you add
  addresses to `remote.bind` yourself.
- Logins travel over plain HTTP. That is safe on a tailnet, which is
  encrypted end to end. On a LAN it is a password in clear text, which is
  your call for a network you trust. Behind a TLS-terminating reverse
  proxy the login cookie is marked secure automatically.
- Requests whose `Host` or `Origin` is not a bound address or an entry in
  `remote.allowed_hosts` are refused, as a cross-site and DNS-rebinding
  defense.
- Passwords are stored as bcrypt hashes. Failed logins are rate-limited
  per address and per account, and the login page never reveals whether
  an account exists.

Exposing the port directly to the internet is out of scope, and not
something the project supports.

## What is not a vulnerability

- Anything that requires an already-authenticated admin account. Admins
  can manage users and delete data by design.
- The engine's own HTTP API. It listens on localhost only and is treated
  as a trusted local component.
- Model output. Generated lyrics and audio come from a third-party
  model and are not filtered.
