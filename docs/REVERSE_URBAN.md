# Urban VPN Browser Extension 5.14.0 — Reverse Engineering Notes

Package: `EPPIOCEMHMNLBHJPLCGKOFCIIEGOMCON_5_14_0_0.crx` (CRX3, 9.7 MB)
Extension ID: `eppiocemhmnlbhjplcgkofciiegomcon` (from filename, matches store/bugsnag strings)
Product: **Urban VPN Proxy** (manifest name `__MSG_appName__` = "Urban VPN Proxy", `urban-vpn.com`)
Build date: 2024-07-23. Source is bundled via webpack, mostly *unminified* (readable class names), only `popup/build.js` (4.5 MB, Vue 3) is a minified bundle.

---

## 1. Bundle inventory

| Path | Size | Role |
|---|---|---|
| `manifest.json` | 2.2 KB | MV3, `update_url` = Google, minimum Chrome 110 |
| `service-worker/index.js` | 2.3 MB | Background: "Mario/Brother" DI container + all core logic |
| `service-worker/anti-malware.js` | 148 KB | Safe browsing / AI URL check (imported via `importScripts`) |
| `ad-blocker/background.js` | 186 KB | "pos" ad-analytics + ad blocking (imported via `importScripts`) |
| `ad-blocker/content.js` | 252 KB | Page-side ad candidate detector/hider (content script, all frames) |
| `content/content.js` | 152 KB | "Luigi": in-page notifications, iframe injector (content script) |
| `libs/processor.js` | 228 KB | "Luigi" e-commerce watcher DI container (content script) |
| `libs/requests.js` | 5 KB | Page-context fetch/XHR interceptor (web-accessible) |
| `libs/extend-native-history-api.js` | 2 KB | Page-context history.pushState/replaceState hook + Shopify detector |
| `content/location/location.js` | 5 KB | Geolocation spoofing ("toad") |
| `executors/*.js` | 2–25 KB each | Ad-iframe instrumentation snippets (web-accessible) |
| `offscreen/index.html` + `worker.js` | 2 KB | Proxy connection verification worker |
| `popup/*` | 4.5 MB | Vue 3 popup UI |
| `_metadata/verified_contents.json` | — | Chromium signed manifest (ignored) |

Content scripts injected at `document_start`:
1. `ad-blocker/content.js` — all frames, `match_about_blank: true`
2. `content/content.js` — top frame only

---

## 2. High-level architecture

The codebase is a three-process messaging system with internal codenames:

```
┌──────────────────────────┐   chrome.runtime (port/message)   ┌───────────────────────┐
│ service-worker/index.js   │◄─────────────────────────────────►│ content/content.js     │
│ "Mario"  (BG business)    │  LinkModule "boxes" (sessionId,    │ "Luigi" (page UI)      │
│ "Brother" (e-commerce)    │   boxId, appName, sendBox...)      │  → iframes/notifs      │
└──────┬─────────┬──────────┘                                    └───────────┬───────────┘
       │ webRequest / proxy      window.postMessage (page, web-accessible)   │
┌──────▼─────────────────────┐                                    ┌───────────▼───────────┐
│ ad-blocker/background.js   │                                    │ libs/requests.js      │
│ anti-malware.js            │                                    │ libs/extend-history..   │
└────────────────────────────┘                                    │ executors/N.js  (deps) │
                                                                  └───────────────────────┘
```

- **Mario** = service worker side; **Luigi** = content scripts ("makeLuigi({modules:[LuigiLinkModule, ...]})").
- **ToadLinkModule** = one-shot scripts injected into the page (`/content/location/location.js`, allowed message `HideLocation.GetLocation`).
- **Bugsnag** is loaded first thing in the worker (sessions/notify .bugsnag.com).

---

## 3. The VPN itself (proxy service)

