package model

// End-to-end encryption between IRCord clients (ircord/issues#19). The backend
// carries and orders opaque client payloads; it never holds a key and never
// reads what a payload contains.

// EncryptedPlaceholder is the fixed body of every encrypted IRC line. Plain IRC
// clients, older IRCord clients and the CLI show it in place of the message.
const EncryptedPlaceholder = "[encrypted message - open IRCord to read]"

// MaxEncryptedPayload bounds the base64url ciphertext of one encrypted line. With
// the format prefix, the client ID and the label it fits the send tag budget.
const MaxEncryptedPayload = 3800

// The classes of an encrypted event. The class is visible so the server can keep
// auxiliary events out of unread counts, the inbox and push notifications.
const (
	EncryptedMessage   = "message"
	EncryptedAuxiliary = "auxiliary"
)

// Encrypted is an end-to-end encrypted event: an MLS message the clients decrypt.
type Encrypted struct {
	Version int    `json:"version" enum:"1" doc:"Format version of the +ircord/e2ee tag."`
	Class   string `json:"class" enum:"message,auxiliary" doc:"message for a chat message; auxiliary for a reaction, edit, delete or control event, which never counts as unread or notifies."`
	Payload string `json:"payload" minLength:"1" maxLength:"3800" pattern:"^[A-Za-z0-9_-]+$" doc:"The client's MLS message, base64url without padding. The server stores and relays it unchanged."`
}

// EncryptionCapability says that this server offers end-to-end encryption.
type EncryptionCapability struct {
	Experimental bool `json:"experimental" doc:"The feature has not had its outside review yet; clients label it experimental."`
	MaxPayload   int  `json:"maxPayload" doc:"Largest base64url payload one encrypted message may carry."`
}
