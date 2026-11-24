package irc

// FilehostLogins copies the owner's IRC logins, so a file on a network's file
// host can still be deleted after the owner's connections are dropped.
func (m *Manager) FilehostLogins(owner string) func(owner, network string) (user, password string, err error) {
	w := m.space(owner)
	w.mu.Lock()
	logins := map[string][2]string{}
	for id, cfg := range w.configs {
		logins[id] = [2]string{cfg.SASLUser, cfg.SASLPassword}
	}
	w.mu.Unlock()
	return func(_, network string) (string, string, error) {
		login, ok := logins[network]
		if !ok {
			return "", "", ErrNotFound
		}
		return login[0], login[1], nil
	}
}

func (m *Manager) FilehostConfig(owner, network string) (host, advertised, user, password string, err error) {
	w := m.space(owner)
	w.mu.Lock()
	defer w.mu.Unlock()
	cfg, ok := w.configs[network]
	if !ok {
		err = ErrNotFound
		return
	}
	host = cfg.Host
	advertised = w.info[network].FilehostURL
	user = cfg.SASLUser
	password = cfg.SASLPassword
	return
}