### 3.1 How the proxy is set
`ProxyChromeStrategy.connect(server, bypassList)`:
```
chrome.proxy.settings.set({
  value: { mode: "fixed_servers",
           rules: { singleProxy: server, bypassList } },
  scope: "regular" })
```
`server` = `{ scheme: "http", host, port }` — a plain **HTTP proxy with proxy auth**, no TLS MITM anywhere.

### 3.2 Entry points (static proxy infrastructure, GeoSurf)
`StaticProxiesService` against `https://geo.geosurf.io/`:
- `GET /entrypoints/autoserver` — one random server ("Auto Server")
- `GET /entrypoints/countries` — whole country map (premium access type `accessType` flag)
- `GET /entrypoints/streamingservers` — Netflix-specific servers
- headers: `X-Client-App: URBAN_VPN_BROWSER_EXTENSION`, `Accept: application/json`

Each entry: `server.address.primary {host,port,ip,weight}` + `signature` + optional `secondary` addresses. Response mapped to `ProxyLocation` objects; each proxy server is keyed by `scheme://host:port` (`buildProxySignatureKey`) → signature.

`HttpClient` attaches `Authorization: Bearer <auth-token>`/security tokens to every request, plus `X-Client-App` etc.

### 3.3 Identity / auth bootstrap (anonymous accs)
`AuthClient` (AuthModule):
1. `POST api-pro.urban-vpn.com/rest/v1/registrations/clientApps/URBAN_VPN_BROWSER_EXTENSION/users/anonymous` → anonymous user `{authToken, registrationToken?}` (retries 3×, 1 s)
2. `POST security/tokens/accs` with `Authorization: Bearer <authToken>` and body `{type:"accs", clientApp:{...}}` → `securityToken {value, expirationTime}`

Stored: `AUTH_ANONYMOUS_AUTH_TOKEN`, `AUTH_ANONYMOUS_SECURITY_TOKEN` in `chrome.storage.local`.
`AUTH_ANONYMOUS_SECURITY_TOKEN` is used as Bearer for all subsequent API calls via `RequestsInterceptor`.

### 3.4 Location processor ("MarioLocationProcessor") — connection FSMs
Phases (in `Phase.Sequence`):
```
CONNECTION:  VerifyInternetConnection → ProxySelection → Credentials → Connect → VerifyProxyConnection
           (with autoRepair/reconnect on failure, proxyKeeper "next()" on bad proxy,
            fallback direct, retry loop)
REFRESH:     refreshToken / network change
```

Providers registered:
- `INTERNET_CONNECTION_PROVIDER` (fetch `https://www.cloudflare.com/, https://www.google-analytics.com, https://www.youtube.com/favicon.ico` with 3-30 s timeouts)
- `PROXY_CONNECTION_PROVIDER` (offscreen verification)
- `CREDENTIAL_PROVIDER` → `CredentialProvider`
- `PROXY_PROVIDER` → `ProxyProvider` (picks guaranteed proxy location, saves signatures)

**Observing offscreen**:
- `offscreen/` document spawns a **WebWorker (`worker.js`)** which `fetch(proxyUrl)` with `AbortController` timeout to test the actual tunnel (workaround for `webRequestAuthProvider` not triggering on service-worker-initiated requests).

**Credentials** — `CredentialProvider.provide()`:
1. get signature for the proxy (`signatureStorage[key]`)
2. `POST api-pro.urban-vpn.com/rest/v1/security/tokens/accs-proxy` body `{type:"accs-proxy", clientApp:{name:"URBAN_VPN_BROWSER_EXTENSION"}, signature}`
3. token `{value, expirationTime}` (+ clock-drift shift via HTTP `Date` header)
4. connect with `credentials = {username: value, password: "1"}`

So proxy auth = **HTTP Basic username=one-time accs-proxy token, password literally `"1"`**.

