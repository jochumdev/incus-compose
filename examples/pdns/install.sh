#!/usr/bin/env bash
set -euo pipefail

# Based on: https://computingforgeeks.com/running-powerdns-and-powerdns-admin-in-docker-containers/

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" >/dev/null 2>&1 && pwd)"

source "${SCRIPT_DIR}/.env"

if [ "${DB_PASSWORD}" == "superSecret" ] || [ "${DB_ROOT_PASSWORD}" == "superRootSecret" ] || [ "${API_KEY}" == "superAdminKeySecret" ]; then
  echo "Please adjust your .env first"
  exit 1
fi

mkdir -p "${SCRIPT_DIR}/work"

echo "Creating configs from templates"
sed "s/DB_NAME_HERE/${DB_NAME}/g;
     s/DB_USER_HERE/${DB_USER}/g;
     s/DB_PASS_HERE/${DB_PASSWORD}/g;
     s/API_KEY_HERE/${API_KEY}/g" "${SCRIPT_DIR}/pdns/pdns.conf.template" > "${SCRIPT_DIR}/pdns/pdns.conf"

sed "s/ZONE/${ZONE}/g;
     s/PDNS_IPV4_ADDRESS/${PDNS_IPV4_ADDRESS}/g" "${SCRIPT_DIR}/dnscrypt-proxy/forwarding-rules.txt.template" > "${SCRIPT_DIR}/dnscrypt-proxy/forwarding-rules.txt"

echo "Creating powerdns to copy the schema from it"
incus-compose up --no-start --no-deps pdns --detach

echo "Copying the schema from pdns-auth"
incus-compose incus file pull pdns-auth/usr/local/share/doc/pdns/schema.mysql.sql "${SCRIPT_DIR}/work/schema.mysql.sql"

echo "Starting mariadb"
incus-compose up --no-deps mariadb --detach

echo "Importing the PDNS schema"
incus-compose exec -e MYSQL_PWD="${DB_ROOT_PASSWORD}" mariadb mariadb -uroot "${DB_NAME}" < "${SCRIPT_DIR}/work/schema.mysql.sql"

echo "Creating the pdns-admin database: ${ADMIN_DB_NAME}"
incus-compose exec -e MYSQL_PWD="${DB_ROOT_PASSWORD}" mariadb mariadb -uroot -e \
  "DROP DATABASE IF EXISTS ${ADMIN_DB_NAME};
  CREATE DATABASE ${ADMIN_DB_NAME} CHARACTER SET utf8mb4;
  GRANT ALL PRIVILEGES ON ${ADMIN_DB_NAME}.* TO '${DB_USER}'@'%';
  FLUSH PRIVILEGES;"

echo "Starting pdns"
incus-compose up --no-deps pdns --detach

echo "Creating your zone: ${ZONE}"
incus-compose exec pdns pdnsutil create-zone "${ZONE}"

echo "Starting the project"
incus-compose up --detach
