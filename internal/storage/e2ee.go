package storage

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// The end-to-end encryption directory and group log (ircord/issues#19). The
// store keeps public key material and client-made MLS messages as opaque bytes.
// It checks ownership, bounds and ordering, never content.

// Bounds on what one account can keep in the directory and the group log.
const (
	E2EEMaxDevices      = 32
	E2EEMaxKeyPackages  = 100
	E2EEMaxGroupMembers = 32
	E2EEMaxPendingRelay = 32
	// E2EEMaxPendingWelcomes bounds the Welcomes waiting for one device.
	E2EEMaxPendingWelcomes = 100
	// E2EEMaxGroups bounds the groups one account has created.
	E2EEMaxGroups         = 1000
	E2EECommitHistory     = 1000
	E2EEApprovalLifetime  = 10 * time.Minute
	E2EEWelcomeLifetime   = 30 * 24 * time.Hour
	e2eeCommitPageMaximum = 100
)

var (
	// ErrE2EEConflict reports a write that disagrees with the current state, such
	// as a different identity key without a reset, or a device certificate made
	// for an identity that has since been reset.
	ErrE2EEConflict = errors.New("end-to-end encryption state changed")
	// ErrE2EELimit reports a full directory or relay.
	ErrE2EELimit = errors.New("end-to-end encryption limit reached")
	// ErrE2EEWelcomeTarget reports a Welcome for a device that isn't a
	// published device of a group member.
	ErrE2EEWelcomeTarget = errors.New("welcome for a device outside the group")
)

// E2EEEpochConflict is returned when a commit names an epoch the group has
// already left. The sender processes the commits since and tries again.
type E2EEEpochConflict struct{ Epoch int64 }

func (e E2EEEpochConflict) Error() string {
	return fmt.Sprintf("the group is at epoch %d", e.Epoch)
}

// E2EEGroupConflict is returned when a direct conversation already has another
// current group. The caller joins that group, or replaces it by naming it.
type E2EEGroupConflict struct{ Current string }

func (e E2EEGroupConflict) Error() string { return "the conversation has another current group" }

type E2EEIdentity struct {
	IdentityKey []byte
	Generation  int64
	UpdatedAt   time.Time
}

