package model

// Picture kinds name what a remote picture is for, which sets its size limit.
const (
	PictureAvatar     = "avatar"
	PictureBanner     = "banner"
	PictureBackground = "background"
)

// PictureSource turns a profile picture address into the address clients draw:
// the same address when it is on this server, a cached copy on this server when
// it is on another one, and empty when there is nothing safe to show.
type PictureSource func(kind, address string) string

func (f PictureSource) src(kind, address string) string {
	if f == nil || address == "" {
		return ""
	}
	return f(kind, address)
}

// WithPictures returns the member with its display addresses set.
func (m Member) WithPictures(f PictureSource) Member {
	m.AvatarSrc = f.src(PictureAvatar, m.AvatarURL)
	m.BannerSrc = f.src(PictureBanner, m.BannerURL)
	m.ProfileBackgroundSrc = f.src(PictureBackground, m.ProfileBackgroundURL)
	return m
}

// WithPictures returns the person with its display address set.
func (p Person) WithPictures(f PictureSource) Person {
	p.AvatarSrc = f.src(PictureAvatar, p.AvatarURL)
	return p
}

// WithPictures returns the candidate with its display address set.
func (p PartyCandidate) WithPictures(f PictureSource) PartyCandidate {
	p.AvatarSrc = f.src(PictureAvatar, p.AvatarURL)
	return p
}
