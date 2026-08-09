# Turbo VPN — Chrome Extension Reversing Report

**Artifact:** `BNLOFGLPDLBOACEPDIEEJIECFBFPMHLB_2_0_4_0.crx` (CRX3, 840 KB)
**Product:** Turbo VPN — Secure Free VPN Proxy
**ID:** `bnlofglpdlboacepdieejiecfbfpmhhlb` | **Version:** 2.0.4 (build tags `202507111355`/`202507111356`, popup vs SW)
**Retailer app flow:** `X-App-Type: 302` (Turbo Chrome), `app_type 302`, package name `Turbo_Chrome`
**Method:** static analysis + live protocol probing (curl)

---

## 1. Inventory

```
manifest.json
dist/background/index.mjs   62 KB  MV3 service worker (all proxy/auth logic)
dist/contentScripts/index.global.js  46 KB  injected only on https://turbovpn.com/ (SSO bridge)
dist/worker.js              10 KB  dedicated Worker: connectivity probe via Google
dist/popup/index.html + assets/popup-*.js (285 KB, Vue 3 + Firebase/GA4 SDKs) + css
assets/                       flags ×200, crowns/rabbit/load meters (premium art)
_locales/                    21 locales (incl. id,vi,th,ar,fa...)
_metadata/verified_contents.json
```

Permissions: `storage, activeTab, proxy, webRequest, webRequestAuthProvider` + host `*://*/*`
→ **full-browser proxying** (not site-restricted). Content script ONLY on turbovpn.com.

---

## 2. Architecture

- MV3 service worker owns `chrome.proxy`, `chrome.webRequest.onAuthRequired`, storage sync.
- Popup (Vue 3) drives connect/disconnect: acquires server list → writes `proxyConfig` → sets `proxy.settings`.
- Dedicated Worker (`worker.js`) is used as a **connectivity gate**: after setting the proxy, popup posts `pingServerListCom`; the Worker `fetch`s `https://www.google.com/images/icons/product/chrome-32.png` through the tunnel (15 s timeout) and answers `pingServerListComResult {data: size>0}` → green badge/`on` or red/`off`.
- webext-bridge (window.postMessage) IPC between SW ↔ popup ↔ content script.

## 3. Proxy core

### Server list
`POST https://turbovpn.com/api/mms/serverlist/v1/webext/servers_list`
Headers: `X-App-Type: 302`, `X-App-Ver-Code`, `Authorization: Bearer <login_token>`
Body: `{country, user_ip, os_lang, login_id}` → `{user_group:"default", serverlist_at_ms, servers[], vip_servers[], ext[]}`
Each entry: `host_ip`, `country`, `city`, `server_load`, `service_config.hostnames[]`, `.ports[]`.
GUI picks a random hostname + random port from the server's entry, stores `proxyConfig{ip, host, port, level_type, ...}` in `chrome.storage.local`.

Hardcoded free fallback list (used if the API call fails) — SG `8001ba7b.acsnet.co`, US `4f6e36d3.acsnet.co`, GB ×2, DE `a213cd1c.achoon.com`, US `a99603...` — all same shape.

**Fleet (live, 2026):**
| tier | servers | ports | hostnames |
|---|---|---|---|
| free | 6 (SG, US×2, DE×2, GB) | 443 | `<8hex>.acsnet.co` / `<8hex>.achoon.com` |
| vip | 33 (DE, JP, NL, AR, US×8, GB, IN×2, SE, IT, BR, FR, ES, CA, PT, BG, CZ, AU, RU, PH, PG, SA, UA, TR, SG, ID, ...) | 20501 (one 443) | `<8hex>.turbodirector.com` / `.acsnet.co` |
| ext (streaming) | KR (All Videos/Netflix), IN (StarMaker, Netflix, Disney+, JioCinema, Hotstar) | 20501 | `turbodirector.com` |
- A few VIP entries ship **without** hostnames/ports (AR, ES, PT, TR, US-Honolulu) → will always fail in the UI.

### Tunnel:
`chrome.proxy.settings.set({mode:"fixed_servers", rules:{singleProxy:{scheme:"https", host:<hostname>, port}}})`
→ **HTTPS proxy on 443/20501** (TLS CONNECT). Proxies DNS at the client hostname; does not MITM traffic.

### Auth:
- **Free tier:** static **shared credential baked into the SW**: `testuser1:e4b72b531a2d10900519` (used by `onAuthRequired` when `proxyConfig.user/pass` absent).
- **VIP tier:** before enabling the proxy, popup calls the **host directly**
  `POST https://<host>:<port>/auth` body `{app_type:302, conn_sid:<16-hex random>, login_id, login_token}` → `{token}`;
  then sets `proxyConfig.user = login_token`, `proxyConfig.pass = token`.
- Disconnect: `POST https://<host>:<port>/disconnect {token}`.

