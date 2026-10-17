# Security

Homestack controls Docker (through its socket) and, with the host agent, the server's disks, network and power. A hole in it is a hole in the whole server, so security reports are welcome and taken seriously.

## Reporting a vulnerability

Please **don't open a public issue**. Report it privately instead: on GitHub, **Security → Report a vulnerability** in this repository. Only the maintainer sees it until it's fixed.

Include what you can of:

- the Homestack version (Settings → System → About, or `homestack version`)
- what an attacker could do, and from where (the LAN, signed in or not, another container on the server)
- how to reproduce it

You'll get a reply as soon as possible. Once there's a fix, it's released and the report is published as an advisory, crediting you unless you'd rather not be named.

## What's in scope

Anything that gets around what Homestack is meant to enforce, for example:

- signing in without the password, or getting past the network allowlist
- reaching files outside the shares and disks from the Files page (path traversal)
- running commands on the server through Homestack or its host agent
- cross-site request forgery or scripting in the web UI
- secrets (passwords, tokens, backup passphrases) leaking into logs, backups or responses

A Homestack admin, or anyone with a shell on the server, can already control Docker by design (see **Security model** in the README), so a problem that needs either is out of scope unless it reaches further than that.

## Supported versions

Fixes go into the next release; there are no backports. Run the latest release.
