"""Narrow authenticated Sub2API adapter for the existing on-demand Colab Pool.

Metadata never contacts the inference gateway. Only a validated Chat POST can
record demand. No lifecycle, OAuth, account identity, or administrative routes
are exposed. Paths and localhost targets are deployment configuration only.
"""
import hmac
import json
import math
import os
import time
from datetime import datetime, timezone
from pathlib import Path

import requests
from fastapi import Body, Depends, FastAPI, HTTPException, Request
from starlette.background import BackgroundTask
from starlette.responses import JSONResponse, StreamingResponse

POOL_ROOT = Path(os.getenv('COLAB_POOL_ROOT', '/opt/colab-pool'))
MODEL_ROOT = Path(os.getenv('COLAB_MODEL_ROOT', '/opt/qwen'))
app = FastAPI(docs_url=None, redoc_url=None, openapi_url=None)


def engine_key():
    value = (MODEL_ROOT / 'api-key').read_text().strip()
    if len(value) < 8:
        raise ValueError('Unconfigured API key')
    return value


def authenticate(request: Request):
    try:
        expected = 'Bearer ' + engine_key()
    except (OSError, ValueError):
        raise HTTPException(503, 'Pool authentication is unavailable') from None
    # Compare bytes: unexpected Unicode headers must fail closed, not raise 500.
    supplied = request.headers.get('authorization', '')
    if not hmac.compare_digest(supplied.encode(), expected.encode()):
        raise HTTPException(401, 'Invalid Pool API key')


def model_config():
    try:
        config = json.loads((MODEL_ROOT / 'inference-config.json').read_text())
        if not isinstance(config.get('model_alias'), str) or not config['model_alias'].strip():
            raise ValueError('Missing model')
        return config
    except (OSError, ValueError, AttributeError):
        raise HTTPException(503, 'Pool model configuration is unavailable') from None


def number(value):
    if type(value) in (int, float) and math.isfinite(value) and value >= 0:
        return value
    return None


def timestamp(value):
    value = number(value)
    if not value:
        return None
    try:
        return datetime.fromtimestamp(value, timezone.utc).isoformat()
    except (ValueError, OverflowError, OSError):
        return None


def public_status(state, config, now):
    summary = state['summary']
    accounts = state['accounts']
    settings = state['settings']
    instances = state['instances']
    usable = [a for a in accounts if a.get('enabled') and a.get('status') == 'ok']
    checked = [number(a.get('checked_at')) for a in usable]
    oldest = min(checked) if checked and all(checked) else None
    # "usable" in the dashboard includes accounts below the startup threshold.
    # Mirror the admin start check: gpus must be a list containing the exact
    # configured GPU (no substring matching on strings, missing/null/empty fail).
    gpu = settings.get('gpu')
    eligible = [a for a in usable if number(a.get('balance')) is not None
                and a['balance'] >= settings['min_start_cu']
                and not a.get('cooldown') and not a.get('instance')
                and not a.get('assignments')
                and isinstance(a.get('gpus'), list) and gpu in a['gpus']]
    public_instances = []
    known_states = {'starting', 'ready', 'draining', 'stopping', 'stopped', 'error', 'failed'}
    for instance in instances[:30]:
        public_instances.append({
            'id': instance['id'],
            'state': instance['state'] if instance['state'] in known_states else 'unknown',
            'gpu': settings.get('gpu', ''),
            'running': int(number(instance.get('running')) or 0),
            'waiting': int(number(instance.get('waiting')) or 0),
            'tokens_per_second': number(instance.get('tps')),
            'created_at': timestamp(instance.get('created_at')),
            'last_active_at': timestamp(instance.get('last_active')),
        })
    # This is an explicit allowlist, never pass through accounts/events/settings.
    return {
        'version': 1, 'model_id': config['model_alias'],
        'balance_cu': number(summary.get('balance')),
        'burn_cu_per_hour': number(summary.get('burn')),
        'used_cu_24h': number(summary.get('used_24h')),
        'accounts': len(accounts), 'eligible_accounts': len(eligible),
        'ready': summary['ready'], 'starting': summary['starting'],
        'auto_start': bool(settings['auto_start']), 'max_instances': settings['max_instances'],
        'idle_stop_minutes': settings['idle_stop_min'],
        'observed_at': timestamp(state['now']),
        'quota_updated_at': timestamp(oldest),
        'quota_stale': oldest is None or now - oldest > 600,
        'instances': public_instances,
    }


