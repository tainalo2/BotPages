#!/usr/bin/env bash
# Publie la mini-app "espace membre" (Epic 5 : comptes et sessions
# utilisateurs finaux) sur le déploiement préprod BotPages via l'endpoint
# MCP publish_app. Même pattern que counter-app/publish.sh et
# guestbook-app/publish.sh.
#
# Prérequis (fournis par ops, absents de cet environnement) :
#   BOTPAGES_TOKEN  — jeton bot ou publisher Bearer pour le déploiement cible
#   SITE            — nom de site (un publisher peut utiliser tout site non
#                      répertorié ; sinon site déjà attribué au bot)
#
# Usage: BOTPAGES_TOKEN=... SITE=mon-site ./publish.sh
set -euo pipefail

: "${BOTPAGES_TOKEN:?export BOTPAGES_TOKEN=<jeton bot/publisher>}"
: "${SITE:?export SITE=<nom-du-site>}"

HOST="${BOTPAGES_HOST:-https://botpages.alexandre-rongier.fr}"
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

PAYLOAD=$(python3 - "$SITE" <<PY
import json, sys
site = sys.argv[1]
manifest = json.load(open("$DIR/manifest.json"))
entry = open("$DIR/entry.js").read()
body = {
    "jsonrpc": "2.0",
    "id": 1,
    "method": "tools/call",
    "params": {
        "name": "publish_app",
        "arguments": {"site": site, "manifest": manifest, "entry": entry},
    },
}
print(json.dumps(body))
PY
)

curl -sS -X POST "$HOST/mcp" \
  -H "Authorization: Bearer $BOTPAGES_TOKEN" \
  -H "Content-Type: application/json" \
  -d "$PAYLOAD"
echo
echo "Mini-app publiée. URL attendue : $HOST/a/<slug-du-site>/espace-membre/"
