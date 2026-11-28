# Backend changelog

## Unreleased

### New

- Experimental encrypted chats now reach people on other IRCord servers of the home network, and people can start encrypted groups. A group lives in its own invite-only channel; the person who created it and the admins they appoint decide who is in it. Every message in an encrypted group notifies unless the group is muted.
- If encryption for an account is already set up on another IRCord server, this server says so instead of setting up a second set of keys.

## v0.9.0 — 2026-11-28

### New

- Server operators can turn on experimental end-to-end encrypted direct messages between people on the server's home network. The server delivers these messages but can't read them, and keeps them out of search, link lists, the inbox text and notification text.
- Server owners can turn encrypted direct messages on or off in Administration, without a restart. Turning them off deletes nothing: devices with keys can still read earlier encrypted messages, but nobody can start new ones. Setting IRCORD_E2EE (off or experimental) locks the choice so owners can't change it.

## v0.8.2 — 2026-11-27

### New

- Servers keep an instance-wide high score table for the GPUI client's Ircoid game, seeded once on first start.

## v0.8.1 — 2026-11-25

### Fixed

- Removing an IRC network no longer stalls other accounts that are using IRCord.
- IRCord starts while pending account changes wait for the associated IRC server.
- IRCord stops retrying an unfinished signup or password change after three automatic attempts.

## v0.8.0 — 2026-11-24

### New

- Servers can keep the files people post on the IRC network's own file host.

## v0.7.8 — 2026-11-24

### Fixed

- Profile pictures from people on other IRCord servers now show instead of their initial.

## v0.7.7 — 2026-11-23

### Fixed

- Links written as `[text](url)` show in the link gallery and preview under their real address.

## v0.7.6 — 2026-11-21

### Fixed

- Server owners keep access to message deletion and bot marking when the app reconnects.

## v0.7.5 — 2026-11-21

### New

- Applications can read and save account settings, saved GIFs, emoji usage, and themes through the documented account API.

## v0.7.4 — 2026-11-21

- The backend checks and container publish run from the backend repository.

## v0.7.3 — 2026-11-21

### New

- Signed-in clients can read the backend version and release notes.