`onAuthRequired` handler (ProxyChromeStrategy):
- `async onProxyOnChallenger(...)` → `chrome.webRequest.onAuthRequired` asynchronous blocking; only supplies auth if `isProxy` and the challenged host:port equals the current singleProxy; returns `{}` (cancel) otherwise.
- `CredentialsManager` caches the first response, and after 60 s / 20 callbacks (whatever comes first) triggers extraction of `LOCATION_PROCESSOR_CREDENTIALS_CALLBACK_COUNTER` then re-requests a fresh token.

### 3.5 Session "state" amd sync-up
- Alarm `LOCATION_PROCESSOR_SYNC_UP` every 60 s keeps service worker alive + state sync to `chrome.storage.local` (`LOCATION_PROCESSOR_STATE`, `LOCATION_PROCESSOR_SESSION_ID`, ...)
- `<...>` `chrome.storage.session` for credentials/proxy/token.
- On authProvider fail → `DisconnectReason` recorded, forwarded to `ConnectionStateChangedListener`s (badge icon, notification "VPN disconnected", "connect again").

### 3.6 Bypass list
- `BypassManager` (MarioBypass) — predefined (immutable) domains + user-added domains; `bypassProcessBinder` updates um catches.
- default bypasses: `dns.google, dns.alidns.com, cloudflare-dns.com, authentication.urban-vpn.com, config-toolbar.urban-vpn.com, api-pro.urban-vpn.com, stats.urban-vpn.com, analytics.urban-vpn.com, www.google-analytics.com, sessions.bugsnag.com, notify.bugsnag.com`.
- `PingLocations` measures latency (`WeightStrategy` / `IpFirstWeightStrategy`) per country → used in location list ordering.
- Excluded extensions list: `["ngpampappnmepgilojfohadhhmbhlaek"]` (InterruptExtensionsModule).

### 3.7 Hidden location (hide-my-location / session geo)
- Web-accessible `libs/extend-native-history-api.js` + `/content/location/location.js` (Toad) hook **`navigator.geolocation.getCurrentPosition/watchPosition/clearWatch`** in page.
- Intercepts the message to Mario (`HideLocation.GetLocation`), Mario's `makeHideLocationDetailsProvider` asks:
  - user visiting a site on that domain — if the user is not bypassed, use **GeoIP of the proxy IP** (`geo.geosurf.io/<proxyip>` → contains `loc`), else the real IP.
  - returns `{latitude, longitude, accuracy: 20, timestamp}`.

---

## 4. E-commerce watcher ("Luigi" / safe-net / rights)

Two web-accessible scripts injected on all pages:
1. `libs/requests.js` — wraps `window.fetch` and `XMLHttpRequest` in page context:
   - `RequestValidator` matches `{regex, methods}` against url; if matched → `postMessage` to extension (type `_$SAVE_HTTP_DATA`) `{url, handler, watcher}`.
   - Valid only JSON and text responses.
2. `libs/extend-native-history-api.js` — pushes url changes + Shopify detection (`web_profile_info` signal).

The content script `content/content.js` (`Luigi`) plus `libs/processor.js` implement **Safe Price Check**:
- When enabled (E_COMMERCE_FEATURE_KEY=AGREE, default REJECT), it
  - intercepts product-page DOM / requests
  - queries `_StreamManager` → `_SafePriceCheck`, `_Track`, `_SyncUp`, `_Stacktrace` with credentials
  - displays "Safe price check" notification listing lower prices from partner sites
- The sync endpoints: `anti-phishing-protection.urban-vpn.com`:
  - `GET /ecommerce/template/config` + `?template? + basic auth` **`Basic <base64 "George_Michae!:I'll_never_goNNa_dance_again">`** (hardcoded!!)
  - `GET /ecommerce/template/config/price-parser`, `/ecommerce/template/config/metadata`
- content tokens: nothing on the page; all traffic via service worker.

The Luigi `_BgInjection`-powered "Brother" has `_authManager`, `_configManager`, `_trackManager`, `_couponManager` (coupons fetched per domain), `_currencyManager`, `_errorTraceClient` — business logic that appears when BOTH the user consented ("EU/PA") and the checkbox in popup.

