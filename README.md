# mmdms

Small, dependency-free Mattermost CLI: read, post, search, threads and mentions from the terminal.
Output is plain text that's easy for scripts and coding agents to read; add `--json` for structured
output. It acts as one regular user account through the Mattermost REST API v4. No bot, personal
access token or admin rights needed.

macOS only for now: the password comes from the Keychain.

## Install

```sh
go install github.com/ndemeshchenko/mmdms@latest
```

## Setup

```sh
export MM_URL=https://chat.example.com
export MM_USER=alice

# store the password in the Keychain (service = server host, account = username)
security add-generic-password -U -s chat.example.com -a alice -w

mmdms whoami
```

`mmdms` logs in with that password and caches the session token in
`~/.config/mmdms/<host>_<user>.token` (mode 0600). When the session expires, it logs in again
automatically.

## Usage

```
mmdms channels [--unread]                     my channels with unread / mention counts
mmdms read <channel|@user> [-n 30] [--since 2h]
mmdms thread <post-id>
mmdms post <channel|@user> <text|-> [--reply <post-id>] [--file <path>]...
mmdms search <terms...> [-n 20]               Mattermost syntax: from:x in:y after:2026-10-01 "exact"
mmdms mentions [-n 20]                        posts that mention me
mmdms react <post-id> <emoji>
mmdms whoami
```

`--json` works with `channels`, `read`, `thread`, `search` and `mentions`. A message of `-` is read
from stdin. Channels are names (`town-square`, `~town-square`) or IDs; `@user` is the direct-message
channel with that user. The first team the user belongs to is used.

## License

MIT
