-- +goose Up
-- Encrypted chats carried over IRC (ircord/issues#19, phase 2). Commits,
-- Welcomes, key packages and requests travel between IRCord servers as tagged
-- TAGMSG lines. Every blob is public key material or a client-made MLS object:
-- the server stores and orders them, and never holds a private key or reads a
-- message.

-- The account's signed list of its current device IDs, with a counter that only
-- goes up. Clients refuse a list older than the newest one they have seen.
CREATE TABLE IF NOT EXISTS e2ee_device_lists (
    owner TEXT PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,
    list BLOB NOT NULL,
    counter INTEGER NOT NULL,
    updated_ns INTEGER NOT NULL
);

-- An encrypted chat the account takes part in: a direct conversation with
-- someone anywhere on the network, or a group in its own invite-only channel.
-- state is invited until the account joins, then joined.
CREATE TABLE IF NOT EXISTS e2ee_chats (
    owner TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    group_id TEXT NOT NULL,
    kind TEXT NOT NULL,
    state TEXT NOT NULL,
    conversation TEXT NOT NULL DEFAULT '',
    target TEXT NOT NULL,
    peer TEXT NOT NULL DEFAULT '',
    inviter TEXT NOT NULL DEFAULT '',
    replaces TEXT NOT NULL DEFAULT '',
    epoch INTEGER NOT NULL DEFAULT 0,
    transfer TEXT NOT NULL DEFAULT '',
    created_ns INTEGER NOT NULL,
    updated_ns INTEGER NOT NULL,
    PRIMARY KEY (owner, group_id)
);
CREATE INDEX IF NOT EXISTS e2ee_chats_conversation ON e2ee_chats(owner, conversation);

-- Every complete commit the account's connection saw for a chat, with the
-- position the IRC server stamped on its last line. For each epoch the
-- candidate with the earliest position comes first.
CREATE TABLE IF NOT EXISTS e2ee_observed (
    owner TEXT NOT NULL,
    group_id TEXT NOT NULL,
    epoch INTEGER NOT NULL,
    at_ns INTEGER NOT NULL,
    msgid TEXT NOT NULL,
    sender TEXT NOT NULL,
    commit_data BLOB NOT NULL,
    arrived_ns INTEGER NOT NULL,
    PRIMARY KEY (owner, group_id, epoch, at_ns, msgid),
    FOREIGN KEY (owner, group_id) REFERENCES e2ee_chats(owner, group_id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS e2ee_observed_order ON e2ee_observed(owner, group_id, epoch, at_ns, msgid);

-- Welcomes that arrived over IRC, one row for each of the account's devices.
CREATE TABLE IF NOT EXISTS e2ee_chat_welcomes (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    owner TEXT NOT NULL,
    device TEXT NOT NULL,
    group_id TEXT NOT NULL,
    epoch INTEGER NOT NULL,
    kind TEXT NOT NULL,
    replaces TEXT NOT NULL DEFAULT '',
    target TEXT NOT NULL DEFAULT '',
    sender TEXT NOT NULL,
    welcome BLOB NOT NULL,
    at_ns INTEGER NOT NULL,
    msgid TEXT NOT NULL,
    created_ns INTEGER NOT NULL,
    FOREIGN KEY (owner, device) REFERENCES e2ee_devices(owner, device) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS e2ee_chat_welcomes_device ON e2ee_chat_welcomes(owner, device, id);
CREATE INDEX IF NOT EXISTS e2ee_chat_welcomes_group ON e2ee_chat_welcomes(owner, group_id);

-- Welcomes this account sent to others, kept until the receiving account's
-- server confirms them or they expire.
CREATE TABLE IF NOT EXISTS e2ee_outbox (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    owner TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    group_id TEXT NOT NULL,
    recipient TEXT NOT NULL,
    transfer TEXT NOT NULL,
    object BLOB NOT NULL,
    attempts INTEGER NOT NULL DEFAULT 0,
    next_ns INTEGER NOT NULL,
    created_ns INTEGER NOT NULL,
    state TEXT NOT NULL DEFAULT 'pending'
);
CREATE INDEX IF NOT EXISTS e2ee_outbox_due ON e2ee_outbox(owner, state, next_ns);
CREATE INDEX IF NOT EXISTS e2ee_outbox_transfer ON e2ee_outbox(owner, transfer, recipient);

-- Requests from devices that fell out of a chat and ask to be added again.
CREATE TABLE IF NOT EXISTS e2ee_rejoins (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    owner TEXT NOT NULL,
    group_id TEXT NOT NULL,
    account TEXT NOT NULL,
    device TEXT NOT NULL,
    certificate BLOB NOT NULL,
    key_package BLOB NOT NULL,
    created_ns INTEGER NOT NULL,
    FOREIGN KEY (owner, group_id) REFERENCES e2ee_chats(owner, group_id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS e2ee_rejoins_chat ON e2ee_rejoins(owner, group_id, id);
