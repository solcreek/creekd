#!/bin/bash
set -eux
export DEBIAN_FRONTEND=noninteractive

install -d /usr/share/postgresql-common/pgdg
curl -fsSL https://www.postgresql.org/media/keys/ACCC4CF8.asc \
  -o /usr/share/postgresql-common/pgdg/apt.postgresql.org.asc
echo "deb [signed-by=/usr/share/postgresql-common/pgdg/apt.postgresql.org.asc] \
  https://apt.postgresql.org/pub/repos/apt $(lsb_release -cs)-pgdg main" \
  > /etc/apt/sources.list.d/pgdg.list
apt-get update
apt-get install -y postgresql-16

sudo -u postgres psql -c "ALTER USER postgres PASSWORD 'creek_sandbox';"
mkdir -p /etc/postgresql/16/main/conf.d
cat > /etc/postgresql/16/main/conf.d/creek.conf <<EOF
listen_addresses = '127.0.0.1'
EOF
echo "host all all 127.0.0.1/32 md5" >> /etc/postgresql/16/main/pg_hba.conf
systemctl restart postgresql
systemctl enable postgresql
