# Step-up option B and remember-me: study

2026-10-20

## Answers

**Is option B a hack?** No, but it is a trade: its security rests on a configuration rule rather than on something the protocol proves. B uses only documented, standard behaviour (OIDC `prompt=login` at Dex, ordinary session lifetimes at the login servers). What it gives up is verifiability: nothing in the tokens proves that the login server really asked for the password, so the guarantee holds only while the rule "login servers keep no session longer than 5 minutes" holds.

**Is remember-me broken?** Yes, and independently of step-up. Authelia's "Remember me" box only controls Authelia's own cookie. Neither Dex nor the AppShield gates ever see the choice, and every gate keeps a persistent 30-day cookie. A user on a public laptop who unticks the box is still logged into every app they opened, for 30 days after closing the browser.

## Context

Step-up needs one thing the stack does not give today: proof that the user typed their password moments ago. Full design: [Step-up authentication for Maison](https://claude.ai/code/artifact/ac43861c-fe4d-4a75-b4a2-763ae0a07440).

- **The chain:** AppShield gate → Dex (broker, holds the 30-day single sign-on session) → a login server: Authelia (Local Account) or the Yundera Login IdP.
- **What works:** Dex honours `prompt=login` on its own session and goes straight to the user's login server. Authelia honours `prompt=login` and `max_age` when it receives them (tested live).
- **The gap:** Dex does not pass `prompt=login` / `max_age` on to the login server, and stamps `auth_time` with the moment the login server answers. If the login server still has a session, it answers silently and Dex reports a fresh login that never happened (tested live). This is still true in the release now pinned, Dex v2.46.0.
- **Constraint:** no custom Dex build without the architect's approval. A small Dex patch closing the gap exists and was validated on wisera, but is parked.

## Option B assessed

B passes four of the six tests below; it fails on verifiability and on failing loudly, which is the price of not changing Dex.

**What B is.** Dex becomes the only component that keeps a login session. Authelia's session is cut to a few minutes, and the Yundera Login IdP stops reusing sessions for PCS logins (no silent Firebase relay, no 30-day IdP session). Every time Dex hands a user to a login server, that server has nothing to reuse and asks for the password. Dex's `prompt=login` then means a real password entry, and the `auth_time` Dex reports is true.

| Test of a hack | Option B | Verdict |
| --- | --- | --- |
| Relies on undocumented or accidental behaviour? | No. `prompt=login` is OIDC Core; session lifetimes are documented settings of Authelia and of our own IdP | Pass |
| Works around a component instead of with it? | No. Dex is already the session holder; the login servers are reduced to checking credentials | Pass |
| Can the deciding component verify the guarantee? | No. AppShield trusts `auth_time`, which is true only while the login servers keep no session | Fail |
| If the rule breaks, does it fail loudly? | No. Raising Authelia's session again makes step-up pass silently, with no error anywhere | Fail |
| Simple to explain and own? | Yes. One rule: login servers keep no session longer than a few minutes | Pass |
| Reversible and forward compatible? | Yes. If Dex ever forwards `prompt=login`, B keeps working unchanged and becomes verifiable | Pass |

**How much a broken rule costs.** If the rule is ever broken, step-up degrades to what exists today, no re-authentication; it never becomes worse than today.

**The exact guarantee.** A silent reuse can only happen inside Authelia's session lifetime, and Dex then stamps the reuse time, not the password time. So Maison's 5-minute window really means "password typed within Authelia's lifetime + 5 minutes". With an Authelia lifetime of 1 minute that is at most 6 minutes; with 5 minutes it is at most 10. The shorter Authelia's lifetime, the closer the guarantee is to 5 minutes.

## Options compared

A is the only option where the login time is both verified and the user's password; B is the leanest; C verifies without touching Dex but costs the most parts; D changes the factor.

| | A: patch Dex | B: session-less login servers | C: AppShield goes direct | D: Dex MFA |
| --- | --- | --- | --- | --- |
| What the user does | Password on own login page | Username + password on own login page | Password on own login page | TOTP code or passkey |
| Custom Dex build | Yes, until upstream merges (no release date) | No | No | No |
| Login time verified in tokens | Yes | No, held by a config rule | Yes | No `amr` claim; inferred from a dedicated client |
| Moving parts added | Dex patch | Two settings (Authelia, Yundera IdP) | AppShield clients at Authelia and the Yundera IdP; identity matched via Dex `federated_claims` | Static Dex client with an MFA chain; enrolment at a trusted moment |
| Yundera IdP change | Stop silent reuse on forced login | Stop silent reuse for all PCS logins | Stop silent reuse on forced login | None |
| Side effects | None | No more silent hop from the Yundera dashboard into a PCS; Authelia's "Remember me" removed | AppShield must know which login server sits behind Dex | TOTP enrols on first use: a stolen cookie could enrol an attacker's code |
| Fails if misconfigured | Loudly (AppShield refuses an old `auth_time`) | Silently (step-up passes) | Loudly | Silently if MFA is dropped from the client |

A, B and D share the same Maison and AppShield work, so moving between them later needs no rework above Dex; C adds a second login path inside AppShield.

## Remember-me today

The "Remember me" choice reaches only one of the three session holders, so the user's choice does not protect a shared computer. This predates step-up and is independent of the option chosen.

| Session holder | Lifetime today | Sees the user's choice? |
| --- | --- | --- |
| Authelia | 30-day persistent cookie if ticked; until the browser closes if not | Yes, for its own cookie only |
| Dex | Until the browser closes: for connector logins Dex uses its server default (`rememberMeCheckedByDefault`, unset, so false) | No. Dex shows the box only for password connectors |
| AppShield gates (Maison, terminal, consoles…) | Always a persistent 30-day cookie (`SESSION_DURATION_HOURS: 720`) | No |

**The public-laptop case.** A user unticks the box, uses Maison and two apps, closes the browser and leaves. The next person opens the same browser: Maison and both apps are still logged in, for up to 30 days. Only explicit logout prevents it; since Dex v2.46.0 that works across every gate through RP-initiated and back-channel logout.

**Effect of option B.** Authelia's box disappears, which removes a choice that never protected anything. After a browser restart, already-opened apps stay logged in as today, and opening a new app asks for the password (today it was silent if the box had been ticked). Step-up adds a safety net: nobody can run a critical Maison operation on that laptop without the password.

**What a proper fix needs.** The choice must be made where the session starts and reach every holder:

1. A "Keep me signed in" box on the Dex login page for connector logins, which Dex does not offer today (a Dex change).
2. A way for the gates to learn the choice, so they issue cookies that end with the browser. OIDC has no standard claim for this, so it needs a deliberate design (for example a claim Dex adds, or a choice AppShield asks for itself).

That is a feature of its own, not part of step-up.

## Decisions to take

One choice gates the work: whether a guarantee held by a configuration rule is acceptable (B), or the login time must be verified (A or C).

- [ ] **Option:** B (lean, rule-based) or A (verified, custom Dex until upstream merges) or C (verified, more parts in AppShield).
- [ ] **If B: Authelia session lifetime.** 1 minute keeps the real guarantee at 6 minutes or less; 5 minutes allows up to 10.
- [ ] **If B: owning the rule.** Enforcement is out of scope here; it needs an owner so a later change to Authelia's session cannot silently disable step-up.
- [ ] **Remember-me:** open a separate card for "Keep me signed in" across Dex and the gates; until then, document logout and private windows for shared computers.
- [ ] **Dex upstream:** file the patch as a pull request regardless of the option. If it merges, B moves to verified login times at no cost.

Already decided: two clicks at Authelia are acceptable; one 5-minute window for all critical operations; the template moves to Dex v2.46.0.

## Evidence

Code read at the versions deployed; live tests on wisera (`wisera.inojob.com`) on 2026-10-20 with a Local Account.

| Finding | Source |
| --- | --- |
| Dex forwards only its static `promptType` upstream | `connector/oidc/oidc.go` line 471 in Dex v2.46.0 |
| Dex stamps `auth_time` when the login server answers | `server/authflow/finalize.go` (`a.AuthTime = h.Now()`) |
| Dex uses its default, not the user's choice, for connector logins | `server/authflow/callback.go` lines 106–109 |
| Dex cookie lasts only until the browser closes unless remembered | `server/session/session.go` `SetCookie` |
| Gates keep a 30-day persistent cookie | AppShield `internal/authn/authn.go` (`MaxAge`), template `SESSION_DURATION_HOURS: "720"` |
| Authelia session 30 days today | template `auth/configuration.yml.tmpl` lines 57–59 |
| Yundera IdP silently relays the Firebase session | `pcs-orchestrator/src/service/oidc/interactions.ts` (`renderLoginPage`, `auto`) |
| Live: `prompt=login` on stock Dex returned a code with no password | wisera, Dex trunk `ab64ed7` |
| Live: Authelia asks for the password on `prompt=login` and on `max_age=0` | wisera, Authelia 4.39.25 |
| Live: patched Dex made Authelia ask; `last_login` took Authelia's time | wisera, `dex:stepup-ab64ed7` (since removed) |
| Live: login and logout discovery on Dex v2.46.0 | wisera, now running v2.46.0 |
