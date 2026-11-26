# matrix-mcp

A remote [MCP](https://modelcontextprotocol.io) server that gives AI agents full access to a Matrix account, including **end-to-end encrypted** rooms and chats bridged from Messenger, Instagram, WhatsApp, Discord, iMessage and other [mautrix](https://github.com/mautrix) bridges.

It logs in as its own Matrix device, decrypts messages, and keeps a local full-text index, so an agent can search and read your chats, send messages, and manage rooms and bridges.

- **E2EE.** Built on [mautrix-go](https://github.com/mautrix/go). With your recovery key it cross-signs its device and imports the server-side key backup, so older history decrypts too.
- **Search.** SQLite FTS5 index that survives restarts and folds diacritics, so `zolc` matches `żółć`. It backfills a configurable amount of history per room, and more on demand.
- **Bridge-aware.** Detects each room's network (`m.bridge`) and the bridge management rooms, and can run bridge bot commands.
- **Remote.** Streamable HTTP. Authenticate with a static bearer token (Claude Code, Codex, scripts) or OAuth 2.1 with dynamic client registration, which claude.ai and ChatGPT custom connectors need. OAuth sign-in uses the Matrix account itself: the consent page checks your username and password with the homeserver.

## Tools

| Tool | What it does |
| --- | --- |
| `status` | Sync, encryption, key backup and index status |
| `list_rooms` | Chats by recent activity. Filter by name, network, kind, unread, `awaiting_reply` (last message not from you) and an activity date range |
| `get_room` | Room details and members |
| `read_messages` | Decrypted history, oldest first. Edits, reactions and deletions are folded in; page back with `next_before` |
| `get_message_context` | Messages around a `message_id`, e.g. a search hit |
| `search_messages` | Full-text search. Filter by sender (`me` works), room, `after`/`before` dates or the last N days |
| `backfill_room` | Index older history of a room |
| `wait_for_messages` | Long-poll for new incoming messages |
| `send_message` | Markdown or plain text, with replies and threads |
| `send_file`, `send_reaction`, `edit_message`, `delete_message`, `mark_read` | Messaging |
| `download_media` | Decrypt and return attachments. Images come inline as a 768 px thumbnail by default (`variant: full` for the original); audio also inline |
| `decrypt_events` | Decrypt raw events you already have, e.g. the originals of deleted messages from the homeserver database; `download_media` also takes such a raw `event` |
| `join_room`, `leave_room`, `invite_user`, `remove_user`, `create_room`, `update_room` | Room management, including tags |
| `bridge_command` | Send a command to a bridge bot (`help`, `list-logins`, `search`, `start-chat`, …) and collect its replies |
| `matrix_api` | Raw client-server or Synapse admin API call |

Rooms can be referenced by ID, alias or a unique part of their name. Read-only tools carry `readOnlyHint`; sending and deleting tools are marked as writes.

## Running

The server needs a Matrix account, ideally on the same network as the homeserver. Provide either a password or an access token for a dedicated Matrix device.

| Variable | Required | Notes |
| --- | --- | --- |
| `MATRIX_HOMESERVER` | | Homeserver URL. Default `http://matrix-synapse:8008` |
| `MATRIX_USER_ID` | yes | e.g. `@me:example.com` |
| `MATRIX_PASSWORD` | one of these | Logs into the Matrix account and reuses the device from the crypto store on subsequent starts |
| `MATRIX_ACCESS_TOKEN` | one of these | Access token for a dedicated Matrix device, for example a [MAS compatibility token](https://element-hq.github.io/matrix-authentication-service/reference/cli/manage.html#manage-issue-compatibility-token) |
| `MATRIX_RECOVERY_KEY` | | Recovery (SSSS) key. Verifies the device and imports the key backup so old messages decrypt |
| `PICKLE_KEY` | yes | Encrypts the crypto store. Never change it, or the device has to be recreated |
| `MCP_BEARER_TOKEN` | yes | Static client token, at least 32 characters |
| `PUBLIC_URL` | for OAuth | Public base URL, e.g. `https://matrix-mcp.example.com`. Enables OAuth for custom connectors in password mode |
| `TZ` | | Time zone for date filters without an explicit offset, e.g. `Europe/Warsaw` |
| `BACKFILL_PER_ROOM` | | Messages indexed per room on first start. Default 200 |
| `DATA_DIR`, `LISTEN_ADDR`, `DEVICE_NAME` | | Defaults `/data`, `:8080`, `Matrix MCP` |

```sh
docker build -t matrix-mcp .
docker run -d --name matrix-mcp -p 8080:8080 -v matrix-mcp-data:/data \
  -e MATRIX_HOMESERVER=https://matrix.example.com -e MATRIX_USER_ID=@me:example.com \
  -e MATRIX_PASSWORD=... -e PICKLE_KEY="$(openssl rand -hex 32)" -e MCP_BEARER_TOKEN="$(openssl rand -hex 32)" \
  matrix-mcp
```

Put it behind HTTPS. Keep the `/data` volume: it holds the device keys, the crypto store and the search index.

### Homeservers with password login disabled

Set `MATRIX_ACCESS_TOKEN` instead of `MATRIX_PASSWORD`. The server checks that the token belongs to `MATRIX_USER_ID` and reads its device ID from Matrix. Use a new device dedicated to this MCP instance, with no published keys. Token mode keeps its own crypto store and index in `DATA_DIR/matrix-mcp-token.db`. If you switch from password mode, keep your previous database and provision a new device for token mode. Keep the token-mode data directory for later starts, and use a fresh data directory for a different account or device. A replacement token must belong to the same account and device. `DEVICE_NAME` only applies when the server logs in with a password.

Use a long-lived token or replace it before expiry and restart the MCP server. Leave `PUBLIC_URL` unset and use the separate `MCP_BEARER_TOKEN` to connect clients. The built-in OAuth consent screen uses Matrix password login.

## Connecting clients

**Claude Code**

```sh
claude mcp add --transport http -s user matrix https://matrix-mcp.example.com/mcp \
  --header "Authorization: Bearer $MATRIX_MCP_TOKEN"
```

**Codex** (`~/.codex/config.toml`)

```toml
[mcp_servers.matrix]
url = "https://matrix-mcp.example.com/mcp"
bearer_token_env_var = "MATRIX_MCP_TOKEN"
```

**claude.ai / Claude apps.** Set `PUBLIC_URL`, then go to Settings → Connectors → Add custom connector and enter `https://matrix-mcp.example.com/mcp`. Claude registers itself and opens the consent page, where you sign in with your Matrix username and password.

**ChatGPT.** Enable developer mode (Settings → Apps → Advanced settings), then create an app with the same URL and OAuth authentication.

Only the account the server runs as can sign in. The password is checked by logging in to the homeserver and immediately logging out again, so the homeserver's own rate limits apply. OAuth access tokens last an hour and refresh tokens 60 days (they rotate on use). Tokens are stored hashed.

## Development

```sh
CGO_ENABLED=1 go build -tags "goolm sqlite_fts5" .
CGO_ENABLED=1 go test -tags "goolm sqlite_fts5" ./...
```

The `goolm` tag selects the pure-Go Olm implementation; `sqlite_fts5` enables full-text search.

## Evals

`evals/` measures how well an agent answers real questions through the MCP. Each task runs through Claude Code headless (`claude -p`), connected only to this server and limited to read-only tools. Each trial's final `ANSWER:` line is graded against the expected answers. A task passes only if every trial passes (pass^k).

```sh
cp evals/tasks.example.json evals/tasks.local.json   # write questions about your own chats
MATRIX_MCP_URL=https://matrix-mcp.example.com/mcp MATRIX_MCP_TOKEN=... \
  go run ./evals -tasks evals/tasks.local.json -trials 3 -json evals/report.json
```

Files matching `*.local.json` are gitignored, so questions about your own chats stay on your machine. Use failing transcripts (`-v`) to improve tool descriptions.

## License

MIT
