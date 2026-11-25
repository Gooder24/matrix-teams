# matrix-teams

A Matrix ↔ Microsoft Teams puppeting bridge, built on [mautrix-go](https://github.com/mautrix/go)'s
bridgev2 framework. It bridges your Teams **chats** (one-to-one, group and meeting chats) through
Microsoft Graph, signed in with your own work or school account.

It is not part of the mautrix project. Matrix room: none yet; issues go to this repository.

**Status:** early. Everything listed in [ROADMAP.md](ROADMAP.md) is covered by an end-to-end test
against a fake Microsoft Graph and a throwaway homeserver; it has not yet run against a real tenant.

## How it works

* **Sign-in** uses Microsoft's own public client *Microsoft Graph Command Line Tools*, so no app
  registration is needed. You open a link, sign in, and paste the `http://localhost/?code=…` address
  the browser ends up on. If your organization blocks that client, register a public client app in
  Entra ID (redirect URI `http://localhost`, delegated permissions below) and set `network.client_id`.
* **Permissions** (delegated): `Chat.ReadWrite` (required), `User.Read`, `User.ReadBasic.All` (names,
  photos, directory search), `Files.ReadWrite.All` (sending files through OneDrive, as Teams does).
  Some organizations require an administrator to approve them first; the bridge says so when that happens.
* **Polling:** Graph only offers push for chats to registered apps with a public HTTPS endpoint, so the
  bridge polls `/me/chats` every `poll_interval` (20 s by default) and reads only the chats whose newest
  message moved, plus a few recently active ones for edits, reactions and deletions. Each read asks only
  for messages modified since the last one.
* **History:** a chat seen for the first time gets its last `initial_messages` messages from the last
  `initial_days` days. Older chats get a room when someone writes in them again.
* **Read receipts** go to Teams only when you have read the newest message from someone else and every
  recent message is already bridged, because Teams keeps one read marker per chat.

## Setup

You need Go 1.26 or newer and a Matrix homeserver that supports application services.

```sh
go build -o matrix-teams ./cmd/matrix-teams
./matrix-teams -e                 # writes config.yaml from the example
$EDITOR config.yaml               # homeserver, database, permissions; the network section is optional
./matrix-teams -g                 # writes registration.yaml; add it to your homeserver
./matrix-teams
```

Then send `login` to the bridge bot (`@teamsbot:your.server`) and follow its instructions. Clients that
use the bridgev2 provisioning API (`/_matrix/provision/v3/login/start/browser`) can do the same.

### With Beeper

Like any bridgev2 bridge, it can run on your own machine and connect to your Beeper account with
[bbctl](https://github.com/beeper/bridge-manager):

```sh
bbctl login
bbctl config --type bridgev2 -o config.yaml sh-teams
./matrix-teams -c config.yaml
```

bbctl writes everything except the `network` section; the bridge adds that section with its defaults
when it first starts. The ghosts are `@sh-teams_…:beeper.local` and the bot `@sh-teamsbot:beeper.local`.
This path has not been tried yet; reports are welcome.

## Configuration

The `network` section of `config.yaml`:

| Key | Default | Meaning |
|---|---|---|
| `graph_url` | `https://graph.microsoft.com/v1.0` | Graph API base. Change only for testing. |
| `login_url` | `https://login.microsoftonline.com` | Identity platform. Change only for testing. |
| `client_id` | Graph Command Line Tools | Public client used for sign-in. |
| `tenant` | `organizations` | Any work or school account, or one tenant ID. |
| `poll_interval` | `20s` | How often to check for new messages. |
| `initial_messages`, `initial_days` | `20`, `14` | History of a chat seen for the first time. |
| `max_file_size` | 25 MiB | Larger files are sent as a link. |

Personal Microsoft accounts are refused: Graph does not expose their Teams chats.

## Tests

```sh
go test ./...                                         # unit tests; the end-to-end test is skipped
TUWUNEL=/path/to/tuwunel go test ./tests/e2e -v       # bridge + throwaway Tuwunel + fake Graph
```

The end-to-end test starts the bridge as its own process between a temporary
[Tuwunel](https://github.com/matrix-construct/tuwunel) homeserver and `internal/fakegraph`, signs in,
and checks every feature in both directions. Nothing reaches Microsoft or any other server.

Measured with `E2E_MEASURE=5m` (signed in, polling every 20 s): see the numbers in the commit that
added the measurement.

## License

AGPL-3.0-or-later, like the mautrix bridges.
