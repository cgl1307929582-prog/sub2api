# Native Colab management in Sub2API

The **Colab GPU Pool** top-level administrator sidebar entry opens `/admin/colab`.
This is a native Vue page, authenticated by Sub2API. It replaces the standalone
Pool dashboard and does not embed or navigate to it. Features include overview,
Google OAuth accounts, credit refresh, enable/disable, priority, notes, deletion,
instance start/stop/drain, hardware monitoring, scheduler logs and all scheduling
settings. Mutations use Sub2API's existing administrator, compliance and audit
middleware. Authorization codes are covered by audit-body redaction.

The existing Python Colab scheduler and telemetry collector continue as private
workers. Their account database, OAuth tokens, provisioning logic, model files,
SSH tunnels and instance history stay intact. They need no public domain.
Sub2API alone exposes the management UI and client model gateway.

## Private connection

1. Back up Sub2API's compose file, environment, PostgreSQL data, the old Pool
   nginx virtual host, and the Pool's SQLite database/account directory.
2. Adapt `private-gateway.conf.example` to the actual Sub2API Docker network
   gateway/subnet. It must listen only on that private bridge address. Validate
   with `nginx -t` and reload. The control API still requires the Pool internal
   secret; inference still requires the separate inference key. Never insert a
   fixed control credential into a public nginx location.
   If a host firewall is enabled, allow only the Sub2API bridge interface/subnet
   to reach that private address and port. Test connectivity from the application
   network namespace; host-originated requests may be masqueraded by Docker and
   correctly rejected by the nginx subnet restriction.
3. Make the Pool internal secret available only to the application as a
   read-only mounted file, e.g. `/run/colab-pool/secret`, readable by the app UID.
   Configure these deployment variables (not ordinary account credentials):

   ```yaml
   environment:
     - COLAB_POOL_CONTROL_URL=http://172.21.0.1:18903/control
     - COLAB_POOL_CONTROL_SECRET_FILE=/run/colab-pool/secret
   volumes:
     - ./colab-control/secret:/run/colab-pool/secret:ro
   ```

   The control URL is a fixed operator-controlled origin, not a request parameter.
   No caller cookies or authorization headers are forwarded; redirects fail
   closed. Configure URL policy for the private inference endpoint according to
   your deployment's existing security policy.
4. Change the existing Sub2API `colab` inference account base URL to the private
   `/v1` address, preserve its inference key/group/settings, and synchronize its
   model catalog. No Google token or control secret belongs in this account.
5. Verify `/admin/colab`, administrator actions, ordinary-user/anonymous rejection,
   model sync, quota, and real Sub2API inference. Test settings reversibly and
   restore the original values. Stop the validation GPU and verify zero active
   assignments before finishing.
6. Retire the old public virtual host after native acceptance. Its root may
   redirect to Sub2API; all legacy API paths should return 410. Keep the private
   workers running. Remove any old public bridge location rather than leaving a
   second unmetered inference entrypoint.

## Semantics

- Snapshot reads never call inference and never record demand.
- GPU start requires explicit confirmation in the UI. Drain waits for current
  requests; immediate stop interrupts them. Google account deletion also requires
  confirmation and the worker refuses deletion while its instance is active.
- Hardware samples are approximately 30 seconds apart; stale/failed readings are
  marked. Instance HTTP totals are from worker logs. The former New API gateway
  totals are excluded. Native usage cards and recent calls query Sub2API with
  explicit Colab account filters; today's totals follow the browser timezone.
- CU remains compute credit, distinct from downstream token charges in USD.
- A Google OAuth flow retains only its pending session ID in browser session
  storage. The PKCE verifier and refresh tokens remain private to the worker.
- If the worker is unavailable, the UI retains the last snapshot with a warning.
  Polling does not overwrite unsaved scheduling edits.

## Rollback

Restore the previous application image/compose/environment, the inference
account's previous base URL, and its matching model catalog. Restore the old
nginx virtual host only if returning to the standalone dashboard. Validate nginx
before reload. Preserve current usage and account databases; do not restore an
old database over new usage merely to roll back application code.
