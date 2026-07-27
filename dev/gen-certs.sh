#!/usr/bin/env bash
# Generate a local dev CA + a leaf cert that mesh-router-caddy serves for every
# gateway-routed host (auth-<DOMAIN>, maison-<DOMAIN>, <app>-<DOMAIN>, …).
#
# Output (dev/certs/):
#   ca.pem / ca.key   the dev root CA — trust ca.pem in your browser and in app
#                     containers (NODE_EXTRA_CA_CERTS) to silence warnings and
#                     let OIDC back-channel TLS validate.
#   cert.pem / key.pem the leaf served by Caddy (mounted at /certs).
#
# SANs cover *.localhost + localhost (the default DOMAIN=app.localhost) plus the
# 127-0-0-1.nip.io fallback. If you use a different DOMAIN, add its SANs below.
set -euo pipefail
cd "$(dirname "$0")/certs" 2>/dev/null || { mkdir -p "$(dirname "$0")/certs"; cd "$(dirname "$0")/certs"; }

if [ -f cert.pem ] && [ "${1:-}" != "--force" ]; then
  echo "certs already present (use --force to regenerate); nothing to do."
  exit 0
fi

openssl req -x509 -newkey rsa:2048 -nodes -keyout ca.key -out ca.pem -days 3650 \
  -subj "/CN=Maison Dev Local CA"

openssl req -newkey rsa:2048 -nodes -keyout key.pem -out leaf.csr \
  -subj "/CN=*.localhost"

cat > san.ext <<'EOF'
subjectAltName=DNS:localhost,DNS:*.localhost,DNS:*.127-0-0-1.nip.io,DNS:127-0-0-1.nip.io
extendedKeyUsage=serverAuth
EOF

openssl x509 -req -in leaf.csr -CA ca.pem -CAkey ca.key -CAcreateserial \
  -out cert.pem -days 3650 -extfile san.ext

rm -f leaf.csr san.ext ca.srl
echo "Wrote dev/certs/{ca.pem,cert.pem,key.pem}"
openssl x509 -in cert.pem -noout -ext subjectAltName
