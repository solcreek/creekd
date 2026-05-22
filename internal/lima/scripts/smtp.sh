#!/bin/bash
set -eux

curl -fsSL "https://github.com/axllent/mailpit/releases/latest/download/mailpit-linux-amd64.tar.gz" -o /tmp/mailpit.tar.gz
tar -xzf /tmp/mailpit.tar.gz -C /usr/local/bin/ mailpit
chmod +x /usr/local/bin/mailpit
rm -f /tmp/mailpit.tar.gz

cat > /etc/systemd/system/mailpit.service << 'EOF'
[Unit]
Description=Mailpit SMTP Testing Server
After=network.target

[Service]
ExecStart=/usr/local/bin/mailpit --smtp 127.0.0.1:1025 --listen 127.0.0.1:8025 --db-file /var/lib/mailpit/mailpit.db
Restart=always
RestartSec=2

[Install]
WantedBy=multi-user.target
EOF

mkdir -p /var/lib/mailpit
systemctl daemon-reload
systemctl enable mailpit
systemctl start mailpit

mailpit version
