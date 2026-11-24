package irc

// FilehostLogins copies the owner's IRC logins, so a file on a network's file
// host can still be deleted after the owner's connections are dropped. The copy
// is kept until ForgetFilehostLogins, so that a deletion which has to be retried
// still has the logins the first attempt had.
func (m *Manager) FilehostLogins(owner string) func(owner, network string) (user, password string, err error) {
	m.mu.Lock()
	kept, ok := m.filehostLogins[owner]
	m.mu.Unlock()
	if ok {
		return kept
	}
	w := m.space(owner)
	w.mu.Lock()
	logins := map[string][2]string{}
	for id, cfg := range w.configs {
		logins[id] = [2]string{cfg.SASLUser, cfg.SASLPassword}
	}
	w.mu.Unlock()
	copied := func(_, network string) (string, string, error) {
		login, ok := logins[network]
		if !ok {
			return "", "", ErrNotFound
		}
		return login[0], login[1], nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.filehostLogins == nil {
		m.filehostLogins = map[string]func(string, string) (string, string, error){}
	}
	if kept, ok = m.filehostLogins[owner]; ok {
		return kept
	}
	m.filehostLogins[owner] = copied
	return copied
}

// ForgetFilehostLogins drops the copy FilehostLogins kept, once the owner's
// files are deleted.
func (m *Manager) ForgetFilehostLogins(owner string) {
	m.mu.Lock()
	delete(m.filehostLogins, owner)
	m.mu.Unlock()
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
