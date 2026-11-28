package irc

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Handshake lines carry everything an encrypted chat needs besides its
// messages: directory requests and replies, commits, Welcomes, receipts, log
// requests, rejoin requests and change notices. They travel between IRCord
// servers as TAGMSG lines with the same +ircord/e2ee tag as encrypted messages:
//
//	@+ircord/e2ee=h.<kind>.<transfer>.<index>.<count>.<data> TAGMSG <target>
//
// "h" marks a handshake line; phase 1 parsers ignore it. transfer is 16 random
// bytes in base64url, chosen by the sender; a reply reuses the transfer of its
// request. index and count number the chunks of one object from 0, at most
// handshakeMaxChunks. data is a base64url slice of the object.
//
// docs/e2ee-handshake.md describes the objects. The backend builds and reads
// their headers; every MLS object inside stays opaque.

const handshakeVersion = 2

// The kinds of handshake object.
const (
	kindDirectoryRequest byte = 'q'
	kindDirectory        byte = 'd'
	kindCommit           byte = 'c'
	kindWelcome          byte = 'w'
	kindReceipt          byte = 'k'
	kindLogRequest       byte = 'l'
	kindRejoin           byte = 'j'
	kindNotice           byte = 'n'
)

const (
	handshakePrefix = "h."
	// The tag value of a handshake line stays within the 3,804 characters an
	// encrypted message's tag uses and the self-test proves. The header takes
	// at most 33 of them.
	handshakeTagBudget  = 3804
	handshakeHeaderMax  = len("h.x.") + 22 + len(".31.32.")
	handshakeChunkBytes = (handshakeTagBudget - handshakeHeaderMax) / 4 * 3
	handshakeMaxChunks  = 32
	handshakeMaxObject  = handshakeChunkBytes * handshakeMaxChunks
	transferLength      = 22
)

func validKind(k byte) bool { return strings.IndexByte("qdcwkljn", k) >= 0 }

// newTransfer returns a fresh transfer ID.
func newTransfer() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b[:])
}

func validTransfer(v string) bool { return len(v) == transferLength && validPayload(v) }

type handshakeLine struct {
	kind     byte
	transfer string
	index    int
	count    int
	data     []byte
}

func (l handshakeLine) tag() string {
	return fmt.Sprintf("h.%c.%s.%d.%d.%s", l.kind, l.transfer, l.index, l.count, base64.RawURLEncoding.EncodeToString(l.data))
}

// parseHandshakeLine reads a handshake tag value. Anything malformed is not a
// handshake line.
func parseHandshakeLine(value string) (handshakeLine, bool) {
	if !strings.HasPrefix(value, handshakePrefix) || len(value) > handshakeTagBudget {
		return handshakeLine{}, false
	}
	parts := strings.SplitN(value[len(handshakePrefix):], ".", 5)
	if len(parts) != 5 || len(parts[0]) != 1 || !validKind(parts[0][0]) || !validTransfer(parts[1]) {
		return handshakeLine{}, false
	}
	index, err1 := strconv.Atoi(parts[2])
	count, err2 := strconv.Atoi(parts[3])
	if err1 != nil || err2 != nil || count < 1 || count > handshakeMaxChunks || index < 0 || index >= count || strconv.Itoa(index) != parts[2] || strconv.Itoa(count) != parts[3] {
		return handshakeLine{}, false
	}
	if !validPayload(parts[4]) {
		return handshakeLine{}, false
	}
	data, err := base64.RawURLEncoding.DecodeString(parts[4])
	if err != nil || len(data) > handshakeChunkBytes {
		return handshakeLine{}, false
	}
	return handshakeLine{kind: parts[0][0], transfer: parts[1], index: index, count: count, data: data}, true
}

var errObjectTooLarge = errors.New("the object is too large for a handshake transfer")