@app.get('/v1/models', dependencies=[Depends(authenticate)])
def models():
    config = model_config()
    entry = {'id': config['model_alias'], 'object': 'model', 'owned_by': 'colab',
             'supported_endpoints': ['chat_completions'], 'input_modalities': ['text']}
    # Configuration describes capacity, not a live GPU health assertion. Do not
    # guess reasoning/tool capabilities from the model name.
    if number(config.get('context')):
        entry['context_window'] = config['context']
    return {'object': 'list', 'data': [entry]}


@app.get('/v1/pool/status', dependencies=[Depends(authenticate)])
def status():
    config = model_config()
    try:
        secret = (POOL_ROOT / 'internal-secret').read_text().strip()
        with requests.get('http://127.0.0.1:18900/api/state',
                          headers={'X-Internal-Auth': secret}, timeout=(3, 10),
                          allow_redirects=False) as response:
            response.raise_for_status()
            return public_status(response.json(), config, time.time())
    except (OSError, ValueError, KeyError, TypeError, requests.RequestException):
        raise HTTPException(503, 'Pool status is temporarily unavailable') from None


@app.post('/v1/chat/completions', dependencies=[Depends(authenticate)])
def chat(body: dict = Body(...)):
    config = model_config()
    if body.get('model') != config['model_alias']:
        raise HTTPException(400, 'Unknown Pool model; refresh /v1/models')
    if not isinstance(body.get('messages'), list) or not body['messages']:
        raise HTTPException(400, 'messages must be a non-empty array')
    try:
        response = requests.post('http://127.0.0.1:18801/v1/chat/completions',
                                 headers={'Authorization': 'Bearer ' + engine_key(),
                                          'Accept-Encoding': 'identity'},
                                 json=body, stream=True, timeout=(5, 900), allow_redirects=False)
    except (OSError, ValueError, requests.RequestException):
        return unavailable()
    if response.status_code in (502, 503, 504):
        response.close()
        return unavailable()
    if 300 <= response.status_code < 400:
        response.close()
        return unavailable()

    def content():
        try:
            yield from upstream_chunks(response)
        finally:
            response.close()

    return StreamingResponse(content(), status_code=response.status_code,
                             media_type=response.headers.get('Content-Type', 'application/json'),
                             headers={'Cache-Control': 'no-store', 'X-Accel-Buffering': 'no'},
                             background=BackgroundTask(response.close))


def upstream_chunks(response):
    """Yield upstream bytes as soon as they arrive, never waiting to fill a
    fixed-size buffer: small SSE events must not stall until more data or EOF.

    read1 returns whatever is available for chunked, length-delimited and
    close-delimited bodies (urllib3 2.x). iter_content(chunk_size=None) is the
    fallback; it is immediate for chunked bodies, which SSE servers use.

    Requests opens the raw stream with decode_content=False, so read1 must
    decode explicitly: Accept-Encoding: identity is only a preference, and the
    downstream response never carries Content-Encoding. urllib3's read1 keeps
    reading until the decoder yields bytes (empty only at EOF), so a
    sync-flushed compressed event is still forwarded without waiting for EOF.
    """
    read1 = getattr(response.raw, 'read1', None)
    if read1 is None:
        yield from response.iter_content(chunk_size=None)
        return
    while True:
        data = read1(65536, decode_content=True)
        if not data:
            return
        yield data


def unavailable():
    return JSONResponse(status_code=503, headers={'Retry-After': '30'}, content={
        'error': {'type': 'server_error', 'code': 'pool_not_ready',
                  'message': 'GPU Pool is offline or starting. With auto-start enabled, this request records demand. Cold start can take 10-15 minutes; check Pool status before retrying.'}})


if __name__ == '__main__':
    import uvicorn
    uvicorn.run(app, host='127.0.0.1', port=18902, access_log=False)
