package api

import (
	"context"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"ircord/backend/internal/storage"
)

// The end-to-end encryption modes server owners choose from.
const (
	EncryptionOff          = "off"
	EncryptionExperimental = "experimental"
)

// encryptionLockReason explains a mode that IRCORD_E2EE fixes.
const encryptionLockReason = "The server's IRCORD_E2EE setting fixes this mode. To choose it here, remove IRCORD_E2EE and restart the server."

// EncryptionSetting is the server's end-to-end encryption mode as owners see it.
type EncryptionSetting struct {
	Mode       string     `json:"mode" enum:"off,experimental" doc:"The mode in effect now. Experimental lets people on this server's network start encrypted direct messages."`
	Locked     bool       `json:"locked" doc:"True when the server's configuration fixes the mode, so owners can't change it here."`
	LockReason string     `json:"lockReason,omitempty" doc:"Why the mode can't be changed, present when locked."`
	UpdatedAt  *time.Time `json:"updatedAt,omitempty" doc:"When an owner last changed the mode. Absent when locked or never changed."`
	UpdatedBy  string     `json:"updatedBy,omitempty" doc:"The owner who last changed the mode."`
}

// EncryptionSettingUpdate chooses the encryption mode.
type EncryptionSettingUpdate struct {
	Mode   string `json:"mode" enum:"off,experimental"`
	Reason string `json:"reason,omitempty" maxLength:"500"`
}

func (s *Server) encryptionSetting(ctx context.Context) (*Output[EncryptionSetting], error) {
	out := EncryptionSetting{Mode: EncryptionOff, Locked: s.Manager.E2EELocked()}
	if s.Manager.E2EEEnabled() {
		out.Mode = EncryptionExperimental
	}
	if out.Locked {
		out.LockReason = encryptionLockReason
		return result(out, nil)
	}
	saved, err := s.options.Store.ServerSetting(ctx, storage.SettingEncryption)
	if err != nil {
		return nil, huma.Error503ServiceUnavailable("Server settings are unavailable.")
	}
	if !saved.UpdatedAt.IsZero() {
		out.UpdatedAt, out.UpdatedBy = &saved.UpdatedAt, saved.UpdatedBy
	}
	return result(out, nil)
}

func (s *Server) registerEncryptionAdministration() {
	huma.Register(s.API, huma.Operation{OperationID: "administration-encryption", Method: http.MethodGet, Path: administrationPrefix + "/encryption", Summary: "The server's end-to-end encryption mode"}, func(ctx context.Context, _ *struct{}) (*Output[EncryptionSetting], error) {
		if _, err := s.requireOwner(ctx); err != nil {
			return nil, err
		}
		return s.encryptionSetting(ctx)
	})
	huma.Register(s.API, huma.Operation{OperationID: "administration-update-encryption", Method: http.MethodPut, Path: administrationPrefix + "/encryption", Summary: "Turn end-to-end encryption on or off",
		Description: "Takes effect at once, without a restart. Turning encryption off deletes nothing: devices with keys can still read earlier encrypted messages, but nobody can start or continue an encrypted conversation until it is on again. Refused with 409 when the server's configuration fixes the mode.", MaxBodyBytes: 4096}, func(ctx context.Context, in *struct{ Body EncryptionSettingUpdate }) (*Output[EncryptionSetting], error) {
		actor, err := s.requireOwner(ctx)
		if err != nil {
			return nil, err
		}
		if s.Manager.E2EELocked() {
			return nil, problem(409, encryptionLockReason, problemResourceConflict)
		}
		s.encryptionMu.Lock()
		defer s.encryptionMu.Unlock()
		if err = s.options.Store.SaveServerSetting(ctx, storage.SettingEncryption, in.Body.Mode, actor.Account.Username); err != nil {
			s.audit(actor, "encryption-mode", in.Body.Mode, in.Body.Reason, "failed")
			return nil, huma.Error503ServiceUnavailable("The encryption mode couldn't be saved.")
		}
		s.Manager.SetE2EE(in.Body.Mode == EncryptionExperimental)
		s.audit(actor, "encryption-mode", in.Body.Mode, in.Body.Reason, "saved")
		return s.encryptionSetting(ctx)
	})
}