// chunkObject cuts an object into the lines of one transfer.
func chunkObject(kind byte, transfer string, object []byte) ([]handshakeLine, error) {
	if len(object) == 0 || len(object) > handshakeMaxObject {
		return nil, errObjectTooLarge
	}
	count := (len(object) + handshakeChunkBytes - 1) / handshakeChunkBytes
	lines := make([]handshakeLine, 0, count)
	for i := 0; i < count; i++ {
		end := min(len(object), (i+1)*handshakeChunkBytes)
		lines = append(lines, handshakeLine{kind: kind, transfer: transfer, index: i, count: count, data: object[i*handshakeChunkBytes : end]})
	}
	return lines, nil
}

// wireWriter and wireReader encode object fields: unsigned integers as
// uvarints, strings and byte strings as a uvarint length and the bytes.
type wireWriter struct{ b []byte }

func newObject(kind byte) *wireWriter {
	return &wireWriter{b: []byte{handshakeVersion, kind}}
}

func (w *wireWriter) uint(v uint64) *wireWriter { w.b = binary.AppendUvarint(w.b, v); return w }
func (w *wireWriter) int(v int64) *wireWriter {
	if v < 0 {
		v = 0
	}
	return w.uint(uint64(v))
}
func (w *wireWriter) bool(v bool) *wireWriter {
	if v {
		return w.uint(1)
	}
	return w.uint(0)
}
func (w *wireWriter) bytes(v []byte) *wireWriter {
	w.b = binary.AppendUvarint(w.b, uint64(len(v)))
	w.b = append(w.b, v...)
	return w
}
func (w *wireWriter) str(v string) *wireWriter { return w.bytes([]byte(v)) }

type wireReader struct {
	b   []byte
	bad bool
}

// openObject checks the version and kind an object starts with.
func openObject(kind byte, object []byte) *wireReader {
	if len(object) < 2 || object[0] != handshakeVersion || object[1] != kind {
		return &wireReader{bad: true}
	}
	return &wireReader{b: object[2:]}
}

func (r *wireReader) uint() uint64 {
	if r.bad {
		return 0
	}
	v, n := binary.Uvarint(r.b)
	if n <= 0 {
		r.bad = true
		return 0
	}
	r.b = r.b[n:]
	return v
}
func (r *wireReader) int() int64 {
	v := r.uint()
	if v > 1<<62 {
		r.bad = true
		return 0
	}
	return int64(v)
}
func (r *wireReader) bool() bool { return r.uint() == 1 }
func (r *wireReader) bytes(limit int) []byte {
	n := r.uint()
	if r.bad || n > uint64(len(r.b)) || n > uint64(limit) {
		r.bad = true
		return nil
	}
	v := append([]byte(nil), r.b[:n]...)
	r.b = r.b[n:]
	return v
}
func (r *wireReader) str(limit int) string { return string(r.bytes(limit)) }

// done reports a well-formed object: every field read and nothing left over.
func (r *wireReader) done() bool { return !r.bad && len(r.b) == 0 }

// Field bounds, matching the HTTP API's.
const (
	wireMaxName   = 100
	wireMaxGroup  = 128
	wireMaxDevice = 64
	wireMaxKey    = 4096
	wireMaxMLS    = 64 << 10
	wireMaxMsgID  = 128
	wireMaxDevs   = 32
)

type directoryRequest struct {
	Account string
	Claim   bool // hand out one key package for each device
}

func (v directoryRequest) encode() []byte {
	return newObject(kindDirectoryRequest).str(v.Account).bool(v.Claim).b
}

func decodeDirectoryRequest(b []byte) (directoryRequest, bool) {
	r := openObject(kindDirectoryRequest, b)
	v := directoryRequest{Account: r.str(wireMaxName), Claim: r.bool()}
	return v, r.done() && v.Account != ""
}

// E2EEDirectoryDevice is one device in a directory answer. KeyPackage is set
// when the request claimed packages and the device had one.
type E2EEDirectoryDevice struct {
	ID          string
	Certificate []byte
	KeyPackage  []byte
	LastResort  bool
}

