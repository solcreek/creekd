#!/bin/bash
set -eux
export DEBIAN_FRONTEND=noninteractive

apt-get update -qq
apt-get install -y --no-install-recommends mariadb-server mariadb-client
systemctl enable mariadb
systemctl start mariadb

mariadb -e "CREATE USER IF NOT EXISTS 'creek'@'localhost' IDENTIFIED BY 'creek_sandbox';"
mariadb -e "GRANT ALL PRIVILEGES ON *.* TO 'creek'@'localhost';"
mariadb -e "FLUSH PRIVILEGES;"
