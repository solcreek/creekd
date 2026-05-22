# System integration files

## creekd.service

Systemd unit for running creekd as a system service.

```bash
# Install
sudo cp init/creekd.service /etc/systemd/system/
sudo mkdir -p /var/lib/creekd/state /var/lib/creekd/logs
sudo systemctl daemon-reload
sudo systemctl enable creekd
sudo systemctl start creekd

# Check
sudo systemctl status creekd
journalctl -u creekd -f

# Override env vars
sudo mkdir -p /etc/creekd
echo 'CREEKD_ADMIN_TOKEN=my-secret-token' | sudo tee /etc/creekd/env
sudo systemctl restart creekd
```

The install script (`install.sh`) does NOT install the systemd unit
automatically — the user opts in by copying it. This keeps the install
script non-invasive on systems that don't use systemd.
