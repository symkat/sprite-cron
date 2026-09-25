# Sprite HTTP probe

A small server returning fixed JSON on `/` and `/health`. It listens on
`0.0.0.0:8080` and relies on the Sprite proxy's authenticated URL mode. It does
not serve files or expose environment variables or request headers.

## Deployed instance

Configured September 25, 2026 on `test-sprite-cron`:

- Service: `sprite-cron-http`
- Source on the Sprite: `/home/sprite/sprite-cron-http/server.py`
- URL: https://test-sprite-cron-7gce.sprites.app
- Listener and service HTTP port: `8080`
- URL authentication: `sprite` (private access: `admins`)

After copying `server.py` to that path, the service was registered inside the
Sprite with:

```sh
sprite-env services create sprite-cron-http \
  --cmd /.sprite/bin/python3 \
  --args /home/sprite/sprite-cron-http/server.py \
  --http-port 8080
```

Startup logged `sprite-cron-http listening on 0.0.0.0:8080` and service status
reported `running`. The service remains installed for subsequent testing.

## Access

With your Sprite token already available in `SPRITES_TOKEN`:

```sh
curl --fail-with-body \
  -H "Authorization: Bearer $SPRITES_TOKEN" \
  https://test-sprite-cron-7gce.sprites.app/health
```

Expected body:

```json
{"status": "ok"}
```

Verified live:

| Request | Result |
| --- | --- |
| Authenticated `GET /` through the Sprite URL | 200, `{"service": "sprite-cron-http", "ok": true}` |
| Authenticated `GET /health` through the Sprite URL | 200, `{"status": "ok"}` |
| Unauthenticated `GET /health`, redirects disabled | 302 authentication redirect |
| `GET http://127.0.0.1:8080/health` inside the Sprite | 200, `{"status": "ok"}` |

No token is stored in the server, service definition, or repository. Keep the
Sprite URL authenticated; the application itself has no login mechanism.

## Management

Run these commands inside the Sprite:

```sh
sprite-env services get sprite-cron-http
sprite-env services restart sprite-cron-http
```

The platform service log is at
`/.sprite/logs/services/sprite-cron-http.log`. To remove the service deliberately,
use `sprite-env services delete sprite-cron-http`.