---

## 5. Ad-blocker + ad analytics (the "pos" system)

### 5.1 background (`ad-blocker/background.js`)
Modules:
- `ConfigManager` sets `configuration?pv=2.1.2` from `https://analytics-toolbar.urban-vpn.com` (also `online` / sw config fetch)
- `TicketBuilder` + `TicketSender`: builds **"tickets"** describing ads (chains of frames that were involved in one ad event, = frameId chains from `content` side) and sends them:
  - `POST https://analytics-toolbar.urban-vpn.com/tickets/{ticketId}` (single)
  - `POST .../tickets` (batch)
  - auth: `Authorization: Basic pnldsk:1-5-5-<panalyticsId>` ← panalyticsId = panel identity user id
- `SensitiveDataFilter`: redacts **username/password tokens in URLs** (`user:pass@`), query parameters, path segments, and page titles using remote rules fetched from `/api/privacy/data/rules/exclusions` (self-checks privacy).
- `DataCollectionManager`: toggle `posdDataCollection` in `chrome.storage.local` (default off). Also an `enable/disable` API.
- `AdBlockInspector`: reports hidden-ad elements (hiding confirmations), "ad candidates" from FB/Twitter/etc... detection state shared to backend.
- `AdBlocker`: statuses per network (display/fb/twitter/instagram/...), removal/hiding; counters.

### 5.2 content (`ad-blocker/content.js`)
- Detectors for ads: `FacebookLoader`, `TwitterLoader`, `InstagramLoader`, `PinterestLoader`, `LinkedinLoader`, `TwitchLoader`, `BannerAds`, `HTML5Ads`, `VideoAds`, `SkinAds`.
- Facebook-specific: main feed (`fbFeed`), right column (`fbRightColumn`), Marketplace (`fbMarketplace`) — `MutationObserver` on each, scores "sponsored" candidates (heuristics: `data-ad-previewable` attributes, sizes, headings).
- **Injection of executors** into ad/har frames: from a `Detector` it creates `<script type="text/javascript" src="chrome-extension://<id>/executors/N.js" bis_use...>`, with `data-dynamic-id`, `data-config` (server config for that executor), `nonce=""` — i.e. runs in **page context of the ad iframes**.
- Video/Ad-Har trick: On found video/`video element traffic` (e.g. facebook), it adds **`videoHar`** tracking + click hijacking on child `iframe` to detect video click successors.

### 5.3 executors (page-deployed scripts)
Dedicated spaghetti that runs inside ad frames:
- `100.js` — `GET_WINDOW_TARGET_URL`: window prop scan (clickTag, adData, BF.Parameters.targeturl, ADAPT.symbols.stage.clickUrl, admixAPI.ownerBanner.clickUrl; depth-limited BFS over keys whose normalized name matches keyword list). Also monkey-patches `window.open` to capture target URLs from simulated clicks (only elements > 40×40 px).
- `304.js` / `305.js` / `306.js` — `304_VIDEO_DATA`: sample JSON embedded in HTML5 `<script>`/XHR (config.NAME/PATH / PARSERS), extracts `videoId`, `duration`, etc. → `window.postMessage({posdMessageId:"304_VIDEO_DATA"...})`.
- `307/308` — spot traffic listeners (XHR `TRAFFIC_LISTENER_CONFIG`).
- `501.js` — XHR/fetch sniffing helpers.
Data flows back to `ad-blocker/background.js` via `window.postMessage`-style channel to build "tickets" → analytics backend.

So the ad-blocker IS ALSO an ad-attribution tracker (ad candidate + chain + click target URLs) skewing towards "ad analytics" backend plus ad blocker UX.

---

## 6. Anti-malware / safe browsing (AI protection)

