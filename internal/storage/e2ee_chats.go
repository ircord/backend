package storage

import (
	"bytes"
	"database/sql"
	"errors"
	"time"
)

// Encrypted chats carried over IRC (ircord/issues#19, phase 2). Commits and
// Welcomes travel between IRCord servers as tagged IRC lines; this store keeps
// what the account's own connection saw, the Welcomes it still has to deliver,
// and the chats it takes part in. Every blob stays opaque.

const (
	E2EEChatDirect  = "direct"
	E2EEChatGroup   = "group"
	E2EEChatInvited = "invited"
	E2EEChatJoined  = "joined"

	// E2EEMaxChats bounds the encrypted chats of one account, invitations included.
	E2EEMaxChats = 1000
	// E2EEMaxRejoins bounds the waiting rejoin requests of one chat.
	E2EEMaxRejoins = 32
	// E2EEMaxOutbox bounds the Welcomes one account is still delivering.
	E2EEMaxOutbox      = 2000
	E2EERejoinLifetime = 7 * 24 * time.Hour
)

// Outbox states.
const (
	E2EEOutboxPending   = "pending"
	E2EEOutboxDelivered = "delivered"
	E2EEOutboxDeclined  = "declined"
)

// E2EEChat is one encrypted chat of an account. Target is where its commits go
// on IRC: the other person's nick in a direct chat, the channel of a group.
type E2EEChat struct {
	Group        string
	Kind         string
	State        string
	Conversation string
	Target       string
	Peer         string
	Inviter      string
	Replaces     string
	Epoch        int64
	// Transfer is the handshake transfer of the Welcome that invited the
	// account; a decline answers under it.
	Transfer  string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// E2EEObserved is a complete commit as the account's connection saw it. At and
// MsgID are the IRC server's time and message ID on its last line.
type E2EEObserved struct {
	Epoch   int64
	At      int64
	MsgID   string
	Sender  string
	Commit  []byte
	Arrived time.Time
}

// Before orders candidates: earlier server time first, then message ID.
func (v E2EEObserved) Before(o E2EEObserved) bool {
	if v.At != o.At {
		return v.At < o.At
	}
	return v.MsgID < o.MsgID
}

type E2EEChatWelcome struct {
	ID        int64
	Device    string
	Group     string
	Epoch     int64
	Kind      string
	Replaces  string
	Target    string
	Sender    string
	Welcome   []byte
	At        int64
	MsgID     string
	CreatedAt time.Time
}

type E2EEOutboxItem struct {
	ID        int64
	Group     string
	Recipient string
	Transfer  string
	Object    []byte
	Attempts  int
	State     string
	CreatedAt time.Time
}

type E2EERejoin struct {
	ID          int64
	Account     string
	Device      string
	Certificate []byte
	KeyPackage  []byte
	CreatedAt   time.Time
}

func scanChat(row interface{ Scan(...any) error }) (E2EEChat, error) {
	var v E2EEChat
	var created, updated int64
	err := row.Scan(&v.Group, &v.Kind, &v.State, &v.Conversation, &v.Target, &v.Peer, &v.Inviter, &v.Replaces, &v.Epoch, &v.Transfer, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return v, ErrNotFound
	}
	v.CreatedAt, v.UpdatedAt = unixTime(created), unixTime(updated)
	return v, err
}

const chatColumns = "group_id,kind,state,conversation,target,peer,inviter,replaces,epoch,transfer,created_ns,updated_ns"

// PutE2EEChat adds or updates one of the account's encrypted chats.
func (s *Store) PutE2EEChat(owner string, v E2EEChat) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var exists bool
	var count int
	if err = tx.QueryRow("SELECT EXISTS(SELECT 1 FROM e2ee_chats WHERE owner=? AND group_id=?),(SELECT count(*) FROM e2ee_chats WHERE owner=?)", owner, v.Group, owner).Scan(&exists, &count); err != nil {
		return err
	}
	if !exists && count >= E2EEMaxChats {
		return ErrE2EELimit
	}
	now := time.Now().UnixNano()
	if _, err = tx.Exec(`INSERT INTO e2ee_chats(owner,`+chatColumns+`) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(owner,group_id) DO UPDATE SET kind=excluded.kind,state=excluded.state,conversation=excluded.conversation,target=excluded.target,
		peer=excluded.peer,inviter=excluded.inviter,replaces=excluded.replaces,epoch=excluded.epoch,transfer=excluded.transfer,updated_ns=excluded.updated_ns`,
		owner, v.Group, v.Kind, v.State, v.Conversation, v.Target, v.Peer, v.Inviter, v.Replaces, v.Epoch, v.Transfer, now, now); err != nil {
		return err
	}
	return tx.Commit()
}