type E2EEDevice struct {
	ID          string
	Certificate []byte
	Label       string
	KeyPackages int
	LastResort  bool
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// E2EEClaim is one device's key package, taken so the device can be added to a
// group while it is offline.
type E2EEClaim struct {
	Device      string
	Certificate []byte
	KeyPackage  []byte
	LastResort  bool
}

type E2EEGroup struct {
	ID        string
	Epoch     int64
	GroupInfo []byte
	Members   []string
	CreatedBy string
	CreatedAt time.Time
	UpdatedAt time.Time
}

type E2EECommit struct {
	Epoch     int64
	Sender    string
	Commit    []byte
	CreatedAt time.Time
}

// E2EEWelcomeInput addresses a Welcome to one device of a member account.
type E2EEWelcomeInput struct {
	Owner   string
	Device  string
	Welcome []byte
}

type E2EEWelcome struct {
	ID        int64
	GroupID   string
	Epoch     int64
	Device    string
	Welcome   []byte
	CreatedAt time.Time
}

type E2EEApproval struct {
	ID         int64
	FromDevice string
	ToDevice   string
	Payload    []byte
	CreatedAt  time.Time
}

// E2EEConversation is an account's direct conversation bound to its pair.
type E2EEConversation struct {
	Peer  string
	Group string
}

func unixTime(ns int64) time.Time { return time.Unix(0, ns).UTC() }

// E2EEAccountNames maps account IDs to usernames, which are the accounts' names
// on the home network. Deleted accounts are left out.
func (s *Store) E2EEAccountNames(ids []string) (map[string]string, error) {
	out := map[string]string{}
	for _, id := range ids {
		if _, done := out[id]; done {
			continue
		}
		var name sql.NullString
		err := s.DB.QueryRow("SELECT username FROM accounts WHERE id=? AND deleted=0", id).Scan(&name)
		if errors.Is(err, sql.ErrNoRows) || err == nil && !name.Valid {
			continue
		}
		if err != nil {
			return nil, err
		}
		out[id] = name.String
	}
	return out, nil
}

// E2EEIdentity returns the account's identity public key, or ErrNotFound.
func (s *Store) E2EEIdentity(owner string) (E2EEIdentity, error) {
	var v E2EEIdentity
	var updated int64
	err := s.DB.QueryRow("SELECT identity_key,generation,updated_ns FROM e2ee_identities WHERE owner=?", owner).Scan(&v.IdentityKey, &v.Generation, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return v, ErrNotFound
	}
	v.UpdatedAt = unixTime(updated)
	return v, err
}

// PublishE2EEIdentity stores the account's first identity key. Publishing the
// same key again changes nothing. A different key needs reset: the reset replaces
// the identity, bumps its generation and removes every device, key package,
// waiting Welcome and approval message of the old identity.
func (s *Store) PublishE2EEIdentity(owner string, key []byte, reset bool) (E2EEIdentity, error) {
	tx, err := s.DB.Begin()
	if err != nil {
		return E2EEIdentity{}, err
	}
	defer tx.Rollback()
	now := time.Now().UnixNano()
	var current []byte
	var generation int64
	err = tx.QueryRow("SELECT identity_key,generation FROM e2ee_identities WHERE owner=?", owner).Scan(&current, &generation)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		generation = 1
		if _, err = tx.Exec("INSERT INTO e2ee_identities(owner,identity_key,generation,updated_ns) VALUES(?,?,?,?)", owner, key, generation, now); err != nil {
			return E2EEIdentity{}, err
		}
	case err != nil:
		return E2EEIdentity{}, err
	case bytes.Equal(current, key) && !reset:
		var updated int64
		if err = tx.QueryRow("SELECT updated_ns FROM e2ee_identities WHERE owner=?", owner).Scan(&updated); err != nil {
			return E2EEIdentity{}, err
		}
		return E2EEIdentity{IdentityKey: current, Generation: generation, UpdatedAt: unixTime(updated)}, nil
	case !reset:
		return E2EEIdentity{}, ErrE2EEConflict
	default:
		generation++
		if _, err = tx.Exec("UPDATE e2ee_identities SET identity_key=?,generation=?,updated_ns=? WHERE owner=?", key, generation, now, owner); err != nil {
			return E2EEIdentity{}, err
		}
		for _, table := range []string{"e2ee_devices", "e2ee_welcomes", "e2ee_approvals", "e2ee_device_lists"} {
			if _, err = tx.Exec("DELETE FROM "+table+" WHERE owner=?", owner); err != nil {
				return E2EEIdentity{}, err
			}
		}
	}
	if err = tx.Commit(); err != nil {
		return E2EEIdentity{}, err
	}
	return E2EEIdentity{IdentityKey: key, Generation: generation, UpdatedAt: unixTime(now)}, nil
}

// PutE2EEDevice stores a device certificate made under the identity generation
// the caller names, so a device approved before a reset cannot come back.
func (s *Store) PutE2EEDevice(owner, device string, generation int64, certificate, lastResort []byte, label string) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var current int64
	err = tx.QueryRow("SELECT generation FROM e2ee_identities WHERE owner=?", owner).Scan(&current)
	if errors.Is(err, sql.ErrNoRows) || err == nil && current != generation {
		return ErrE2EEConflict
	}
	if err != nil {
		return err
	}
	var exists bool
	var count int
	if err = tx.QueryRow("SELECT EXISTS(SELECT 1 FROM e2ee_devices WHERE owner=? AND device=?),(SELECT count(*) FROM e2ee_devices WHERE owner=?)", owner, device, owner).Scan(&exists, &count); err != nil {
		return err
	}
	if !exists && count >= E2EEMaxDevices {
		return ErrE2EELimit
	}
	now := time.Now().UnixNano()
	if _, err = tx.Exec(`INSERT INTO e2ee_devices(owner,device,certificate,last_resort,label,created_ns,updated_ns) VALUES(?,?,?,?,?,?,?)
		ON CONFLICT(owner,device) DO UPDATE SET certificate=excluded.certificate,last_resort=coalesce(excluded.last_resort,e2ee_devices.last_resort),label=excluded.label,updated_ns=excluded.updated_ns`,
		owner, device, certificate, nullBytes(lastResort), label, now, now); err != nil {
		return err
	}
	return tx.Commit()
}