- `service-worker/anti-malware.js` + `MarioAntiMaleModule` read policy key 🔒 `ANTI_MINING_POLICY` (NoAnswer→agreed)
- Endpoints: `https://ai-user-protection.urban-vpn.com/api/rest/v2/secure/urls/checkSafety` (and `/basic`), `/api/privacy/data/rules/exclusions`
- `checkUserPrivacy` in ad-blocker (alasnatch)
- On page visited → `SafeBrowsingService` stream informs an extension page that fetches `checkSafety` for URL, if treated `unsafe` → popup warning notification.
- default muted domains (`mutedDomainsKey`).

---

## 7. Monetization / growth UX vestigial modules
- `MarioUpgradeCampaignModule`: premium upsell after 3 successful connections (`minSuccessConnectionsThreshold: 3`)
- `MarioQrCodeModule`: QR promo after 7 days
- `MarioConsentReminderModule`: re-prompt consent after 2 h (`showAfterMinutes`)
- `uru/HelloGoodbye`: install/uninstall redirect URLs with analytics parameters (`analyticsRedirection`, `ED_CAMPAIGN`)
- referral codes stored in local storage (`campaign`)
- `Popup`: Vue 3 app — connect/disconnect, location list, proxy IP, serving IP, bypass list editor, adblocker/privacy toggles, supports all platforms + premium flows; fetches `https://api-pro.urban-vpn.com/rest/v1/storeLinks` for download links.

---

## 8. Analytics & tracking

Everything aggregates through **cluster of p-analytics (panalytics)**:
- Installed `analytic` services: `MarioInternalAnalyticsModule` → `analytics.urban-vpn.com/rest/v1` (`appName: URBAN_VPN_EXTENSION`), `modifyAnalyticsService` (Bugsnag ErrorMonitoring at install/uninstall, heartbeat)
- `MarioGa4Module` → GA4 `google-analytics.com/mp/collect` (measurementId `G-9RH9D2RSVZ`, API secret `DAgrEhTmSI...`!)
- `MarioDataSharingModule` distributes user settings (panelistId, distributorId 5, partnerId 5)
- Identity: `panel.identics` (from `authentication.urban-vpn.com` — identity token) with `UserId`
- `MarioPing`, `MarioPingAnalytics`, `MarioHeartbeatModule` (20-min keepalive, heartbeat payload includes adblocker+antimining policy status!)

---

## 9. Privacy / security observations

1. **Proxy = plain HTTP (`http` scheme) with username:token / password `"1"`** — allows complete visibility of your **unencrypted** HTTP traffic at the private Mint. HTTPS is untouched (no MITM certs).
2. **No TLS interception** — no `debugger`-override/certificate hooks; content targeted at page scripts only.
3. **Hardcoded credential**: Basic `George_Michae!:I'll_never_goNNa_dance_again` for `/ecommerce/...` sync on `anti-phishing-protection.urban-vpn.com` (in extension source, usable to call that API).
4. **GA4 measurement `G-9RH9D2RSVZ` + apiSecret `DAgr...`** hardcoded (fire-and-forget client-side analytics; apiSecret is a server-side secret normally!).
5. **Identification**: anonymous account (no email/password) registered on first launch; all APIs identified via Bearer security token; plus `panalyticsId` (panel identity) and per-install `userId`.
6. **The "accs-proxy" scheme**: one-time proxy username tokens minted by server for the requested signature — they cannot be reused across proxies/hosts; `password:"1"` never changes.
7. **Extension interop**: `excludeList: ngpampappnmepgilojfohadhhmbhlaek` — a known competing extension that says "never touch" (probably **AdBlock**). It also monitors `chrome.management` (both for notifications + `onInstalled` event of *other* extensions).
8. **Offscreen & webRequestAuthProvider** — workaround code described in detail (for proxy verification via dedicated worker).
9. **Hardcoded test URLs**: cloudflare, google-analytics, youtube favicon — used only for connection probes.

---

## 10. Reconstructed API surface (endpoints)

