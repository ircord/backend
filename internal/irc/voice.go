package irc

import "ircord/backend/internal/model"

// NotifyVoiceRoster invalidates the server-wide room list on private account streams.
func (m *Manager) NotifyVoiceRoster() {
	m.mu.Lock()
	workspaces := make([]*workspace, 0, len(m.owners))
	for _, w := range m.owners {
		workspaces = append(workspaces, w)
	}
	m.mu.Unlock()
	for _, w := range workspaces {
		w.mu.Lock()
		if !w.deleted && len(w.ephemeralSubscribers) != 0 {
			w.publishEphemeral(model.VoiceRosterEvent{V: 1, Type: "voice.updated"})
		}
		w.mu.Unlock()
	}
}
