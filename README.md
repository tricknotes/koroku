<img src="docs/assets/logo.svg" alt="" width="72" height="72" align="right">

# koroku

**The one-person PaaS** — *Platform as a Self-hosted application*

Your compose.yaml, on one VPS you own, with guardrails.

English | [日本語](README.ja.md)

koroku deploys the Docker Compose apps in your GitHub repositories to a single VPS you own, from a web console. It gives you a Heroku-like workflow on your own server, at a scale one person can run.

> [!NOTE]
> v0.1.0 is an early, experimental release. Settings and the API may still change. The console and the detailed documents are in Japanese.

## The problem it solves

- **PaaS bills grow with every app.** Even small personal apps and staging environments cost a monthly fee per app and per database. With koroku, three to five apps share the cost of one server.
- **Hand-run VPSes break easily.** Wiring up `docker compose up`, a reverse proxy, certificates, backup cron jobs and log storage yourself leaves room for a failed deploy to take production down, or for backups that were never actually taken. koroku puts these together and adds guardrails.
- **Existing self-hosted PaaSes don't fit your Compose file, or are too big.** Many expect buildpacks or their own config format, or target multiple servers and use a lot of memory for the platform itself. koroku uses your repository's Compose file without rewriting it, and runs as a single Go binary with SQLite.

## What it does

- **Deploys**: pick a branch or commit, or push to GitHub, and koroku fetches, builds, starts and checks the app over HTTP. If the new version fails to start, it goes back to the last working one. Optionally, it replaces the public service with zero downtime.
- **Guardrails**: risky settings such as `privileged` or mounting the Docker socket are refused before deploying, app ports are never exposed, and apps cannot see each other. Production apps ask for confirmation before deploys and deletion.
- **URLs and TLS**: every app gets `https://<app>.<base domain>` with a certificate. Custom domains can be added.
- **Data protection**: named volumes, database dumps and koroku itself are backed up daily to object storage and restorable from the console. You can recover even after losing the whole server.
- **Fits a small server**: per-service memory limits, production kept alive first when memory runs out, and sleeping apps that get no traffic.
- **Heroku-like workflow**: staging and production, review apps per pull request, environment variables, logs and one-off commands in one console.
- **Works with AI agents**: Claude Code Skills prepare your app's repository for koroku, and read and fix the cause of a failed deploy.

## Who it is for

- You want to run three to five apps you wrote (a web app plus a database, and so on) on one server
- Your apps already run with Docker Compose, or can be made to
- One person runs it (plus a few people you trust with specific apps)

If you need multiple servers, autoscaling, `git push` and buildpack deploys, or ready-made app templates, another tool will fit better. See [docs/COMPARISON.md](docs/COMPARISON.md) (in Japanese).

## How it works

```text
            Internet
               | 80 / 443
+--------------v--------------------------------------- server --+
|  Caddy          console.<base domain> -> koroku (console)       |
|                 <app>.<base domain>   -> each app               |
|  koroku         API, console, deploys, logs, backups            |
|                 drives docker compose and routes Caddy          |
|  apps           one app = one Compose project                   |
+-----------------------------------------------------------------+
       |                     |                          |
     GitHub            object storage             Let's Encrypt
 (login, fetch)     (log archive, backups)       (TLS certificates)
```

koroku does not depend on any particular hosting, DNS or object storage provider.

## Requirements

| What | Requirement |
| --- | --- |
| Server | One VPS with Ubuntu 24.04 or 26.04 (amd64). Around 2 vCPU, 4GB memory and 40GB disk. A static public IPv4 address with ports 80 and 443 reachable |
| Domain | `*.<base domain>` (or the console and each app's name) pointed at the server's IP |
| GitHub | One GitHub App registered on your account |
| Object storage | Anything with an S3-compatible API, for log archives and backups (koroku runs without it, but cannot take backups) |
| Local machine | Docker, to build the `koroku` binary |

## Getting started

1. Run `bin/dev make build` locally to build `bin/koroku` with the console embedded.
2. Prepare the server, DNS, GitHub App and object storage.
3. Copy the binary and `deploy/` to the server and run `deploy/provision.sh`.
4. Write `/etc/koroku/config.toml`, start koroku, and check the setup with `sudo koroku check`.
5. Open `https://console.<base domain>` and sign in with GitHub.

Each step is described in [docs/SETUP.md](docs/SETUP.md) (in Japanese).

## Security

koroku drives Docker, so it holds the same power as root on the server. The console login, matched by GitHub's numeric user ID, is the only gate. Deploy only repositories from yourself and people you trust. See [docs/USAGE.md](docs/USAGE.md#セキュリティについて) and chapter 10 of [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) (in Japanese).

## Documents

The documents below are in Japanese.

| Document | Contents |
| --- | --- |
| [docs/COMPARISON.md](docs/COMPARISON.md) | Comparison with other tools |
| [docs/SETUP.md](docs/SETUP.md) | First-time setup (server, DNS, GitHub App, object storage) |
| [docs/USAGE.md](docs/USAGE.md) | Using the console, and the Skills that prepare your app's repository |
| [docs/OPERATIONS.md](docs/OPERATIONS.md) | Building, updating and recovering the server |
| [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) | Design |
| [docs/API.md](docs/API.md) | HTTP API |
| [docs/DEVELOPMENT.md](docs/DEVELOPMENT.md) | Developing koroku itself |
