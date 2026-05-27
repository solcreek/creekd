# Self-Host Quickstart

From zero to running app on a VPS in 15 minutes.

## Prerequisites

- A VPS running Debian 12 or Ubuntu 24.04 (e.g., Hetzner CX22 @ €4/mo)
- Root SSH access
- A domain name with DNS pointing to your VPS IP

## 1. Install creekd (2 min)

```bash
curl -fsSL https://raw.githubusercontent.com/solcreek/creekd/main/install.sh | sh
```

This installs `creekd` + `creekctl` to `/usr/local/bin/`.

## 2. Set up systemd service (1 min)

```bash
# install.sh ships only the binaries; fetch the systemd unit
# directly from the repo. The shipped unit hardens the daemon
# (NoNewPrivileges, ProtectSystem=strict, etc.) and points
# CREEKD_STATE_DIR / CREEKD_LOG_DIR at /var/lib/creekd/{state,logs}.
sudo curl -fsSL https://raw.githubusercontent.com/solcreek/creekd/main/init/creekd.service \
  -o /etc/systemd/system/creekd.service

sudo mkdir -p /var/lib/creekd/state /var/lib/creekd/logs

# Set admin token. The unit reads /etc/creekd/env as an optional
# EnvironmentFile, so anything you put here overrides the unit's
# Environment= lines.
sudo mkdir -p /etc/creekd
echo "CREEKD_ADMIN_TOKEN=$(openssl rand -hex 32)" | sudo tee /etc/creekd/env

sudo systemctl daemon-reload
sudo systemctl enable creekd
sudo systemctl start creekd

# Verify the shipped hardening directives are intact on disk
# (catches operator edits that weaken the unit).
creekctl hardening-check /etc/systemd/system/creekd.service
```

Verify:
```bash
creekctl ps
# Should show empty app list
```

## 3. Install Caddy for auto SSL (2 min)

```bash
sudo apt install -y debian-keyring debian-archive-keyring apt-transport-https curl
curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/gpg.key' | \
  sudo gpg --dearmor -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg
curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt' | \
  sudo tee /etc/apt/sources.list.d/caddy-stable.list
sudo apt update && sudo apt install -y caddy
```

Configure Caddy:
```bash
# Single app
sudo tee /etc/caddy/Caddyfile << 'EOF'
my-app.example.com {
    reverse_proxy 127.0.0.1:9000 {
        header_up X-Creek-App my-app
    }
}
EOF

sudo systemctl restart caddy
```

## 4. Deploy your first app (5 min)

On your VPS:
```bash
# Clone your app
git clone https://github.com/you/my-app.git
cd my-app

# Install runtime (if not already)
curl -fsSL https://bun.sh/install | bash

# Deploy
creekctl ensure my-app \
  --runtime bun \
  --entry src/index.ts \
  --port 3000
```

Verify:
```bash
creekctl ps
# my-app   running   pid:1234   port:3000

curl -H "X-Creek-App: my-app" http://127.0.0.1:9000/
# Your app responds!

# With Caddy configured:
curl https://my-app.example.com/
# HTTPS with auto cert!
```

## 5. Monitor (ongoing)

```bash
# App status
creekctl ps
creekctl stats my-app

# Logs
creekctl logs my-app

# Event stream
creekctl events my-app

# One-off command (--app picks the env to inherit; omitting it
# uses whichever app comes back first, which is ambiguous in a
# multi-app setup — always pass --app once you have more than one).
creekctl exec --app my-app -- bun run seed.ts
```

## What's running

```
Internet → Caddy (:443, auto SSL) → creekd dispatch (:9000) → app (:3000)
                                     creekd admin (:9080) ← creekctl
```

Total overhead: ~25MB (creekd ~10MB + Caddy ~15MB).
Your app gets the rest of the VPS memory.

## Next steps

- [Add more apps](../examples/pm2-replacement/) — creekd handles multi-app routing
- [cgroup limits](../docs/CONFIG.md) — per-app memory + CPU caps
- [Blue-green deploy](../docs/DESIGN.md) — zero-downtime updates
- [Monitoring](../examples/observability/) — Prometheus + Grafana
