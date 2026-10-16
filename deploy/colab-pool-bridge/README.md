# Colab / GPU Pool channel

This adapter connects an existing Colab GPU Pool to the native `colab` platform
in Sub2API. It preserves the Pool's OAuth accounts, autoscaling, rotation and
idle shutdown. The bridge does not expose account identities, OAuth tokens,
settings changes, instance creation or stop operations.

The Pool must already provide `GET http://127.0.0.1:18900/api/state`, authenticated
with `/opt/colab-pool/internal-secret`, and the on-demand inference gateway at
`127.0.0.1:18801`. Model configuration and inference key are read from
`/opt/qwen/inference-config.json` and `/opt/qwen/api-key`. These paths can be
changed with `COLAB_POOL_ROOT` and `COLAB_MODEL_ROOT`. This is an integration for
that self-hosted Pool contract, not a general Google Colab API.

## Install on the Pool host

1. Back up the existing TLS virtual host. Copy `bridge.py` and
   `nginx-location.conf` to `/opt/colab-pool/sub2api-bridge/`.
2. The existing `/opt/qwen/venv` needs FastAPI, Requests and Uvicorn. Copy the
   systemd unit to `/etc/systemd/system/`, then run `systemctl daemon-reload` and
   `systemctl enable --now colab-sub2api-bridge`.
3. Add `limit_req_zone $binary_remote_addr zone=colab_sub2api:10m rate=30r/s;`
   to nginx's `http` context, and include `nginx-location.conf` inside the
   existing HTTPS Pool server block. Run `nginx -t` before reloading nginx.
   Dashboard Basic Auth remains in place; the `/sub2api/` prefix uses the
   inference key's Bearer authentication. The bridge listens on loopback only.
4. In Sub2API, create a **Colab / GPU Pool** group and API-key account, enter
   `https://your-pool.example/sub2api/v1` and the Pool inference key, then bind
   the account to that group. The default adaptive client protocol uses Chat
   Completions upstream and converts Messages/Responses clients automatically.

New accounts created in the UI sync the configured model catalog automatically.
Use **Sync upstream models** after changing the Pool model. Public model lists
use the saved catalog plus explicit administrator aliases/restrictions, never
an unrelated provider's default models. Catalog sync does not prove that a GPU
is ready or that an unspecified model capability is supported.

## Contract and behavior

- `GET /v1/models`: configured model alias and configured context capacity;
  reads configuration only, works with all GPUs stopped.
- `GET /v1/pool/status`: version 1 status, CU balance, CU/hour, 24h consumption,
  available account count, ready/starting counts and recent instances. This
  reads the existing control API and never contacts the inference gateway.
- `POST /v1/chat/completions`: validates the key and configured model before
  forwarding to the existing inference gateway, including SSE streaming.
  Offline instances return HTTP 503 and `Retry-After: 30`; the valid POST records
  demand for the Pool's existing auto-start behavior. Wait for readiness rather
  than continuously retrying. Cold-start duration depends on downloading and
  building the model runtime.

All routes require `Authorization: Bearer <Pool inference key>`. There are no
administrative routes. Do not enter the dashboard password, Google OAuth token,
or Pool internal secret into Sub2API.

Sub2API caches successful status reads for 30 seconds and failures for 15 seconds;
forced refreshes share a five-second cooldown. A failure retains the previous
snapshot with a stale marker. Credential, URL or proxy changes isolate caches.
State timestamps and balance sample timestamps are separate: the Pool normally
refreshes account credits about every five minutes. Old/unknown balance samples
are marked, not silently replaced by zero.

CU is GPU compute credit, not USD or a token quota. Downstream Sub2API token
accounting and model prices remain independent; configure token prices in the
group if charging users. No automatic CU-to-USD conversion is invented.

To remove the bridge, remove its nginx location include and zone, validate and
reload nginx, then disable/stop `colab-sub2api-bridge`. The original Pool and
inference gateway continue to work. Stop validation instances through the
existing Pool dashboard and verify active assignments reach zero.

## Validation

Run `python -m unittest -v test_bridge` in this directory with the above Python
dependencies. Sub2API's `TestColab*` unit tests cover credential validation,
protocol routing, model persistence, cache isolation, read-only status requests,
URL/proxy policy and stale/unknown observations. Live acceptance must separately
exercise a real GPU through Sub2API's account test and client endpoints.
