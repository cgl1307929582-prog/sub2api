import json
import time
import unittest
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
                 'accounts': [{'id': 1, 'enabled': True, 'status': 'ok', 'balance': 1, 'checked_at': now - 700, 'email': 'private@example.com'},
                              {'id': 2, 'enabled': True, 'status': 'ok', 'balance': 9, 'checked_at': now}],
                 'instances': [{'id': 24, 'state': 'stopped', 'running': 0, 'waiting': 0, 'email': 'private@example.com', 'phase': 'sensitive log'}],
                 'events': [{'message': 'sensitive log'}]}
        result = bridge.public_status(state, {'model_alias': 'model'}, now)
        self.assertTrue(result['quota_stale'])
        self.assertEqual(result['eligible_accounts'], 1)
        self.assertEqual(result['balance_cu'], 10)
        encoded = json.dumps(result)
        for secret in ('private@example.com', 'do-not-export', 'sensitive log'):
            self.assertNotIn(secret, encoded)

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
