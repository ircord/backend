# Encrypted chat handshakes over IRC

This document describes how IRCord servers exchange the handshake objects of
encrypted chats over IRC. It covers direct chats between people on different
IRCord servers and encrypted groups. The feature is experimental and stays
behind the owner's Encryption setting.

The encryption core's wire specification (ircord/e2ee, `docs/wire.md`) owns
the MLS objects. This document owns the lines and object headers that the
backend reads. Encrypted messages do not change: they stay `1.m` and `1.a`
values on a `PRIVMSG`.

## Lines

A handshake object travels as one or more `TAGMSG` lines:

```
@+ircord/e2ee=h.<kind>.<transfer>.<index>.<count>.<data> TAGMSG <target>
```

- `h` marks a handshake line. Phase 1 parsers ignore it.
- `kind` is one character. The table in [Objects](#objects) lists them.
- `transfer` is 16 random bytes in base64url without padding (22 characters).
  The sender chooses it. A reply uses the transfer of its request.
- `index` and `count` number the chunks of one object from 0. `count` is 1 to 32.
  Numbers have no leading zeros.
- `data` is base64url without padding: at most 2,826 bytes of the object.

A tag value is at most 3,804 characters. The encryption self-test proves that
the network carries tags of that length. One object is at most 90,432 bytes.

A `TAGMSG` has no body. Plain IRC clients and clients without the tag
capability receive nothing. Ergo does not keep these lines in history.

## Receiving

A receiver joins chunks by sender account, kind and transfer:

- It drops a line without an `account` tag, a `time` tag or a `msgid` tag.
- It never joins chunks from two accounts, even under one transfer.
- It drops a transfer that is not complete after 60 seconds.
- It ignores a transfer it completed in the last 10 minutes, except a Welcome:
  a Welcome sent again gets its receipt again.

The position of an object is the `time` and `msgid` of its last chunk. Every
member sees the same pair for a line.

## Objects

Each object starts with the version byte `2` and its kind character. Fields
follow in order. An unsigned integer is a uvarint. A string or byte string is a
uvarint length and the bytes. A boolean is the uvarint 0 or 1. A receiver
refuses an object with bytes left over.

| Kind | Object | Fields | Sent to |
|---|---|---|---|
| `q` | Directory request | account, claim | The account's nick |
| `d` | Directory answer | account, identity key, generation, device list, list counter, device count, then for each device: ID, certificate, key package, last resort | The requester, under the request's transfer |
| `c` | Commit | group, entry count, then for each entry: epoch, commit, time, message ID, sender; then more | The chat: the other person, or the group's channel |
| `w` | Welcome | group, epoch, chat kind (0 direct, 1 group), replaces, channel, Welcome | Each added account, up to four names a line |
| `k` | Receipt | group, declined | The Welcome's sender, under the Welcome's transfer |
| `l` | Log request | group, from epoch | One member |
| `j` | Rejoin request | group, device ID, certificate, key package | The chat |
| `n` | Notice | reason (0 devices changed, 1 identity changed) | Each joined chat |

A live commit has one entry with time 0, an empty message ID and an empty
sender: its position is the stamp on its own last line. The answer to a log
request lists entries with the position and sender recorded when they were
first seen, and sets more when the receiver should ask again from the next
epoch.

A group's channel is `#e2ee-` and the group's 16-byte ID in lowercase hex. A
group Welcome names that channel; a direct Welcome names none.

## Ordering

For each epoch, the candidates are all complete commits that name it. They sort
by time, then by message ID. Devices try them in that order and apply the first
one their core accepts.

A backend offers a candidate 2 seconds after its arrival. A sending backend
waits for the echo of its own last chunk and those 2 seconds. Then it answers
its client: accepted when no candidate for the epoch sorts before its own.

## Welcomes for the sender's own account

The IRC server returns a line that an account sends to itself only as the
sender's echo. A backend does not take its own echo as a Welcome. So a backend
does not send a Welcome to its own account over IRC. It stores the Welcome for
the account's devices when it queues the Welcome:

- A new chat's Welcome goes to every device of the creating account.
- A commit's Welcome goes to them when the commit adds the account itself.

No IRC server stamps this copy, so its position is the time the backend stored
it, with an empty message ID. Other accounts on the same backend get their
Welcome over IRC, with its receipt, like accounts on other servers.

## Retries and limits

- A Welcome stays in the sender's outbox until the receiver's server sends its
  receipt. The sender tries again after 1, 5 and 30 minutes, then every hour,
  for 30 days. It also tries again when the IRC server reports the recipient
  online (`MONITOR`).
- A directory or log request waits 8 seconds and is sent three times.
- One account may send 10 directory or log requests a minute to one account
  here. Beyond that, directory answers carry only last-resort key packages.
- Handshake lines go out behind the account's own lines: five at once, then two
  a second.
- Only the server that holds an account's identity answers requests for it.

## One account on two IRCord servers

A second server that has no keys for an account asks the account's other
sessions for its directory. When another server answers, the second server
reports that encryption is set up elsewhere and refuses a new identity there.