// E2EEDirectory is an account's identity, signed device list and devices, as
// the IRCord server that holds them answered over IRC.
type E2EEDirectory struct {
	Account     string
	IdentityKey []byte
	Generation  int64
	DeviceList  []byte
	ListCounter int64
	Devices     []E2EEDirectoryDevice
}

func (v E2EEDirectory) encode() []byte {
	w := newObject(kindDirectory).str(v.Account).bytes(v.IdentityKey).int(v.Generation).bytes(v.DeviceList).int(v.ListCounter).uint(uint64(len(v.Devices)))
	for _, d := range v.Devices {
		w.str(d.ID).bytes(d.Certificate).bytes(d.KeyPackage).bool(d.LastResort)
	}
	return w.b
}

func decodeDirectory(b []byte) (E2EEDirectory, bool) {
	r := openObject(kindDirectory, b)
	v := E2EEDirectory{Account: r.str(wireMaxName), IdentityKey: r.bytes(wireMaxKey), Generation: r.int(), DeviceList: r.bytes(wireMaxKey), ListCounter: r.int()}
	n := r.uint()
	if n > wireMaxDevs {
		return v, false
	}
	for i := uint64(0); i < n && !r.bad; i++ {
		d := E2EEDirectoryDevice{ID: r.str(wireMaxDevice), Certificate: r.bytes(wireMaxKey), KeyPackage: r.bytes(wireMaxKey), LastResort: r.bool()}
		if d.ID == "" || !validPayload(d.ID) || len(d.Certificate) == 0 {
			return v, false
		}
		v.Devices = append(v.Devices, d)
	}
	return v, r.done() && v.Account != "" && len(v.IdentityKey) > 0
}

// commitEntry is one commit. A live commit has no position of its own: the
// IRC server's stamp on its last line is its position. Entries in the answer to
// a log request carry the position and sender recorded when they were first
// seen.
type commitEntry struct {
	Epoch  int64
	Commit []byte
	At     int64
	MsgID  string
	Sender string
}

type commitObject struct {
	Group   string
	Entries []commitEntry
	More    bool // a log answer that stopped early; ask again from the next epoch
}

func (v commitObject) encode() []byte {
	w := newObject(kindCommit).str(v.Group).uint(uint64(len(v.Entries)))
	for _, e := range v.Entries {
		w.int(e.Epoch).bytes(e.Commit).int(e.At).str(e.MsgID).str(e.Sender)
	}
	return w.bool(v.More).b
}

func decodeCommit(b []byte) (commitObject, bool) {
	r := openObject(kindCommit, b)
	v := commitObject{Group: r.str(wireMaxGroup)}
	n := r.uint()
	if n > 1000 {
		return v, false
	}
	for i := uint64(0); i < n && !r.bad; i++ {
		e := commitEntry{Epoch: r.int(), Commit: r.bytes(wireMaxMLS), At: r.int(), MsgID: r.str(wireMaxMsgID), Sender: r.str(wireMaxName)}
		if len(e.Commit) == 0 {
			return v, false
		}
		v.Entries = append(v.Entries, e)
	}
	v.More = r.bool()
	return v, r.done() && validGroupID(v.Group)
}

// Chat kinds on the wire.
const (
	wireDirect byte = 0
	wireGroup  byte = 1
)

type welcomeObject struct {
	Group    string
	Epoch    int64
	Kind     byte
	Replaces string // direct chats: the group this one replaces after a fresh start
	Channel  string // groups: the group's channel
	Welcome  []byte
}

func (v welcomeObject) encode() []byte {
	return newObject(kindWelcome).str(v.Group).int(v.Epoch).uint(uint64(v.Kind)).str(v.Replaces).str(v.Channel).bytes(v.Welcome).b
}

