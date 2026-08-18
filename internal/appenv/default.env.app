# .env.app — the variables Maison forwards into every app.
#
# This file belongs to the DEPLOYMENT, not to Maison. On a Yundera PCS it is
# written by the orchestrator; on a plain install it is yours to edit. Maison
# creates it once, with the defaults below, and never overwrites it again.
#
# On install, and again on every start, Maison reads this file and ensures each
# key in the app's own .env — a key already there is set to the value here, a key
# missing is appended. Order does not matter, in either file. Nothing else in an
# app's .env is touched: keys you add there yourself are the app's, and survive.
#
# That is the whole separation. What an app receives is stated here. What Maison
# needs to run itself stays in its own environment (DATA_ROOT, APPSTORE_URL,
# HTTP_ADDR, …) and is never forwarded.
#
# A few variables are NOT listed here because Maison derives them per app and
# per install — AppID, PUID, PGID, TZ, DATA_ROOT, DATA_HOST_PATH. They are merged
# in automatically. Setting them here has no effect.
#
# An empty value is treated as "not set": the key is skipped rather than written
# blank, so an app never ends up with APP_DOMAIN= and a Caddy route pointing at
# nothing. Comment a line out, or leave it empty, to not forward it.

# --- placement -------------------------------------------------------------
# The external Docker network every app's main service is attached to. It must
# already exist. Empty = attach no network. Maison's own compose creates `mesh`;
# a Yundera PCS uses `pcs`.
APP_NET=mesh

# --- routing ---------------------------------------------------------------
# The deployment's base domain and public IP. Store apps template their Caddy
# labels with these (`myapp-${APP_DOMAIN}`, `myapp-${APP_PUBLIC_IP_DASH}.sslip.io`),
# so if the box moves, every app follows on its next start. Leave empty on a local
# install with no reverse proxy — apps then have no reachable web address.
APP_DOMAIN=
APP_PUBLIC_IP=
APP_PUBLIC_IP_DASH=
APP_PUBLIC_IPV4=
APP_PUBLIC_IPV4_DASH=
APP_PUBLIC_IPV6=
APP_PUBLIC_IPV6_DASH=

# Lowercase alias: some store apps' x-compose-app `webui-host` uses ${domain}.
domain=

# --- identity --------------------------------------------------------------
# Seeded into apps that provision an admin account on first boot. These are
# consumed once, when the app initialises its own database — changing them later
# does not rotate anything already provisioned.
#
# Keep the password at 12 characters or more. Several store apps refuse a shorter
# one outright (FileBrowser >= v2.63 rejects it with "password is too short,
# minimum length is 12" and the install fails in its pre-install hook), and an app
# that enforces a minimum is the norm rather than the exception.
#
# This is a placeholder, not a secret: it is the same on every plain install, so
# change it before installing anything you expose. A Yundera PCS never uses it —
# the orchestrator writes this file with a ~24-character random DEFAULT_PWD.
APP_EMAIL=
APP_DEFAULT_PASSWORD=changeme-please
DefaultUserName=admin
DefaultPassword=changeme-please