func nullBytes(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return b
}

// DeleteE2EEDevice removes a device with its key packages and waiting Welcomes.
// Peers' clients then remove it from every group, which rotates the keys.
func (s *Store) DeleteE2EEDevice(owner, device string) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.Exec("DELETE FROM e2ee_devices WHERE owner=? AND device=?", owner, device)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	if _, err = tx.Exec("DELETE FROM e2ee_welcomes WHERE owner=? AND device=?", owner, device); err != nil {
		return err
	}
	if _, err = tx.Exec("DELETE FROM e2ee_approvals WHERE owner=? AND to_device=?", owner, device); err != nil {
		return err
	}
	return tx.Commit()
}

// E2EEDevices lists an account's devices, oldest first, with their key package stock.
func (s *Store) E2EEDevices(owner string) ([]E2EEDevice, error) {
	rows, err := s.DB.Query(`SELECT d.device,d.certificate,d.label,d.last_resort IS NOT NULL,d.created_ns,d.updated_ns,
		(SELECT count(*) FROM e2ee_key_packages k WHERE k.owner=d.owner AND k.device=d.device)
		FROM e2ee_devices d WHERE d.owner=? ORDER BY d.created_ns,d.device`, owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []E2EEDevice{}
	for rows.Next() {
		var v E2EEDevice
		var created, updated int64
		if err = rows.Scan(&v.ID, &v.Certificate, &v.Label, &v.LastResort, &created, &updated, &v.KeyPackages); err != nil {
			return nil, err
		}
		v.CreatedAt, v.UpdatedAt = unixTime(created), unixTime(updated)
		out = append(out, v)
	}
	return out, rows.Err()
}

// AddE2EEKeyPackages adds single-use key packages to one of the account's
// devices and returns the device's stock.
func (s *Store) AddE2EEKeyPackages(owner, device string, packages [][]byte) (int, error) {
	tx, err := s.DB.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var exists bool
	var stock int
	if err = tx.QueryRow("SELECT EXISTS(SELECT 1 FROM e2ee_devices WHERE owner=? AND device=?),(SELECT count(*) FROM e2ee_key_packages WHERE owner=? AND device=?)", owner, device, owner, device).Scan(&exists, &stock); err != nil {
		return 0, err
	}
	if !exists {
		return 0, ErrNotFound
	}
	if stock+len(packages) > E2EEMaxKeyPackages {
		return stock, ErrE2EELimit
	}
	for _, p := range packages {
		if _, err = tx.Exec("INSERT INTO e2ee_key_packages(owner,device,package) VALUES(?,?,?)", owner, device, p); err != nil {
			return 0, err
		}
	}
	if err = tx.Commit(); err != nil {
		return 0, err
	}
	return stock + len(packages), nil
}

// ClaimE2EEKeyPackages takes one key package for each of the account's devices:
// the oldest single-use package, or the last-resort package when the stock is
// empty. A device with neither is left out. The identity is read in the same
// transaction, so a concurrent reset can't pair new packages with an old key.
// An account without an identity is ErrNotFound.
func (s *Store) ClaimE2EEKeyPackages(owner string) (identity E2EEIdentity, _ []E2EEClaim, _ error) {
	tx, err := s.DB.Begin()
	if err != nil {
		return identity, nil, err
	}
	defer tx.Rollback()
	var updated int64
	err = tx.QueryRow("SELECT identity_key,generation,updated_ns FROM e2ee_identities WHERE owner=?", owner).Scan(&identity.IdentityKey, &identity.Generation, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return identity, nil, ErrNotFound
	}
	if err != nil {
		return identity, nil, err
	}
	identity.UpdatedAt = unixTime(updated)
	claims, err := claimPackages(tx, owner)
	if err != nil {
		return identity, nil, err
	}
	return identity, claims, tx.Commit()
}

func claimPackages(tx *sql.Tx, owner string) ([]E2EEClaim, error) {
	rows, err := tx.Query("SELECT device,certificate,last_resort FROM e2ee_devices WHERE owner=? ORDER BY created_ns,device", owner)
	if err != nil {
		return nil, err
	}
	devices := []E2EEClaim{}
	lastResort := map[string][]byte{}
	for rows.Next() {
		var v E2EEClaim
		var resort []byte
		if err = rows.Scan(&v.Device, &v.Certificate, &resort); err != nil {
			rows.Close()
			return nil, err
		}
		lastResort[v.Device] = resort
		devices = append(devices, v)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, err
	}
	out := []E2EEClaim{}
	for _, v := range devices {
		var id int64
		err = tx.QueryRow("SELECT id,package FROM e2ee_key_packages WHERE owner=? AND device=? ORDER BY id LIMIT 1", owner, v.Device).Scan(&id, &v.KeyPackage)
		switch {
		case err == nil:
			if _, err = tx.Exec("DELETE FROM e2ee_key_packages WHERE id=?", id); err != nil {
				return nil, err
			}
		case errors.Is(err, sql.ErrNoRows):
			if len(lastResort[v.Device]) == 0 {
				continue
			}
			v.KeyPackage, v.LastResort = lastResort[v.Device], true
		default:
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

type e2eeTx interface {
	Exec(string, ...any) (sql.Result, error)
	QueryRow(string, ...any) *sql.Row
}

// insertWelcomes queues Welcomes for published devices of member accounts. A
// Welcome for an account outside the group would hand its secrets to a
// stranger: refuse it. A device holds at most E2EEMaxPendingWelcomes.
func insertWelcomes(tx e2eeTx, group string, epoch int64, welcomes []E2EEWelcomeInput, now int64) error {
	if _, err := tx.Exec("DELETE FROM e2ee_welcomes WHERE created_ns<?", now-E2EEWelcomeLifetime.Nanoseconds()); err != nil {
		return err
	}
	for _, w := range welcomes {
		var member, device bool
		var pending int
		if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM e2ee_group_members WHERE group_id=? AND owner=?),
			EXISTS(SELECT 1 FROM e2ee_devices WHERE owner=? AND device=?),
			(SELECT count(*) FROM e2ee_welcomes WHERE owner=? AND device=?)`, group, w.Owner, w.Owner, w.Device, w.Owner, w.Device).Scan(&member, &device, &pending); err != nil {
			return err
		}
		if !member || !device {
			return ErrE2EEWelcomeTarget
		}
		if pending >= E2EEMaxPendingWelcomes {
			return ErrE2EELimit
		}
		if _, err := tx.Exec("INSERT INTO e2ee_welcomes(group_id,epoch,owner,device,welcome,created_ns) VALUES(?,?,?,?,?,?)", group, epoch, w.Owner, w.Device, w.Welcome, now); err != nil {
			return err
		}
	}
	return nil
}

// CreateE2EEGroup registers a new group at the epoch its creator reached, with
// the member accounts allowed to use its log, and queues the creator's Welcomes.
// The creator must be a member. A group ID is never reused.
func (s *Store) CreateE2EEGroup(g E2EEGroup, welcomes []E2EEWelcomeInput) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var created int
	if err = tx.QueryRow("SELECT count(*) FROM e2ee_groups WHERE created_by=?", g.CreatedBy).Scan(&created); err != nil {
		return err
	}
	if created >= E2EEMaxGroups {
		return ErrE2EELimit
	}
	now := time.Now().UnixNano()
	result, err := tx.Exec("INSERT INTO e2ee_groups(id,epoch,group_info,created_by,created_ns,updated_ns) VALUES(?,?,?,?,?,?) ON CONFLICT(id) DO NOTHING", g.ID, g.Epoch, nullBytes(g.GroupInfo), g.CreatedBy, now, now)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return ErrE2EEConflict
	}
	for _, member := range g.Members {
		if _, err = tx.Exec("INSERT INTO e2ee_group_members(group_id,owner) VALUES(?,?) ON CONFLICT DO NOTHING", g.ID, member); err != nil {
			return err
		}
	}
	if err = insertWelcomes(tx, g.ID, g.Epoch, welcomes, now); err != nil {
		return err
	}
	return tx.Commit()
}

func groupMember(q e2eeTx, owner, group string) (bool, error) {
	var member bool
	err := q.QueryRow("SELECT EXISTS(SELECT 1 FROM e2ee_group_members WHERE group_id=? AND owner=?)", group, owner).Scan(&member)
	return member, err
}

// E2EEGroup returns a group the account is a member of. A group the account
// does not belong to is reported as not found, like one that doesn't exist.
func (s *Store) E2EEGroup(owner, group string) (E2EEGroup, error) {
	member, err := groupMember(s.DB, owner, group)
	if err != nil {
		return E2EEGroup{}, err
	}
	if !member {
		return E2EEGroup{}, ErrNotFound
	}
	g := E2EEGroup{ID: group}
	var created, updated int64
	if err = s.DB.QueryRow("SELECT epoch,group_info,created_by,created_ns,updated_ns FROM e2ee_groups WHERE id=?", group).Scan(&g.Epoch, &g.GroupInfo, &g.CreatedBy, &created, &updated); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			err = ErrNotFound
		}
		return E2EEGroup{}, err
	}
	g.CreatedAt, g.UpdatedAt = unixTime(created), unixTime(updated)
	rows, err := s.DB.Query("SELECT owner FROM e2ee_group_members WHERE group_id=? ORDER BY owner", group)
	if err != nil {
		return E2EEGroup{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var member string
		if err = rows.Scan(&member); err != nil {
			return E2EEGroup{}, err
		}
		g.Members = append(g.Members, member)
	}
	return g, rows.Err()
}

// AppendE2EECommit accepts a commit only if it names the group's current epoch.
// SQLite serializes the compare-and-swap, so of two commits racing for one epoch
// exactly one wins; the other gets E2EEEpochConflict with the epoch to catch up to.
func (s *Store) AppendE2EECommit(owner, group string, epoch int64, commit, groupInfo []byte, welcomes []E2EEWelcomeInput) (int64, error) {
	tx, err := s.DB.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	member, err := groupMember(tx, owner, group)
	if err != nil {
		return 0, err
	}
	if !member {
		return 0, ErrNotFound
	}
	now := time.Now().UnixNano()
	result, err := tx.Exec("UPDATE e2ee_groups SET epoch=epoch+1,group_info=coalesce(?,group_info),updated_ns=? WHERE id=? AND epoch=?", nullBytes(groupInfo), now, group, epoch)
	if err != nil {
		return 0, err
	}
	if n, _ := result.RowsAffected(); n == 0 {
		var current int64
		if err = tx.QueryRow("SELECT epoch FROM e2ee_groups WHERE id=?", group).Scan(&current); err != nil {
			return 0, err
		}
		return 0, E2EEEpochConflict{Epoch: current}
	}
	if _, err = tx.Exec("INSERT INTO e2ee_commits(group_id,epoch,sender,commit_data,created_ns) VALUES(?,?,?,?,?)", group, epoch, owner, commit, now); err != nil {
		return 0, err
	}
	if _, err = tx.Exec("DELETE FROM e2ee_commits WHERE group_id=? AND epoch<?", group, epoch+1-E2EECommitHistory); err != nil {
		return 0, err
	}
	if err = insertWelcomes(tx, group, epoch+1, welcomes, now); err != nil {
		return 0, err
	}
	if err = tx.Commit(); err != nil {
		return 0, err
	}
	return epoch + 1, nil
}

// E2EECommits returns the group's commits from an epoch on, in order. A device
// at epoch N applies the commit made at N first.
func (s *Store) E2EECommits(owner, group string, from int64, limit int) ([]E2EECommit, error) {
	member, err := groupMember(s.DB, owner, group)
	if err != nil {
		return nil, err
	}
	if !member {
		return nil, ErrNotFound
	}
	if limit <= 0 || limit > e2eeCommitPageMaximum {
		limit = e2eeCommitPageMaximum
	}
	rows, err := s.DB.Query("SELECT epoch,sender,commit_data,created_ns FROM e2ee_commits WHERE group_id=? AND epoch>=? ORDER BY epoch LIMIT ?", group, from, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []E2EECommit{}
	for rows.Next() {
		var v E2EECommit
		var at int64
		if err = rows.Scan(&v.Epoch, &v.Sender, &v.Commit, &at); err != nil {
			return nil, err
		}
		v.CreatedAt = unixTime(at)
		out = append(out, v)
	}
	return out, rows.Err()
}

// E2EEWelcomes lists the Welcomes waiting for one of the account's devices.
func (s *Store) E2EEWelcomes(owner, device string) ([]E2EEWelcome, error) {
	rows, err := s.DB.Query("SELECT id,group_id,epoch,device,welcome,created_ns FROM e2ee_welcomes WHERE owner=? AND device=? AND created_ns>=? ORDER BY id LIMIT 100", owner, device, time.Now().Add(-E2EEWelcomeLifetime).UnixNano())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []E2EEWelcome{}
	for rows.Next() {
		var v E2EEWelcome
		var at int64
		if err = rows.Scan(&v.ID, &v.GroupID, &v.Epoch, &v.Device, &v.Welcome, &at); err != nil {
			return nil, err
		}
		v.CreatedAt = unixTime(at)
		out = append(out, v)
	}
	return out, rows.Err()
}

// DeleteE2EEWelcome removes a Welcome the device has processed.
func (s *Store) DeleteE2EEWelcome(owner string, id int64) error {
	result, err := s.DB.Exec("DELETE FROM e2ee_welcomes WHERE owner=? AND id=?", owner, id)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func pairKey(a, b string) (string, string) {
	if b < a {
		return b, a
	}
	return a, b
}

// BindE2EEConversation makes group the current group of the direct conversation
// between owner and peer, and binds owner's own conversation to that pair, so
// the backend refuses plaintext into it. The group must have exactly these two
// accounts as members. There is one current group per pair: binding the current
// group again changes nothing; a different group needs replaces to name the
// current one (compare-and-swap), else E2EEGroupConflict names the current group.
func (s *Store) BindE2EEConversation(owner, conversation, peer, group, replaces string) error {
	if owner == peer {
		return ErrNotFound
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var members, matched int
	if err = tx.QueryRow("SELECT count(*),count(CASE WHEN owner IN (?,?) THEN 1 END) FROM e2ee_group_members WHERE group_id=?", owner, peer, group).Scan(&members, &matched); err != nil {
		return err
	}
	if members != 2 || matched != 2 {
		return ErrNotFound
	}
	a, b := pairKey(owner, peer)
	var current sql.NullString
	err = tx.QueryRow("SELECT group_id FROM e2ee_pairs WHERE owner_a=? AND owner_b=?", a, b).Scan(&current)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	now := time.Now().UnixNano()
	switch {
	case current.String == group:
	case current.String == replaces:
		if _, err = tx.Exec(`INSERT INTO e2ee_pairs(owner_a,owner_b,group_id,updated_by,updated_ns) VALUES(?,?,?,?,?)
			ON CONFLICT(owner_a,owner_b) DO UPDATE SET group_id=excluded.group_id,updated_by=excluded.updated_by,updated_ns=excluded.updated_ns`, a, b, group, owner, now); err != nil {
			return err
		}
	default:
		return E2EEGroupConflict{Current: current.String}
	}
	if _, err = tx.Exec(`INSERT INTO e2ee_conversations(owner,conversation,peer,created_ns) VALUES(?,?,?,?)
		ON CONFLICT(owner,conversation) DO UPDATE SET peer=excluded.peer`, owner, conversation, peer, now); err != nil {
		return err
	}
	return tx.Commit()
}

// TurnOffE2EE ends encryption in the direct conversation for both accounts, if
// group is still its current group. A stale request can't end a replacement.
func (s *Store) TurnOffE2EE(owner, conversation, group string) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var peer string
	err = tx.QueryRow("SELECT peer FROM e2ee_conversations WHERE owner=? AND conversation=?", owner, conversation).Scan(&peer)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	a, b := pairKey(owner, peer)
	var current sql.NullString
	err = tx.QueryRow("SELECT group_id FROM e2ee_pairs WHERE owner_a=? AND owner_b=?", a, b).Scan(&current)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if !current.Valid {
		return nil
	}
	if current.String != group {
		return E2EEGroupConflict{Current: current.String}
	}
	if _, err = tx.Exec("UPDATE e2ee_pairs SET group_id=NULL,updated_by=?,updated_ns=? WHERE owner_a=? AND owner_b=?", owner, time.Now().UnixNano(), a, b); err != nil {
		return err
	}
	// Both sides' bindings go with it: turning encryption on again takes each
	// person's own client, so one side can't lock the other's conversation alone.
	if _, err = tx.Exec("DELETE FROM e2ee_conversations WHERE owner=? AND peer=? OR owner=? AND peer=?", owner, peer, peer, owner); err != nil {
		return err
	}
	return tx.Commit()
}

// E2EEConversationState returns the pair an account's conversation is bound to
// and its current group, empty while encryption is off. ErrNotFound means the
// conversation was never bound.
func (s *Store) E2EEConversationState(owner, conversation string) (E2EEConversation, error) {
	var v E2EEConversation
	var group sql.NullString
	// Every send checks this; the read-only handle keeps it from waiting behind
	// the writer. WAL shows it every committed binding.
	reader := s.DB
	if s.read != nil {
		reader = s.read
	}
	err := reader.QueryRow(`SELECT c.peer,p.group_id FROM e2ee_conversations c
		LEFT JOIN e2ee_pairs p ON p.owner_a=min(c.owner,c.peer) AND p.owner_b=max(c.owner,c.peer)
		WHERE c.owner=? AND c.conversation=?`, owner, conversation).Scan(&v.Peer, &group)
	if errors.Is(err, sql.ErrNoRows) {
		return v, ErrNotFound
	}
	v.Group = group.String
	return v, err
}

// E2EEConversationEncrypted reports whether plaintext must be refused in the
// account's conversation.
func (s *Store) E2EEConversationEncrypted(owner, conversation string) (bool, error) {
	v, err := s.E2EEConversationState(owner, conversation)
	if err == nil && v.Group != "" {
		return true, nil
	}
	if err != nil && !errors.Is(err, ErrNotFound) {
		return false, err
	}
	// An encrypted chat carried over IRC binds the conversation once joined.
	_, err = s.E2EEChatForConversation(owner, conversation)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

// AddE2EEApproval relays one message between two devices of an account. Relay
// messages expire after E2EEApprovalLifetime and are removed once read.
func (s *Store) AddE2EEApproval(owner, from, to string, payload []byte) (int64, error) {
	tx, err := s.DB.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	now := time.Now()
	if _, err = tx.Exec("DELETE FROM e2ee_approvals WHERE created_ns<?", now.Add(-E2EEApprovalLifetime).UnixNano()); err != nil {
		return 0, err
	}
	var pending int
	if err = tx.QueryRow("SELECT count(*) FROM e2ee_approvals WHERE owner=?", owner).Scan(&pending); err != nil {
		return 0, err
	}
	if pending >= E2EEMaxPendingRelay {
		return 0, ErrE2EELimit
	}
	result, err := tx.Exec("INSERT INTO e2ee_approvals(owner,from_device,to_device,payload,created_ns) VALUES(?,?,?,?,?)", owner, from, to, payload, now.UnixNano())
	if err != nil {
		return 0, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

// E2EEApprovals lists unexpired relay messages for one device of the account.
func (s *Store) E2EEApprovals(owner, device string) ([]E2EEApproval, error) {
	rows, err := s.DB.Query("SELECT id,from_device,to_device,payload,created_ns FROM e2ee_approvals WHERE owner=? AND to_device=? AND created_ns>=? ORDER BY id", owner, device, time.Now().Add(-E2EEApprovalLifetime).UnixNano())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []E2EEApproval{}
	for rows.Next() {
		var v E2EEApproval
		var at int64
		if err = rows.Scan(&v.ID, &v.FromDevice, &v.ToDevice, &v.Payload, &at); err != nil {
			return nil, err
		}
		v.CreatedAt = unixTime(at)
		out = append(out, v)
	}
	return out, rows.Err()
}

// DeleteE2EEApproval removes a relay message its device has read.
func (s *Store) DeleteE2EEApproval(owner string, id int64) error {
	result, err := s.DB.Exec("DELETE FROM e2ee_approvals WHERE owner=? AND id=?", owner, id)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
