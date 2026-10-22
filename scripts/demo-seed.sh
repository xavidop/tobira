#!/usr/bin/env bash
# Seeds the SQLite database the README demo (demo.tape) records against, using
# the real tobira binary so nothing else needs installing. Usage:
#   scripts/demo-seed.sh ./bin/tobira /tmp/tobira-demo.db /tmp/tobira-demo/config.yaml
set -euo pipefail

bin=${1:?usage: demo-seed.sh <tobira-binary> <db-path> <config-path>}
db=${2:?db path}
cfg=${3:?config path}

rm -f "$db"
mkdir -p "$(dirname "$cfg")"
cat >"$cfg" <<YAML
connections:
  - name: shop
    dsn: sqlite:$db
tui:
  keymap: default
YAML

q() { TOBIRA_CONFIG="$cfg" "$bin" query "sqlite:$db" "$1" >/dev/null; }

q "create table customers (id integer primary key, name text not null, city text, plan text)"
q "insert into customers (name, city, plan) values
  ('Mochi Tanaka', 'Osaka', 'pro'), ('Tama Sato', 'Kyoto', 'free'), ('Hana Ito', 'Tokyo', 'pro'),
  ('Ren Kimura', 'Sapporo', 'team'), ('Yuki Mori', 'Nagoya', 'free'), ('Sora Abe', 'Fukuoka', 'pro'),
  ('Aoi Hayashi', 'Kobe', 'team'), ('Riku Ono', 'Sendai', 'free'), ('Mio Ishii', 'Tokyo', 'pro'),
  ('Kaito Endo', 'Osaka', 'free')"
q "create table orders (id integer primary key, customer_id integer not null references customers(id), total real not null, status text not null)"
q "insert into orders (customer_id, total, status) values
  (1, 42.5, 'paid'), (1, 18.0, 'paid'), (2, 9.99, 'refunded'), (3, 120.0, 'paid'), (3, 35.25, 'pending'),
  (4, 64.0, 'paid'), (6, 15.5, 'paid'), (7, 80.0, 'pending'), (9, 27.0, 'paid'), (9, 52.75, 'paid')"
echo "seeded $db"