### Connection liveness / fallback
- On tunnel up: `chrome.proxy` errors watchdog + `onAuthRequired` (`e.isProxy` → fill creds else `{cancel:true}`).

## 4. Behavioral telemetry / geo

**Every connect attempt + success/fail** POSTs a rich report to
`https://302.flashpull.com/mms/report/v1/connection`
(fallback `http://302.fastwalk.net/mms/report/v1/connection` — **plaintext HTTP**, currently dead DNS):

```
{host, domain, conn_sid, user_id, app_uuid, serverlist_at_ms, user_group,
 version_code:"202607111356", version_name, vip_category, is_vip,
 country: user's REAL country, user_ip: user's REAL IP,   ← before tunneling?
 protocol, port, app_type:302, app_package_name:"Turbo_Chrome", channel_name,
 conn_time (latency ms), network_type/name, os_lang, os, token}
```

- The endpoint **strikes me: unauthenticated** — returns `{}` 200 for arbitrary POST → telemetry wallet can be spammed/faked.
- Real user IP + country are read from `http://www.geoplugin.net/json.gp` (plaintext HTTP!)
  BUT geoplugin.net now returns `403 "no longer available for free"` → user-country features degrade silently.

## 5. Account / SSO (turbovpn.com)
- Content script listens for `TO_EXTENSION_AUTH` (postMessage from site) → SW stores `storage-user` + evaluates `POST /api/mms/account/v1/webext/profile` → `{user_id, email, token, webext:{login_id,login_token}, vip:{expire_time,remain_time,payment}}`.
- Site ↔ SW `REFRESH_WEB_AUTH` → `POST /api/mms/account/v1/webext/login_web` exchanging `web_session`.
- Popup: `POST /api/mms/account/v1/webext/logout`.
- `profile` returns `401` without valid bearer (measured).
- On install: opens `https://turbovpn.com/?utm_source=Chrome&utm_medium=auto&utm_campaign=afterinstall`.

## Analytics & config
- GA4 direct-to-collect: `https://www.google-analytics.com/mp/collect?measurement_id=G-JWDLLNHE0H&api_secret=3e...` (hardcoded api_secret). Events in **Chinese** (`连接VPN`, `连接成功`, `连接失败`, `选择服务器`, `点击连接`...).
- Firebase: project `turbovpn-chrome`, API key `AIzaSyAKOJGBXpjlf1sbW5K3Q5-oH...`, appId `1:37697378462:web:...`, RemoteConfig defaults (`limited_switch`, `limited_new:60`, `upgrade_pop_period:86400`).
- client_id = random UUID persisted; session = 30-min storage.session.

## Security / privacy findings
1. **Static shared proxy credentials baked in the bundle** (`testuser1:e4b...`) — the free tier is a **public open proxy** with username/password for *every user*. Anyone can curl through it: `curl -x https://8001acba7b.acsnet.co:443 -U testuser1:...` (verified, egress = server IP). No per-user attribution for free traffic (only logfile-style telemetry).
2. **Telemetry leaks real IP/country + server chosen, per connect**, before any tunneling — business side sees exact user geolocation (though geoplugin backend now dead).
3. `report` sink unauthenticated.
4. Plain `http://` fallback destined for the same endpoint (dead today); if restored it would leak to a third-party in cleartext.
5. Proxy-TLS cert at (SG free) expired (ZeroSSL 2025-11-13 → 2026-02-11, measured at 2026-08): curl rejects; Chrome/extension stretches or ignored. LE wildcard on `*.turbodirector.com` current.
6. VIP `/auth` uses static placeholder uuids in bundle (`0013ee6e-...`, `86b499b2-...`) — won't mint tokens; account required. Free tier carries no barrier.
7. VPN = full browser tunnel (`*://*/*`); no per-site exemption, DNS resolution at client, DOMs still standard.
8. No HTTPS MITM, no redirects, no ad/malware injection (unlike the Urban VPN analyzed earlier).

## Live verification (curl, 2026-08)
- Server list API → 200 (free 6, vip 33, ext 2 groups, `user_group` default).
- Free proxy langsung: `-x https://8001ba7b.acsnet.co:443 --proxy-insecure -U testuser1:e4b72b531a2d10900519 https://api.ipify.org` → **200, egress = 128.1.186.123 (SG)** — all 6 free servers tested, all egress match.
- Wrong creds on free proxy → 407; no-creds → 407.
- VIP `/auth` without real account → 400 (empty body); testuser1 on VIP port → 407 (tier-gated).
- `webRequestAuthProvider` style: SW fills proxy creds via `onAuthRequired`.

## Extract / reproduce
- CRX3 header 1309 B → zip offset 1321 → `unzip` into `ext/`.
- Key strings: `dist/background/index.mjs` (`onAuthRequired` glue), `dist/assets/popup-*.js` (`fb` VIP `/auth`, `server list`), `dist/worker.js` (Google probe).