| Endpoint | Purpose |
|---|---|
| `authentication.urban-vpn.com/rest/v1` | panel identity, data sharing policy |
| `api-pro.urban-vpn.com/rest/v1/registrations/clientApps/URBAN_VPN_BROWSER_EXTENSION/users/anonymous` | anonymous register |
| `api-pro.urban-vpn.com/rest/v1/security/tokens/accs` | security session token |
| `api-pro.urban-vpn.com/rest/v1/security/tokens/accs-proxy` | per-proxy one-time auth token |
| `api-pro.urban-vpn.com/rest/v1/redirect` | install/uninstall tracking URL |
| `api-pro.urban-vpn.com/rest/v1/storeLinks` | download links |
| `config-toolbar.urban-vpn.com/rest/v3/configs/extensions/urban-vpn` | remote config (promotions, features) |
| `geo.geosurf.io/<ip?>` | GeoIP (proxy IP / own IP / served IP) |
| `geo.geosurf.io/entrypoints/{autoserver,countries,streamingservers}` | proxy server lists |
| `stats.urban-vpn.com/api/rest/v2` | stats |
| `analytics.urban-vpn.com/rest/v1` | internal analytics |
| Google Analytics 4 `G-9-9RH9D2RSVZ` (`analytics.google.com/mp/collect`) | GA4 events |
| `analytics-toolbar.urban-vpn.com/tickets[/{id}]`, `/install`, `/configuration?pv=2.1.2` | ad tickets (analytics) |
| `analytics-toolbar.urban-vpn.com/api/identity/{cache,cookie}` | identity support |
| `ai-user-protection.urban-vpn.com/api/rest/v2/secure/urls/checkSafety` (+`/basic`), `secure/content/checkUserPrivacy` | AI URL analysis |
| `anti-phishing-protection.urban-vpn.com/rest/v1` | safe price check, trace(d) |
| `anti-phishing-protection.urban-vpn.com/rest/v2` | ecommerce sync (configs/templates) (Basic fixed cred) |
| `www.instagram.com/api/v1/users/web_profile_info/` | (buggy) direct profile fetch for ad-blocker userinfo |
| `notify.bugsnag.com`, `sessions.bugsnag.com` | error reporting |

---

## 11. Full startup flow (service worker)

1. Load Bugsnag (notify/session endpoints)
2. `prepareProxySettings(false)` — store-local state adoption, on-chrome `startup` restore
3. Gate on `fallbackConfig` (`v1.ext.bkp-api.urban-vpn.com` → `urban-vpn.com` domain fallback)
4. `importScripts("anti-malware.js")` + `importScripts("../ad-blocker/background.js")`
5. `makeMario(...)` with ~40 modules and options (URLs listed above)
6. Now uses: `registerModule(MarioDataSharingPolicyModule, {distributorId 5, partnerId 5, panelistId: PanalyticsId})`
7. `MarioLocationProcessorModule` starts with sync 60 s
8. Popup fetch config with disabled banner promos
9. `MarioEcommerceModule` subscribes to ECOMMERCE_SAFE_CHECK + INFO messages
10. `MarioAdblockerModule`, `MarioAdBlockerProcessorModule` (with `adTypes` list), `MarioAntiMaleWareModule` (after user agrees??) — data collected if policy agreed
11. `MarioHelloGoodbyeModule` (install/uninstall sink URLs)
12. `mario.work()` → battle
</template>

---

---

## 12. Live verification (curl, date of test)

Full protocol exercised end-to-end against production, using a freshly created anonymous account:

