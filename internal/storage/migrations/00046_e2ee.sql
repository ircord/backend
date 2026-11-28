-- +goose Up
-- End-to-end encryption between IRCord clients (ircord/issues#19). Every blob
-- here is public key material or an MLS message the clients made: the server
-- stores and orders them, and never holds a private key or a decrypted message.

-- An account's identity public key. A reset replaces it and bumps the generation.
CREATE TABLE IF NOT EXISTS e2ee_identities (
    owner TEXT PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,
    identity_key BLOB NOT NULL,
    generation INTEGER NOT NULL,
    updated_ns INTEGER NOT NULL
);

-- A device: its certificate, signed by the identity key, and an optional
-- reusable last-resort key package.
CREATE TABLE IF NOT EXISTS e2ee_devices (
    owner TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    device TEXT NOT NULL,
    certificate BLOB NOT NULL,
    last_resort BLOB,
    label TEXT NOT NULL DEFAULT '',
    created_ns INTEGER NOT NULL,
    updated_ns INTEGER NOT NULL,
    PRIMARY KEY (owner, device)
);

-- Single-use key packages; claiming one removes it.
CREATE TABLE IF NOT EXISTS e2ee_key_packages (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    owner TEXT NOT NULL,
    device TEXT NOT NULL,
    package BLOB NOT NULL,
    FOREIGN KEY (owner, device) REFERENCES e2ee_devices(owner, device) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS e2ee_key_packages_device ON e2ee_key_packages(owner, device, id);

-- An MLS group and its current epoch. Only member accounts can read or extend it.
CREATE TABLE IF NOT EXISTS e2ee_groups (
    id TEXT PRIMARY KEY,
    epoch INTEGER NOT NULL,
    group_info BLOB,
    created_by TEXT NOT NULL,
    created_ns INTEGER NOT NULL,
    updated_ns INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS e2ee_group_members (
    group_id TEXT NOT NULL REFERENCES e2ee_groups(id) ON DELETE CASCADE,
    owner TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    PRIMARY KEY (group_id, owner)
);
CREATE INDEX IF NOT EXISTS e2ee_group_members_owner ON e2ee_group_members(owner);
-- A group whose last member account is gone is removed with its log and Welcomes.
-- +goose StatementBegin
CREATE TRIGGER IF NOT EXISTS e2ee_group_emptied AFTER DELETE ON e2ee_group_members
WHEN NOT EXISTS (SELECT 1 FROM e2ee_group_members WHERE group_id = old.group_id)
BEGIN
    DELETE FROM e2ee_groups WHERE id = old.group_id;
END;
-- +goose StatementEnd

-- The ordered group log: one commit per epoch, accepted by compare-and-swap.
CREATE TABLE IF NOT EXISTS e2ee_commits (
    group_id TEXT NOT NULL REFERENCES e2ee_groups(id) ON DELETE CASCADE,
    epoch INTEGER NOT NULL,
    sender TEXT NOT NULL,
    commit_data BLOB NOT NULL,
    created_ns INTEGER NOT NULL,
    PRIMARY KEY (group_id, epoch)
);

-- Welcomes waiting for one device each.
CREATE TABLE IF NOT EXISTS e2ee_welcomes (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    group_id TEXT NOT NULL REFERENCES e2ee_groups(id) ON DELETE CASCADE,
    epoch INTEGER NOT NULL,
    owner TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    device TEXT NOT NULL,
    welcome BLOB NOT NULL,
    created_ns INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS e2ee_welcomes_device ON e2ee_welcomes(owner, device, id);
CREATE INDEX IF NOT EXISTS e2ee_welcomes_group ON e2ee_welcomes(group_id);
CREATE INDEX IF NOT EXISTS e2ee_welcomes_created ON e2ee_welcomes(created_ns);
CREATE INDEX IF NOT EXISTS e2ee_groups_creator ON e2ee_groups(created_by);

-- The current group of the direct conversation between two accounts, shared by
-- both. owner_a sorts before owner_b. group_id is NULL while encryption is off.
CREATE TABLE IF NOT EXISTS e2ee_pairs (
    owner_a TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    owner_b TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    group_id TEXT,
    updated_by TEXT NOT NULL,
    updated_ns INTEGER NOT NULL,
    PRIMARY KEY (owner_a, owner_b)
);
CREATE INDEX IF NOT EXISTS e2ee_pairs_owner_b ON e2ee_pairs(owner_b);

-- Each account's own direct conversation with a peer, bound to their pair.
CREATE TABLE IF NOT EXISTS e2ee_conversations (
    owner TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    conversation TEXT NOT NULL,
    peer TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    created_ns INTEGER NOT NULL,
    PRIMARY KEY (owner, conversation)
);
CREATE INDEX IF NOT EXISTS e2ee_conversations_peer ON e2ee_conversations(peer, owner);

-- Short-lived messages between two devices of one account during approval.
CREATE TABLE IF NOT EXISTS e2ee_approvals (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    owner TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    from_device TEXT NOT NULL,
    to_device TEXT NOT NULL,
    payload BLOB NOT NULL,
    created_ns INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS e2ee_approvals_device ON e2ee_approvals(owner, to_device, id);
CREATE INDEX IF NOT EXISTS e2ee_approvals_created ON e2ee_approvals(created_ns);
