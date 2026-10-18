import asyncio
import json
import queue
import socket
import threading
import time
import unittest
import zlib
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from types import SimpleNamespace
from unittest.mock import patch

import bridge
from fastapi import HTTPException


class BridgeTests(unittest.TestCase):
    def test_auth_does_not_accept_admin_or_empty_credentials(self):
        with patch.object(bridge, 'engine_key', return_value='valid-pool-secret'):
            for value in ('', 'Basic admin', 'Bearer invalid', 'Bearer 非法'):
                with self.assertRaises(HTTPException) as caught:
                    bridge.authenticate(SimpleNamespace(headers={'authorization': value}))
                self.assertEqual(caught.exception.status_code, 401)
            bridge.authenticate(SimpleNamespace(headers={'authorization': 'Bearer valid-pool-secret'}))

    def test_models_never_call_gateway(self):
        with patch.object(bridge, 'model_config', return_value={'model_alias': 'configured-model', 'context': 262144}), patch.object(bridge.requests, 'get') as get, patch.object(bridge.requests, 'post') as post:
            self.assertEqual(bridge.models()['data'][0]['id'], 'configured-model')
            get.assert_not_called()
            post.assert_not_called()

    def test_invalid_model_does_not_record_demand(self):
        with patch.object(bridge, 'model_config', return_value={'model_alias': 'configured-model'}), patch.object(bridge.requests, 'post') as post:
            with self.assertRaises(HTTPException):
                bridge.chat({'model': 'wrong', 'messages': [{'role': 'user', 'content': 'hi'}]})
            post.assert_not_called()

    def test_status_allowlist_and_oldest_balance_sample(self):
        now = time.time()
        state = {'now': now, 'summary': {'balance': 10, 'burn': 0, 'used_24h': 3, 'ready': 0, 'starting': 0},
                 'settings': {'min_start_cu': 8, 'auto_start': True, 'max_instances': 2, 'idle_stop_min': 60, 'gpu': 'G4', 'secret': 'do-not-export'},
                 'accounts': [{'id': 1, 'enabled': True, 'status': 'ok', 'balance': 1, 'checked_at': now - 700, 'email': 'private@example.com', 'gpus': ['G4']},
                              {'id': 2, 'enabled': True, 'status': 'ok', 'balance': 9, 'checked_at': now, 'gpus': ['G4']}],
                 'instances': [{'id': 24, 'state': 'stopped', 'running': 0, 'waiting': 0, 'email': 'private@example.com', 'phase': 'sensitive log'}],
                 'events': [{'message': 'sensitive log'}]}
        result = bridge.public_status(state, {'model_alias': 'model'}, now)
        self.assertTrue(result['quota_stale'])
        self.assertEqual(result['eligible_accounts'], 1)
        self.assertEqual(result['balance_cu'], 10)
        encoded = json.dumps(result)
        for secret in ('private@example.com', 'do-not-export', 'sensitive log'):
            self.assertNotIn(secret, encoded)

    def test_eligible_accounts_require_exact_configured_gpu(self):
        now = time.time()

        def eligible(**extra):
            account = {'id': 1, 'enabled': True, 'status': 'ok', 'balance': 9, 'checked_at': now}
            account.update(extra)
            state = {'now': now, 'summary': {'ready': 0, 'starting': 0},
                     'settings': {'min_start_cu': 8, 'auto_start': True, 'max_instances': 2, 'idle_stop_min': 60, 'gpu': 'G4'},
                     'accounts': [account], 'instances': []}
            return bridge.public_status(state, {'model_alias': 'model'}, now)['eligible_accounts']

        self.assertEqual(eligible(gpus=['G4']), 1)
        self.assertEqual(eligible(gpus=['T4', 'G4']), 1)
        # Sufficient balance but GPU mismatch must not count.
        self.assertEqual(eligible(gpus=['T4']), 0)
        self.assertEqual(eligible(), 0)
        self.assertEqual(eligible(gpus=None), 0)
        self.assertEqual(eligible(gpus=[]), 0)
        # Strings must not be treated as lists (no substring matching).
        self.assertEqual(eligible(gpus='G4'), 0)
        self.assertEqual(eligible(gpus='G4,T4'), 0)
        self.assertEqual(eligible(gpus=['G40']), 0)
        self.assertEqual(eligible(gpus=['g4']), 0)

    def test_small_sse_event_is_forwarded_before_upstream_eof_chunked(self):
        self.assert_small_event_forwarded_before_eof(chunked=True)

    def test_small_sse_event_is_forwarded_before_upstream_eof_close_delimited(self):
        self.assert_small_event_forwarded_before_eof(chunked=False)

    def test_gzip_sse_event_is_decoded_and_forwarded_before_upstream_eof(self):
        self.assert_small_event_forwarded_before_eof(chunked=True, encoding='gzip')

    def test_deflate_sse_event_is_decoded_and_forwarded_before_upstream_eof(self):
        self.assert_small_event_forwarded_before_eof(chunked=True, encoding='deflate')

    def test_gzip_json_response_is_fully_decoded(self):
        self.assert_compressed_json_decoded('gzip')

    def test_deflate_json_response_is_fully_decoded(self):
        self.assert_compressed_json_decoded('deflate')

    @staticmethod
    def compressor(encoding):
        # gzip container (wbits 31) or HTTP "deflate" = zlib-wrapped (wbits 15).
        return zlib.compressobj(wbits=31 if encoding == 'gzip' else 15)

    def assert_compressed_json_decoded(self, encoding):
        payload = json.dumps({'id': 'chatcmpl-1', 'object': 'chat.completion',
                              'choices': [{'index': 0, 'message': {'role': 'assistant', 'content': 'hello ' * 200}}]}).encode()
        compressor = self.compressor(encoding)
        body = compressor.compress(payload) + compressor.flush()
        self.assertNotEqual(body, payload)

        class Upstream(BaseHTTPRequestHandler):
            protocol_version = 'HTTP/1.1'

            def log_message(self, *args):
                pass

            def do_POST(self):
                self.rfile.read(int(self.headers.get('Content-Length', 0)))
                self.send_response(200)
                self.send_header('Content-Type', 'application/json')
                self.send_header('Content-Encoding', encoding)
                self.send_header('Content-Length', str(len(body)))
                self.end_headers()
                self.wfile.write(body)
                self.wfile.flush()

        server = ThreadingHTTPServer(('127.0.0.1', 0), Upstream)
        server.daemon_threads = True
        server_thread = threading.Thread(target=server.serve_forever, kwargs={'poll_interval': 0.05}, daemon=True)
        server_thread.start()
        port = server.server_address[1]
        real_post = bridge.requests.post
        closes = []

        def redirected(url, **kwargs):
            response = real_post('http://127.0.0.1:%d/v1/chat/completions' % port, **kwargs)
            original_close = response.close

            def tracked_close():
                closes.append(True)
                original_close()
            response.close = tracked_close
            return response

        received = queue.Queue()
        consumer = None
        try:
            with patch.object(bridge, 'model_config', return_value={'model_alias': 'model'}), \
                    patch.object(bridge, 'engine_key', return_value='secret'), \
                    patch.object(bridge.requests, 'post', side_effect=redirected):
                result = bridge.chat({'model': 'model', 'messages': [{'role': 'user', 'content': 'hi'}]})
            self.assertEqual(result.status_code, 200)
            self.assertTrue(result.headers['content-type'].startswith('application/json'))
            self.assertEqual(result.headers['cache-control'], 'no-store')
            self.assertEqual(result.headers['x-accel-buffering'], 'no')
            self.assertNotIn('content-encoding', result.headers)
            self.assertIsNotNone(result.background)

            async def consume():
                iterator = result.body_iterator
                chunks = []
                try:
                    async for chunk in iterator:
                        chunks.append(chunk)
                    received.put(b''.join(chunks))
                except BaseException as error:  # surface failures to the test thread
                    received.put(error)
                finally:
                    await iterator.aclose()
                    await result.background()

            consumer = threading.Thread(target=lambda: asyncio.run(consume()), daemon=True)
            consumer.start()
            try:
                data = received.get(timeout=5)
            except queue.Empty:
                self.fail('compressed JSON response was not fully read')
            if isinstance(data, BaseException):
                raise data
            self.assertEqual(data, payload)
            self.assertEqual(json.loads(data)['id'], 'chatcmpl-1')
            consumer.join(5)
            self.assertTrue(closes, 'upstream response was not closed')
        finally:
            if consumer is not None:
                consumer.join(10)
                self.assertFalse(consumer.is_alive())
            server.shutdown()
            server.server_close()
            server_thread.join(5)
            self.assertFalse(server_thread.is_alive())

    def assert_small_event_forwarded_before_eof(self, chunked, encoding=None):
        event = b'data: {"choices":[]}\n\n'
        release = threading.Event()      # lets the upstream finish the response
        finished = threading.Event()     # upstream wrote the terminating chunk
        client_closed = threading.Event()
        seen = {}
        if encoding is None:
            wire = event
        else:
            # Incremental compression with a sync flush: the whole event is
            # decodable now, but the compressed stream is deliberately unfinished.
            compressor = self.compressor(encoding)
            wire = compressor.compress(event) + compressor.flush(zlib.Z_SYNC_FLUSH)
            self.assertNotIn(event, wire)

        class Upstream(BaseHTTPRequestHandler):
            protocol_version = 'HTTP/1.1'

            def log_message(self, *args):
                pass

            def do_POST(self):
                seen['path'] = self.path
                seen['auth'] = self.headers.get('Authorization')
                self.rfile.read(int(self.headers.get('Content-Length', 0)))
                self.send_response(200)
                self.send_header('Content-Type', 'text/event-stream')
                if encoding is not None:
                    self.send_header('Content-Encoding', encoding)
                if chunked:
                    self.send_header('Transfer-Encoding', 'chunked')
                else:
                    # No length and no chunking: the body ends when the socket closes.
                    self.send_header('Connection', 'close')
                    self.close_connection = True
                self.end_headers()
                self.wfile.write(b'%x\r\n%s\r\n' % (len(wire), wire) if chunked else wire)
                self.wfile.flush()
                # Keep the stream open until released or the client disconnects.
                self.connection.settimeout(0.05)
                deadline = time.monotonic() + 5
                while not release.is_set() and time.monotonic() < deadline:
                    try:
                        if self.connection.recv(1) == b'':
                            client_closed.set()
                            return
                    except socket.timeout:
                        continue
                    except OSError:
                        client_closed.set()
                        return
                try:
                    if chunked:
                        self.wfile.write(b'0\r\n\r\n')
                        self.wfile.flush()
                    finished.set()
                except OSError:
                    client_closed.set()

        server = ThreadingHTTPServer(('127.0.0.1', 0), Upstream)
        server.daemon_threads = True
        server_thread = threading.Thread(target=server.serve_forever, kwargs={'poll_interval': 0.05}, daemon=True)
        server_thread.start()
        port = server.server_address[1]
        real_post = bridge.requests.post
        targets = []

        def redirected(url, **kwargs):
            targets.append(url)
            return real_post('http://127.0.0.1:%d/v1/chat/completions' % port, **kwargs)

        received = queue.Queue()
        stop = threading.Event()
        consumer = None
        try:
            with patch.object(bridge, 'model_config', return_value={'model_alias': 'model'}), \
                    patch.object(bridge, 'engine_key', return_value='secret'), \
                    patch.object(bridge.requests, 'post', side_effect=redirected):
                result = bridge.chat({'model': 'model', 'messages': [{'role': 'user', 'content': 'hi'}], 'stream': True})
            self.assertEqual(targets, ['http://127.0.0.1:18801/v1/chat/completions'])
            self.assertEqual(seen['path'], '/v1/chat/completions')
            self.assertEqual(seen['auth'], 'Bearer secret')
            self.assertEqual(result.status_code, 200)
            self.assertTrue(result.headers['content-type'].startswith('text/event-stream'))
            self.assertEqual(result.headers['cache-control'], 'no-store')
            self.assertEqual(result.headers['x-accel-buffering'], 'no')
            self.assertIsNotNone(result.background)
            self.assertNotIn('content-encoding', result.headers)

            async def consume():
                iterator = result.body_iterator
                try:
                    data = await iterator.__anext__()
                    # Uncompressed: the first chunk must be the whole event.
                    # Decoded output may be split, so accumulate (still bounded
                    # by the test-thread timeout below) until the event length.
                    while encoding is not None and len(data) < len(event):
                        data += await iterator.__anext__()
                    received.put(data)
                    await asyncio.to_thread(stop.wait, 5)
                except BaseException as error:  # surface failures to the test thread
                    received.put(error)
                finally:
                    await iterator.aclose()
                    await result.background()  # the production upstream close hook

            consumer = threading.Thread(target=lambda: asyncio.run(consume()), daemon=True)
            consumer.start()
            try:
                first = received.get(timeout=2)
            except queue.Empty:
                self.fail('small SSE event was not forwarded before upstream EOF')
            self.assertEqual(first, event)
            self.assertFalse(finished.is_set(), 'event must arrive while the upstream stream is still open')
            stop.set()
            self.assertTrue(client_closed.wait(3), 'upstream connection was not closed')
            self.assertFalse(finished.is_set())
        finally:
            stop.set()
            release.set()
            if consumer is not None:
                consumer.join(10)
                self.assertFalse(consumer.is_alive())
            server.shutdown()
            server.server_close()
            server_thread.join(5)
            self.assertFalse(server_thread.is_alive())

    def test_offline_closes_upstream_and_returns_retryable_error(self):
        with patch.object(bridge, 'model_config', return_value={'model_alias': 'model'}), patch.object(bridge, 'engine_key', return_value='secret'), patch.object(bridge.requests, 'post') as post:
            post.return_value.status_code = 503
            result = bridge.chat({'model': 'model', 'messages': [{'role': 'user', 'content': 'hi'}]})
            self.assertEqual(result.status_code, 503)
            self.assertEqual(result.headers['retry-after'], '30')
            post.return_value.close.assert_called_once()
            self.assertFalse(post.call_args.kwargs['allow_redirects'])
            self.assertEqual(post.call_args.args[0], 'http://127.0.0.1:18801/v1/chat/completions')


if __name__ == '__main__':
    unittest.main()