// E2EEChat returns one of the account's encrypted chats, or ErrNotFound.
func (s *Store) E2EEChat(owner, group string) (E2EEChat, error) {
	return scanChat(s.DB.QueryRow("SELECT "+chatColumns+" FROM e2ee_chats WHERE owner=? AND group_id=?", owner, group))
}

// E2EEChats lists the account's encrypted chats, oldest first.
func (s *Store) E2EEChats(owner string) ([]E2EEChat, error) {
	rows, err := s.DB.Query("SELECT "+chatColumns+" FROM e2ee_chats WHERE owner=? ORDER BY created_ns,group_id", owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []E2EEChat{}
	for rows.Next() {
		v, err := scanChat(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// E2EEChatForConversation returns the joined chat bound to the account's
// conversation, or ErrNotFound.
func (s *Store) E2EEChatForConversation(owner, conversation string) (E2EEChat, error) {
	reader := s.DB
	if s.read != nil {
		reader = s.read
	}
	return scanChat(reader.QueryRow("SELECT "+chatColumns+" FROM e2ee_chats WHERE owner=? AND conversation=? AND state=? ORDER BY updated_ns DESC LIMIT 1", owner, conversation, E2EEChatJoined))
}

// DeleteE2EEChat forgets an encrypted chat with its observed commits, rejoin
// requests and waiting Welcomes.
func (s *Store) DeleteE2EEChat(owner, group string) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.Exec("DELETE FROM e2ee_chats WHERE owner=? AND group_id=?", owner, group)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	if _, err = tx.Exec("DELETE FROM e2ee_chat_welcomes WHERE owner=? AND group_id=?", owner, group); err != nil {
		return err
	}
	return tx.Commit()
}

// ObserveE2EECommit records a commit the account's connection saw for one of
// its chats. A copy already recorded changes nothing. The chat keeps its last
// E2EECommitHistory commits. It reports whether the commit was new.
func (s *Store) ObserveE2EECommit(owner, group string, v E2EEObserved) (bool, error) {
	tx, err := s.DB.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var known bool
	if err = tx.QueryRow("SELECT EXISTS(SELECT 1 FROM e2ee_chats WHERE owner=? AND group_id=?)", owner, group).Scan(&known); err != nil {
		return false, err
	}
	if !known {
		return false, ErrNotFound
	}
	arrived := v.Arrived
	if arrived.IsZero() {
		arrived = time.Now()
	}
	result, err := tx.Exec("INSERT OR IGNORE INTO e2ee_observed(owner,group_id,epoch,at_ns,msgid,sender,commit_data,arrived_ns) VALUES(?,?,?,?,?,?,?,?)",
		owner, group, v.Epoch, v.At, v.MsgID, v.Sender, v.Commit, arrived.UnixNano())
	if err != nil {
		return false, err
	}
	added, _ := result.RowsAffected()
	if added == 0 {
		return false, nil
	}
	if _, err = tx.Exec(`DELETE FROM e2ee_observed WHERE owner=? AND group_id=? AND rowid NOT IN
		(SELECT rowid FROM e2ee_observed WHERE owner=? AND group_id=? ORDER BY epoch DESC,at_ns DESC,msgid DESC LIMIT ?)`,
		owner, group, owner, group, E2EECommitHistory); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// E2EEObservedCommits lists a chat's commits from an epoch on: by epoch, and
// within an epoch in position order, so the first candidate of each epoch is
// the one devices try first.
func (s *Store) E2EEObservedCommits(owner, group string, from int64, limit int) ([]E2EEObserved, error) {
	if limit <= 0 || limit > e2eeCommitPageMaximum {
		limit = e2eeCommitPageMaximum
	}
	rows, err := s.DB.Query("SELECT epoch,at_ns,msgid,sender,commit_data,arrived_ns FROM e2ee_observed WHERE owner=? AND group_id=? AND epoch>=? ORDER BY epoch,at_ns,msgid LIMIT ?", owner, group, from, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []E2EEObserved{}
	for rows.Next() {
		var v E2EEObserved
		var arrived int64
		if err = rows.Scan(&v.Epoch, &v.At, &v.MsgID, &v.Sender, &v.Commit, &arrived); err != nil {
			return nil, err
		}
		v.Arrived = unixTime(arrived)
		out = append(out, v)
	}
	return out, rows.Err()
}

// E2EELatestObservedEpoch returns the highest epoch with a recorded commit, or -1.
func (s *Store) E2EELatestObservedEpoch(owner, group string) (int64, error) {
	var epoch sql.NullInt64
	if err := s.DB.QueryRow("SELECT max(epoch) FROM e2ee_observed WHERE owner=? AND group_id=?", owner, group).Scan(&epoch); err != nil {
		return 0, err
	}
	if !epoch.Valid {
		return -1, nil
	}
	return epoch.Int64, nil
}

// AddE2EEChatWelcome stores a Welcome that arrived over IRC for every device
// the account has, and reports how many devices received it. A Welcome already
// stored for the group and epoch is kept once.
func (s *Store) AddE2EEChatWelcome(owner string, v E2EEChatWelcome) (int, error) {
	tx, err := s.DB.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	now := time.Now()
	if _, err = tx.Exec("DELETE FROM e2ee_chat_welcomes WHERE created_ns<?", now.Add(-E2EEWelcomeLifetime).UnixNano()); err != nil {
		return 0, err
	}
	rows, err := tx.Query("SELECT device FROM e2ee_devices WHERE owner=? ORDER BY device", owner)
	if err != nil {
		return 0, err
	}
	devices := []string{}
	for rows.Next() {
		var device string
		if err = rows.Scan(&device); err != nil {
			rows.Close()
			return 0, err
		}
		devices = append(devices, device)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return 0, err
	}
	stored := 0
	for _, device := range devices {
		var duplicate bool
		var pending int
		if err = tx.QueryRow("SELECT EXISTS(SELECT 1 FROM e2ee_chat_welcomes WHERE owner=? AND device=? AND group_id=? AND epoch=? AND welcome=?),(SELECT count(*) FROM e2ee_chat_welcomes WHERE owner=? AND device=?)",
			owner, device, v.Group, v.Epoch, v.Welcome, owner, device).Scan(&duplicate, &pending); err != nil {
			return 0, err
		}
		if duplicate {
			stored++
			continue
		}
		if pending >= E2EEMaxPendingWelcomes {
			continue
		}
		if _, err = tx.Exec("INSERT INTO e2ee_chat_welcomes(owner,device,group_id,epoch,kind,replaces,target,sender,welcome,at_ns,msgid,created_ns) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)",
			owner, device, v.Group, v.Epoch, v.Kind, v.Replaces, v.Target, v.Sender, v.Welcome, v.At, v.MsgID, now.UnixNano()); err != nil {
			return 0, err
		}
		stored++
	}
	return stored, tx.Commit()
}

// E2EEChatWelcomes lists the unexpired Welcomes for one device, oldest first.
func (s *Store) E2EEChatWelcomes(owner, device string) ([]E2EEChatWelcome, error) {
	rows, err := s.DB.Query("SELECT id,device,group_id,epoch,kind,replaces,target,sender,welcome,at_ns,msgid,created_ns FROM e2ee_chat_welcomes WHERE owner=? AND device=? AND created_ns>=? ORDER BY at_ns,msgid,id",
		owner, device, time.Now().Add(-E2EEWelcomeLifetime).UnixNano())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []E2EEChatWelcome{}
	for rows.Next() {
		var v E2EEChatWelcome
		var created int64
		if err = rows.Scan(&v.ID, &v.Device, &v.Group, &v.Epoch, &v.Kind, &v.Replaces, &v.Target, &v.Sender, &v.Welcome, &v.At, &v.MsgID, &created); err != nil {
			return nil, err
		}
		v.CreatedAt = unixTime(created)
		out = append(out, v)
	}
	return out, rows.Err()
}

// DeleteE2EEChatWelcome removes a processed Welcome of one of the account's devices.
func (s *Store) DeleteE2EEChatWelcome(owner string, id int64) error {
	result, err := s.DB.Exec("DELETE FROM e2ee_chat_welcomes WHERE owner=? AND id=?", owner, id)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// e2eeRetryDelay is the wait after a delivery attempt: 1, 5 and 30 minutes,
// then hourly.
func e2eeRetryDelay(attempts int) time.Duration {
	switch attempts {
	case 0:
		return 0
	case 1:
		return time.Minute
	case 2:
		return 5 * time.Minute
	case 3:
		return 30 * time.Minute
	default:
		return time.Hour
	}
}

// AddE2EEOutbox keeps a Welcome for each recipient account until that account's
// server confirms it.
func (s *Store) AddE2EEOutbox(owner, group, transfer string, recipients []string, object []byte) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now()
	if _, err = tx.Exec("DELETE FROM e2ee_outbox WHERE created_ns<?", now.Add(-E2EEWelcomeLifetime).UnixNano()); err != nil {
		return err
	}
	var count int
	if err = tx.QueryRow("SELECT count(*) FROM e2ee_outbox WHERE owner=? AND state=?", owner, E2EEOutboxPending).Scan(&count); err != nil {
		return err
	}
	if count+len(recipients) > E2EEMaxOutbox {
		return ErrE2EELimit
	}
	for _, recipient := range recipients {
		if _, err = tx.Exec("INSERT INTO e2ee_outbox(owner,group_id,recipient,transfer,object,attempts,next_ns,created_ns,state) VALUES(?,?,?,?,?,0,?,?,?)",
			owner, group, recipient, transfer, object, now.UnixNano(), now.UnixNano(), E2EEOutboxPending); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// DueE2EEOutbox returns the account's Welcomes whose next attempt is due and
// counts the attempt, so the next one waits according to e2eeRetryDelay.
func (s *Store) DueE2EEOutbox(owner string, now time.Time, limit int) ([]E2EEOutboxItem, error) {
	tx, err := s.DB.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err = tx.Exec("DELETE FROM e2ee_outbox WHERE created_ns<?", now.Add(-E2EEWelcomeLifetime).UnixNano()); err != nil {
		return nil, err
	}
	rows, err := tx.Query("SELECT id,group_id,recipient,transfer,object,attempts,state,created_ns FROM e2ee_outbox WHERE owner=? AND state=? AND next_ns<=? ORDER BY next_ns,id LIMIT ?", owner, E2EEOutboxPending, now.UnixNano(), limit)
	if err != nil {
		return nil, err
	}
	out := []E2EEOutboxItem{}
	for rows.Next() {
		var v E2EEOutboxItem
		var created int64
		if err = rows.Scan(&v.ID, &v.Group, &v.Recipient, &v.Transfer, &v.Object, &v.Attempts, &v.State, &created); err != nil {
			rows.Close()
			return nil, err
		}
		v.CreatedAt = unixTime(created)
		out = append(out, v)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, err
	}
	for _, v := range out {
		if _, err = tx.Exec("UPDATE e2ee_outbox SET attempts=attempts+1,next_ns=? WHERE id=?", now.Add(e2eeRetryDelay(v.Attempts+1)).UnixNano(), v.ID); err != nil {
			return nil, err
		}
	}
	return out, tx.Commit()
}

// DueE2EEOutboxOwners lists the accounts with a Welcome delivery due.
func (s *Store) DueE2EEOutboxOwners(now time.Time) ([]string, error) {
	rows, err := s.DB.Query("SELECT DISTINCT owner FROM e2ee_outbox WHERE state=? AND next_ns<=? AND created_ns>=? ORDER BY owner", E2EEOutboxPending, now.UnixNano(), now.Add(-E2EEWelcomeLifetime).UnixNano())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var owner string
		if err = rows.Scan(&owner); err != nil {
			return nil, err
		}
		out = append(out, owner)
	}
	return out, rows.Err()
}

// E2EELastResortPackages returns each device's reusable last-resort key
// package without taking a single-use one.
func (s *Store) E2EELastResortPackages(owner string) ([]E2EEClaim, error) {
	rows, err := s.DB.Query("SELECT device,certificate,last_resort FROM e2ee_devices WHERE owner=? AND last_resort IS NOT NULL ORDER BY created_ns,device", owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []E2EEClaim{}
	for rows.Next() {
		v := E2EEClaim{LastResort: true}
		if err = rows.Scan(&v.Device, &v.Certificate, &v.KeyPackage); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// NextE2EEOutbox returns when the account's next Welcome delivery is due.
func (s *Store) NextE2EEOutbox(owner string) (time.Time, bool, error) {
	var next sql.NullInt64
	if err := s.DB.QueryRow("SELECT min(next_ns) FROM e2ee_outbox WHERE owner=? AND state=?", owner, E2EEOutboxPending).Scan(&next); err != nil {
		return time.Time{}, false, err
	}
	if !next.Valid {
		return time.Time{}, false, nil
	}
	return unixTime(next.Int64), true, nil
}

// WakeE2EEOutbox makes the account's waiting Welcomes for a recipient due now,
// for example when that account is seen online.
func (s *Store) WakeE2EEOutbox(owner, recipient string) (bool, error) {
	result, err := s.DB.Exec("UPDATE e2ee_outbox SET next_ns=? WHERE owner=? AND state=? AND lower(recipient)=lower(?) AND next_ns>?", time.Now().UnixNano(), owner, E2EEOutboxPending, recipient, time.Now().UnixNano())
	if err != nil {
		return false, err
	}
	n, _ := result.RowsAffected()
	return n > 0, nil
}

// ConfirmE2EEOutbox ends the retries of a Welcome when the recipient's server
// sent its receipt, or records that the recipient declined. It returns the
// Welcome's group.
func (s *Store) ConfirmE2EEOutbox(owner, transfer, recipient string, declined bool) (string, error) {
	state := E2EEOutboxDelivered
	if declined {
		state = E2EEOutboxDeclined
	}
	var group string
	err := s.DB.QueryRow("SELECT group_id FROM e2ee_outbox WHERE owner=? AND transfer=? AND lower(recipient)=lower(?)", owner, transfer, recipient).Scan(&group)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	if _, err = s.DB.Exec("UPDATE e2ee_outbox SET state=? WHERE owner=? AND transfer=? AND lower(recipient)=lower(?) AND state<>?", state, owner, transfer, recipient, E2EEOutboxDeclined); err != nil {
		return "", err
	}
	return group, nil
}

// E2EEOutboxFor returns the Welcomes of a chat that the account sent to a
// recipient, newest first, so a log request can deliver them again.
func (s *Store) E2EEOutboxFor(owner, group, recipient string) ([]E2EEOutboxItem, error) {
	rows, err := s.DB.Query("SELECT id,group_id,recipient,transfer,object,attempts,state,created_ns FROM e2ee_outbox WHERE owner=? AND group_id=? AND lower(recipient)=lower(?) AND state<>? ORDER BY id DESC LIMIT 4", owner, group, recipient, E2EEOutboxDeclined)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []E2EEOutboxItem{}
	for rows.Next() {
		var v E2EEOutboxItem
		var created int64
		if err = rows.Scan(&v.ID, &v.Group, &v.Recipient, &v.Transfer, &v.Object, &v.Attempts, &v.State, &created); err != nil {
			return nil, err
		}
		v.CreatedAt = unixTime(created)
		out = append(out, v)
	}
	return out, rows.Err()
}

// E2EEOutboxStates returns, for one chat, the newest delivery state of each
// recipient the account sent a Welcome to.
func (s *Store) E2EEOutboxStates(owner, group string) (map[string]string, error) {
	rows, err := s.DB.Query("SELECT recipient,state FROM e2ee_outbox WHERE owner=? AND group_id=? ORDER BY id", owner, group)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var recipient, state string
		if err = rows.Scan(&recipient, &state); err != nil {
			return nil, err
		}
		out[recipient] = state
	}
	return out, rows.Err()
}

// AddE2EERejoin keeps a device's request to be added to a chat again. A newer
// request from the same device replaces the older one.
func (s *Store) AddE2EERejoin(owner, group string, v E2EERejoin) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now()
	if _, err = tx.Exec("DELETE FROM e2ee_rejoins WHERE created_ns<? OR (owner=? AND group_id=? AND lower(account)=lower(?) AND device=?)", now.Add(-E2EERejoinLifetime).UnixNano(), owner, group, v.Account, v.Device); err != nil {
		return err
	}
	var count int
	if err = tx.QueryRow("SELECT count(*) FROM e2ee_rejoins WHERE owner=? AND group_id=?", owner, group).Scan(&count); err != nil {
		return err
	}
	if count >= E2EEMaxRejoins {
		return ErrE2EELimit
	}
	if _, err = tx.Exec("INSERT INTO e2ee_rejoins(owner,group_id,account,device,certificate,key_package,created_ns) VALUES(?,?,?,?,?,?,?)", owner, group, v.Account, v.Device, v.Certificate, v.KeyPackage, now.UnixNano()); err != nil {
		return err
	}
	return tx.Commit()
}

// E2EERejoins lists a chat's waiting rejoin requests, oldest first.
func (s *Store) E2EERejoins(owner, group string) ([]E2EERejoin, error) {
	rows, err := s.DB.Query("SELECT id,account,device,certificate,key_package,created_ns FROM e2ee_rejoins WHERE owner=? AND group_id=? AND created_ns>=? ORDER BY id", owner, group, time.Now().Add(-E2EERejoinLifetime).UnixNano())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []E2EERejoin{}
	for rows.Next() {
		var v E2EERejoin
		var created int64
		if err = rows.Scan(&v.ID, &v.Account, &v.Device, &v.Certificate, &v.KeyPackage, &created); err != nil {
			return nil, err
		}
		v.CreatedAt = unixTime(created)
		out = append(out, v)
	}
	return out, rows.Err()
}

// DeleteE2EERejoin removes a handled rejoin request of one of the account's chats.
func (s *Store) DeleteE2EERejoin(owner string, id int64) error {
	result, err := s.DB.Exec("DELETE FROM e2ee_rejoins WHERE owner=? AND id=?", owner, id)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// PutE2EEDeviceList stores the account's signed device list. The counter must
// go up; the same list again changes nothing. It reports whether the list changed.
func (s *Store) PutE2EEDeviceList(owner string, list []byte, counter int64) (bool, error) {
	tx, err := s.DB.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var identity bool
	if err = tx.QueryRow("SELECT EXISTS(SELECT 1 FROM e2ee_identities WHERE owner=?)", owner).Scan(&identity); err != nil {
		return false, err
	}
	if !identity {
		return false, ErrE2EEConflict
	}
	var current []byte
	var have int64
	err = tx.QueryRow("SELECT list,counter FROM e2ee_device_lists WHERE owner=?", owner).Scan(&current, &have)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return false, err
	case have == counter && bytes.Equal(current, list):
		return false, nil
	case counter <= have:
		return false, ErrE2EEConflict
	}
	if _, err = tx.Exec(`INSERT INTO e2ee_device_lists(owner,list,counter,updated_ns) VALUES(?,?,?,?)
		ON CONFLICT(owner) DO UPDATE SET list=excluded.list,counter=excluded.counter,updated_ns=excluded.updated_ns`, owner, list, counter, time.Now().UnixNano()); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// E2EEDeviceList returns the account's signed device list, or ErrNotFound.
func (s *Store) E2EEDeviceList(owner string) ([]byte, int64, error) {
	var list []byte
	var counter int64
	err := s.DB.QueryRow("SELECT list,counter FROM e2ee_device_lists WHERE owner=?", owner).Scan(&list, &counter)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, 0, ErrNotFound
	}
	return list, counter, err
}

// E2EEAccountsWithIdentity lists the accounts that have an identity here.
func (s *Store) E2EEAccountsWithIdentity() ([]string, error) {
	rows, err := s.DB.Query("SELECT owner FROM e2ee_identities ORDER BY owner")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var owner string
		if err = rows.Scan(&owner); err != nil {
			return nil, err
		}
		out = append(out, owner)
	}
	return out, rows.Err()
}
