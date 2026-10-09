# Feedback

**Settings menu → Send feedback.** A short form whose message goes to whoever operates
the box — not to the Maison project, and not to anyone Maison names.

Maison does not know who operates a box. A stock install has nobody; a hosted one has a
provider. So the deployment names a **sink**, and the feature exists exactly when the
sink does:

| Env | Meaning | Default |
|-----|---------|---------|
| `FEEDBACK_URL` | The sink's endpoint. Maison calls it from its backend, so it only has to be reachable from Maison's container (e.g. `http://admin-app/api/feedback/ingest` on the box's own network). | _(unset — feature off)_ |
| `FEEDBACK_TOKEN` | Bearer token sent on every call to the sink. **Required**: the sink is reachable from every container on that network, and without a secret any installed app could write to the operator as the user. | _(unset — feature off)_ |

One without the other is a deployment mistake: Maison logs it at boot and the feature
stays off.

---

## What the user sees

The menu entry appears only when **all** of these hold:

1. both variables are set, and
2. the sink answers its descriptor (below) with `200` and an `operator` name.

A configured sink that is down, refuses the token, or answers `404` hides the entry. A
form that cannot be sent is the one outcome this is designed not to have. The answer is
cached — five minutes on success, one minute on failure — so the sink does not hear about
every page load, and a sink that was briefly down reappears within a minute.

The form names the operator ("Tell *Acme* …"), shows the sink's privacy note if it gave
one, links to its support page if it gave one, and **lists what is attached** — the
Maison version and the page the user was on. Nothing else is collected.

---

## The sink contract

Both calls carry `Authorization: Bearer $FEEDBACK_TOKEN` and are made **by Maison's
backend, never by the browser**: the browser would need CORS on the sink and a session at
whatever gate stands in front of it, and the token would have to reach the page — which
is exactly what must not happen, since Maison's own API has no authentication behind its
gate. Timeout is 5 s per call.

### `GET $FEEDBACK_URL` — the descriptor

```json
200
{
  "operator": "Acme",
  "privacyNote": "Read by the Acme team; kept 90 days.",
  "maxLength": 5000,
  "supportUrl": "https://help.acme.example"
}
```

`operator` is required; the rest are optional (`maxLength` defaults to 5000). Any other
status, a timeout, a body that is not JSON, or a missing `operator` means **off**.
`404` is the polite way for a sink to switch the feature off for one box.

### `POST $FEEDBACK_URL` — a submission

```json
{
  "message": "Love the new store. A dark mode would be nice.",
  "category": "idea",
  "context": { "source": "maison", "version": "1.1.47", "page": "/store", "appId": "jellyfin" },
  "reporter": null
}
```

- `message` — trimmed, non-empty, at most the descriptor's `maxLength` characters.
  Maison checks both before sending.
- `category` — one of `idea`, `bug`, `other`. Closed, so a sink can route on it.
- `context` — set by Maison, not by the page: `source` is always `maison`, `version` is
  the release tag (`dev` on a hand build). `page` and `appId` are optional.
- `reporter` — always `null` today. Maison sits behind a gate it does not read identity
  from, so it does not know who is typing; the sink knows which box called it. The field
  is in the contract now so a sink does not change shape when Maison learns.

Answer any `2xx` (`202` is conventional) for success. On failure answer `4xx`/`5xx` with
`{"error": "…"}` — that line is shown to the user as-is, so write it for them.

Nothing is queued or retried. A submission that fails is reported on the form and the
user can send it again.

---

## Maison's own API

What the dashboard calls. Neither response ever contains the sink URL or token.

| Route | Answer |
|-------|--------|
| `GET /api/feedback` | Always `200`. `{"enabled": false}`, or `{"enabled": true, "operator", "privacyNote", "maxLength", "supportUrl", "categories", "version"}`. |
| `POST /api/feedback` | Body `{"message", "category", "page", "appId"}`. `202` sent · `400` refused before sending · `404` not configured · `502` the sink failed (its error line in `error`). |

Rate limiting is not Maison's job here; a sink that needs it applies it.
