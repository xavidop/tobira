#!/bin/bash
# Bootstraps a single-node Vertica database inside the official
# opentext/vertica-k8s image, which is built for the VerticaDB operator and
# so ships no entrypoint. The integration suite runs this as root through
# testcontainers; it creates the dbadmin user the image expects (uid 998),
# self-signed HTTPS certificates for the node management agent, and then
# a database named tobira whose dbadmin password is $VERTICA_PASSWORD.
# Test-only: never use these certificates or this layout for real data.
set -euo pipefail

PASSWORD="${VERTICA_PASSWORD:-tobira}"

if ! getent passwd dbadmin >/dev/null; then
    echo "dbadmin:x:998:996:dbadmin:/home/dbadmin:/bin/bash" >>/etc/passwd
fi
if ! getent group 996 >/dev/null; then
    echo "verticadba:x:996:dbadmin" >>/etc/group
fi
mkdir -p /data/tmp /data/db
chown -R 998:996 /data /home/dbadmin

runuser -u dbadmin -- bash -s -- "$PASSWORD" <<'INNER'
set -euo pipefail
PASSWORD="$1"
export HOME=/home/dbadmin
cd /data/tmp
openssl req -x509 -newkey rsa:2048 -nodes -keyout ca.key -out rootca.pem -days 30 \
    -subj /CN=tobira-test-ca -addext basicConstraints=critical,CA:TRUE \
    -addext keyUsage=critical,keyCertSign,cRLSign 2>/dev/null
openssl req -newkey rsa:2048 -nodes -keyout srv.key -out srv.csr -subj /CN=localhost 2>/dev/null
printf "subjectAltName=DNS:localhost,IP:127.0.0.1\nextendedKeyUsage=serverAuth,clientAuth\n" >ext
openssl x509 -req -in srv.csr -CA rootca.pem -CAkey ca.key -CAcreateserial -out srv.pem \
    -days 30 -extfile ext 2>/dev/null
certs=/opt/vertica/config/https_certs
cp rootca.pem "$certs/rootca.pem"
cp srv.pem "$certs/vertica_https.pem"
cp srv.key "$certs/vertica_https.key"
j() { awk '{printf "%s\\n", $0}' "$1"; }
openssl rsa -in srv.key -traditional -out srv.rsa.key 2>/dev/null
printf '{"name": "https", "mode": 2, "key": "%s", "certificate": "%s", "chain_certs": [], "ca_certificates": ["%s"]}' \
    "$(j srv.rsa.key)" "$(j srv.pem)" "$(j rootca.pem)" >httpstls.json

cd /data
setsid /opt/vertica/bin/node_management_agent >/data/nma.out 2>&1 </dev/null &
for _ in $(seq 1 60); do
    grep -q "Starting HTTP listener" /opt/vertica/log/node_management_agent.log 2>/dev/null && break
    sleep 1
done

vcluster create_db --db-name tobira --hosts "$(hostname -i | awk '{print $1}')" \
    --catalog-path /data/db --data-path /data/db \
    --config-param HttpServerConf=/data/tmp/httpstls.json \
    --cert-file /data/tmp/srv.pem --key-file /data/tmp/srv.key --ca-cert-file /data/tmp/rootca.pem \
    --password "$PASSWORD" --skip-package-install --startup-timeout 5 --config /data/vertica_cluster.yaml >/data/create_db.out 2>&1 || true
# vcluster's own startup poll goes over the node's HTTPS service, which does
# not accept these throwaway certificates; the SQL port is what the tests
# need, so poll it instead.
for _ in $(seq 1 180); do
    if vsql -U dbadmin -w "$PASSWORD" -At -c "SELECT 1" >/dev/null 2>&1; then
        echo "tobira: database ready"
        exit 0
    fi
    sleep 1
done
tail -20 /data/create_db.out
exit 1
INNER
