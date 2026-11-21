package appdata

import (
	"cmp"
	"encoding/json"
	"math"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"

	"ircord/backend/internal/irc"
	"ircord/backend/internal/model"
)

var gifSlug = regexp.MustCompile("^[A-Za-z0-9_-]{1,180}$")

type SavedGIF struct {
	model.GIF
	SavedAt string `json:"savedAt" format:"date-time"`
}

type EmojiUsage struct {
	ID       string `json:"id" minLength:"1" maxLength:"120"`
	Count    int    `json:"count" minimum:"1" maximum:"1000000"`
	LastUsed string `json:"lastUsed" format:"date-time"`
}

type Library struct {
	GIFs       []SavedGIF   `json:"gifs" nullable:"false" maxItems:"200" doc:"Saved Klipy GIFs, most recently saved first."`
	EmojiUsage []EmojiUsage `json:"emojiUsage" nullable:"false" maxItems:"120" doc:"Emoji ranked by use count with a two-week half-life."`
	SkinTone   string       `json:"skinTone" enum:"0,1,2,3,4,5"`
	UpdatedAt  *string      `json:"updatedAt" format:"date-time"`
}

type LibraryPatch struct {
	Uses     map[string]int `json:"uses,omitempty" maxProperties:"100" doc:"Emoji ID to additional picks. Each positive count is capped at 100."`
	Recent   []string       `json:"recent,omitempty" maxItems:"100" doc:"Picked IDs in oldest-to-newest order."`
	SkinTone *string        `json:"skinTone,omitempty" enum:"0,1,2,3,4,5"`
}

// Timestamp uses the same millisecond precision as browser clients.
func Timestamp(now time.Time) string { return now.UTC().Format("2006-01-02T15:04:05.000Z") }

func date(value string, now time.Time) string {
	if parsed, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return Timestamp(parsed)
	}
	return Timestamp(now)
}

func validEmoji(id string) bool {
	return bounded(id, 120) && !strings.ContainsAny(id, "<>\"") && !strings.ContainsFunc(id, unicode.IsSpace)
}

func ValidSavedGIF(g model.GIF) bool {
	return bounded(g.ID, 100) && gifSlug.MatchString(g.Slug) && irc.ValidGIF(&g)
}

func ReadLibrary(raw json.RawMessage, now time.Time) Library {
	var saved Library
	decode(raw, &saved)
	library := Library{
		GIFs: []SavedGIF{}, EmojiUsage: []EmojiUsage{},
		SkinTone: choice(saved.SkinTone, "0", "1", "2", "3", "4", "5"), UpdatedAt: saved.UpdatedAt,
	}
	if library.UpdatedAt != nil {
		library.UpdatedAt = pointer(date(*library.UpdatedAt, now))
	}
	seenGIF := map[string]bool{}
	for _, gif := range saved.GIFs {
		if len(library.GIFs) == 200 {
			break
		}
		if ValidSavedGIF(gif.GIF) && !seenGIF[gif.ID] {
			gif.SavedAt = date(gif.SavedAt, now)
			library.GIFs = append(library.GIFs, gif)
			seenGIF[gif.ID] = true
		}
	}
	seenEmoji := map[string]bool{}
	for _, usage := range saved.EmojiUsage {
		if len(library.EmojiUsage) == 120 {
			break
		}
		if validEmoji(usage.ID) && usage.Count > 0 && usage.Count <= 1_000_000 && !seenEmoji[usage.ID] {
			usage.LastUsed = date(usage.LastUsed, now)
			library.EmojiUsage = append(library.EmojiUsage, usage)
			seenEmoji[usage.ID] = true
		}
	}
	return library
}

func (l *Library) Apply(patch LibraryPatch, now time.Time) {
	if patch.SkinTone != nil {
		l.SkinTone = *patch.SkinTone
	}
	if len(patch.Uses) == 0 {
		return
	}
	order := []string{}
	for _, id := range patch.Recent {
		if _, used := patch.Uses[id]; used {
			order = append(order, id)
		}
	}
	rank := map[string]int{}
	for index, id := range order {
		rank[id] = len(order) - 1 - index
	}
	known := map[string]int{}
	for index, item := range l.EmojiUsage {
		known[item.ID] = index
	}
	keys := make([]string, 0, len(patch.Uses))
	for id := range patch.Uses {
		keys = append(keys, id)
	}
	slices.Sort(keys)
	for _, id := range keys {
		times := patch.Uses[id]
		if !validEmoji(id) || times < 1 {
			continue
		}
		offset, ok := rank[id]
		if !ok {
			offset = len(order)
		}
		item := EmojiUsage{ID: id, Count: min(times, 100), LastUsed: Timestamp(now.Add(-time.Duration(offset) * time.Millisecond))}
		if index, exists := known[id]; exists {
			item.Count = min(1_000_000, item.Count+l.EmojiUsage[index].Count)
			l.EmojiUsage[index] = item
		} else {
			l.EmojiUsage = append(l.EmojiUsage, item)
		}
	}
	score := func(item EmojiUsage) float64 {
		at, _ := time.Parse(time.RFC3339Nano, item.LastUsed)
		return float64(item.Count) * math.Exp2(-max(0, now.Sub(at).Hours())/(24*14))
	}
	slices.SortStableFunc(l.EmojiUsage, func(a, b EmojiUsage) int {
		return cmp.Compare(score(b), score(a))
	})
	l.EmojiUsage = l.EmojiUsage[:min(120, len(l.EmojiUsage))]
}

func (l *Library) SaveGIF(gif model.GIF, now time.Time) {
	l.DeleteGIF(gif.ID)
	l.GIFs = append([]SavedGIF{{GIF: gif, SavedAt: Timestamp(now)}}, l.GIFs...)
	l.GIFs = l.GIFs[:min(200, len(l.GIFs))]
}

func (l *Library) DeleteGIF(id string) {
	l.GIFs = slices.DeleteFunc(l.GIFs, func(gif SavedGIF) bool { return gif.ID == id })
}