func decodeWelcome(b []byte) (welcomeObject, bool) {
	r := openObject(kindWelcome, b)
	v := welcomeObject{Group: r.str(wireMaxGroup), Epoch: r.int()}
	kind := r.uint()
	v.Kind = byte(kind)
	v.Replaces, v.Channel, v.Welcome = r.str(wireMaxGroup), r.str(wireMaxName), r.bytes(wireMaxMLS)
	ok := r.done() && validGroupID(v.Group) && len(v.Welcome) > 0 && (v.Replaces == "" || validGroupID(v.Replaces))
	switch {
	case kind == uint64(wireDirect):
		ok = ok && v.Channel == ""
	case kind == uint64(wireGroup):
		ok = ok && v.Channel == groupChannel(v.Group)
	default:
		ok = false
	}
	return v, ok
}

type receiptObject struct {
	Group    string
	Declined bool
}

func (v receiptObject) encode() []byte { return newObject(kindReceipt).str(v.Group).bool(v.Declined).b }

func decodeReceipt(b []byte) (receiptObject, bool) {
	r := openObject(kindReceipt, b)
	v := receiptObject{Group: r.str(wireMaxGroup), Declined: r.bool()}
	return v, r.done() && validGroupID(v.Group)
}

type logRequest struct {
	Group string
	From  int64
}

func (v logRequest) encode() []byte { return newObject(kindLogRequest).str(v.Group).int(v.From).b }

func decodeLogRequest(b []byte) (logRequest, bool) {
	r := openObject(kindLogRequest, b)
	v := logRequest{Group: r.str(wireMaxGroup), From: r.int()}
	return v, r.done() && validGroupID(v.Group)
}

type rejoinObject struct {
	Group       string
	Device      string
	Certificate []byte
	KeyPackage  []byte
}

func (v rejoinObject) encode() []byte {
	return newObject(kindRejoin).str(v.Group).str(v.Device).bytes(v.Certificate).bytes(v.KeyPackage).b
}

func decodeRejoin(b []byte) (rejoinObject, bool) {
	r := openObject(kindRejoin, b)
	v := rejoinObject{Group: r.str(wireMaxGroup), Device: r.str(wireMaxDevice), Certificate: r.bytes(wireMaxKey), KeyPackage: r.bytes(wireMaxKey)}
	return v, r.done() && validGroupID(v.Group) && v.Device != "" && validPayload(v.Device) && len(v.Certificate) > 0 && len(v.KeyPackage) > 0
}

// Notice reasons.
const (
	noticeDevices  byte = 0
	noticeIdentity byte = 1
)

type noticeObject struct{ Reason byte }

func (v noticeObject) encode() []byte { return newObject(kindNotice).uint(uint64(v.Reason)).b }

func decodeNotice(b []byte) (noticeObject, bool) {
	r := openObject(kindNotice, b)
	reason := r.uint()
	return noticeObject{Reason: byte(reason)}, r.done() && reason <= uint64(noticeIdentity)
}

// validGroupID accepts the base64url group IDs the HTTP API takes.
func validGroupID(v string) bool { return v != "" && len(v) <= wireMaxGroup && validPayload(v) }

// groupChannelPrefix starts the name of every encrypted group's channel; the
// rest is the group's 16-byte ID in hex.
const groupChannelPrefix = "#e2ee-"

// groupChannel returns the channel of a group, or "" when the group ID is not
// 16 bytes of base64url.
func groupChannel(group string) string {
	raw, err := base64.RawURLEncoding.DecodeString(group)
	if err != nil || len(raw) != 16 || base64.RawURLEncoding.EncodeToString(raw) != group {
		return ""
	}
	return groupChannelPrefix + fmt.Sprintf("%x", raw)
}

// isGroupChannel reports a channel name of the encrypted group form.
func isGroupChannel(name string) bool {
	name = strings.ToLower(name)
	if !strings.HasPrefix(name, groupChannelPrefix) || len(name) != len(groupChannelPrefix)+32 {
		return false
	}
	for _, r := range name[len(groupChannelPrefix):] {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}
