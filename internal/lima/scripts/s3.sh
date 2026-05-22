#!/bin/bash
set -eux

SWFS_VERSION="4.21"
curl -fsSL "https://github.com/seaweedfs/seaweedfs/releases/download/${SWFS_VERSION}/linux_amd64.tar.gz" -o /tmp/seaweedfs.tar.gz
tar -xzf /tmp/seaweedfs.tar.gz -C /usr/local/bin/
chmod +x /usr/local/bin/weed
rm -f /tmp/seaweedfs.tar.gz

mkdir -p /var/lib/seaweedfs

cat > /etc/systemd/system/seaweedfs.service << 'EOF'
[Unit]
Description=SeaweedFS S3 Server
After=network.target

[Service]
ExecStart=/usr/local/bin/weed server -s3 -dir=/var/lib/seaweedfs -s3.port=8333 -master.port=9333 -volume.port=8080
Restart=always
RestartSec=2

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable seaweedfs
systemctl start seaweedfs

weed version