| Step | Request | Result |
|---|---|---|
| 1 | `POST api-pro.urban-vpn.com/rest/v1/registrations/clientApps/URBAN_VPN_BROWSER_EXTENSION/users/anonymous` `{clientApp:{name,browser}}` | 200 → `{type:"anonm", value:<authToken>, owner.id:<anon uuid>}` |
| 2 | `POST .../security/tokens/accs` `Bearer <authToken>` `{type:"accs",clientApp}` | 200 → JWT (HS256 + `zip:DEF`); claims: `iss:"urban-vpn"`, 1 h expiry, `features:[autoserver]`, `package:{name:"Default restricted",premiumAccess:false}`, ~200 `locations[]`, `encryptedClaim` (entitlements) |
| 3 | `GET stats.urban-vpn.com/api/rest/v2/entrypoints/autoserver` (Bearer) | 200 → `{server:{signature,address:{primary:{host,port:8081},secondary[]},weight,pool}}` (`p-us51`, 169.197.85.174:8081) |
| 3b | `GET .../entrypoints/countries` (Bearer) | 200 → 104 KB, ~200 countries × servers each with per-server `signature` |
| 4 | `POST .../security/tokens/accs-proxy` `{type:"accs-proxy",clientApp,signature:<server sig>}` | 200 → `{value:<30-min token>, package:"Default restricted"}` |
| 5 | `curl -x http://<AE server>:8081 -U <value>:1 https://api.ipify.org` | 200 → **egress IP = proxy IP** (5.42.206.146, Fujairah AE, Melbikomas UAB); direct IP was 157.20.35.19 ID |

Anti-abuse / validity findings:

- Without creds → `407` (real HTTP CONNECT proxy auth).
- Password is **not validated** — `-U <token>:2` also tunnels. Only the username token matters.
- The token is **bound to the server** — same token against a different server (146.70.241.122) → `407`.
- `security/tokens/accs` requires the anonymous Bearer; `accs-proxy` also requires it (invalid sig → `400 error.invalid-request`).
- Hardcoded sync credential works: `anti-phishing-protection.urban-vpn.com/rest/v2/ecommerce/template/config` with Basic `George_Michae!:I'll_never_goNNa_dance_again` → 400 only because `libVersion` param missing (auth passed).
- `config-toolbar.urban-vpn.com/rest/v3/configs/extensions/urban-vpn` → public config JSON (promotion footer).
- `geo.geosurf.io/` → public GeoIP JSON (proxy IP, `loc:[lng,lat]`, isp, state...) — used for serving-IP display / hide-location spoofing.

Per-server `signature` (base64, ~88 B) is required to mint the auth username; without the signature the proxy connection is impossible. Server list route lives on **stats.urban-vpn.com** (registered as StaticProxiesModule apiUrl in the bundle) — NOT geo.geosurf.io (which is only the IpInfo module).

## 13. All-country fleet test

Every country (first server per country) from `entrypoints/countries` was exercised end-to-end: mint accs-proxy token → CONNECT tunnel → `api.ipify.org` probe → GeoIP check of egress IP. Results in `all_countries_test.json` (81 rows).

- **81 countries listed, 80/81 first-pick servers live** (VN server `103.97.125.216` flaked once; its second server `103.9.78.107` works, both egress correctly).
- **No duplicate egress IPs** — each country has its own server.
- **Geo-fidelity: only 1 mismatch** — `CN` (marketed as China) actually egresses from **Hong Kong (CommuniLink)**, so the "China" location is HK-routed.
- Fleet is **datacenter IPs, not residential**: top carriers M247 Europe (14), Sondatech (5), Host Universal, GTHost, Host Africa, Latitude.sh, Navegalo, EstNOC, Contabo, OVH... representative leaks: DE=170.101.109.135 (GTHost, Frankfurt), RU=M247-Moscow, US=146.70.228.82 (M247, Miami). Only a few look local (SG.GS for SG, Gigabit Hosting for MY/TW).
- Every proxy is `http://<ip>:8081`, scheme HTTP, auth `username=<accs-proxy token>, password ignored`.
- The `locations[]` list in the security JWT == server list here; `package: "Default restricted"` gates premiumAccess: false for anonymous accounts.

Full per-country table: `/root/unroxy/all_countries_test.json`.

*Scope: static analysis of the CRX payload + live HTTP probes. No traffic interception/MITM; HTTPS is proxied as CONNECT tunnel only.*