package api

import (
	"context"
	_ "embed"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/danielgtaylor/huma/v2"
)

const (
	appEmojiPath     = "/api/v1/app/emoji/catalog"
	appSpeechRoot    = "/api/v1/app/speech/zonos3/"
	sevenTVGlobalURL = "https://api.7tv.app/v3/emote-sets/global"
)

func publicAppRead(method, path string) bool {
	if path == appEmojiPath {
		return method == http.MethodGet
	}
	if method != http.MethodGet && method != http.MethodHead {
		return false
	}
	suffix, found := strings.CutPrefix(path, appSpeechRoot)
	return found && strings.Count(suffix, "/") == 3 && strings.Contains(suffix, "/models/")
}

type AppEmoji struct {
	ID        string   `json:"id"`
	Label     string   `json:"label"`
	Shortcode string   `json:"shortcode"`
	Aliases   []string `json:"aliases" nullable:"false"`
	Group     string   `json:"group" enum:"workspace,7tv"`
	URL       string   `json:"url" doc:"Preview URL or path to a bundled image."`
	StaticURL string   `json:"staticUrl" doc:"URL or path to a still image."`
	Source    string   `json:"source" enum:"workspace,7tv"`
	SourceURL string   `json:"sourceUrl,omitempty"`
	Creator   string   `json:"creator,omitempty"`
}

type AppEmojiCatalog struct {
	Items       []AppEmoji `json:"items" nullable:"false" doc:"Bundled workspace emoji and the cached 7TV global set."`
	StandardURL string     `json:"standardUrl" doc:"Path to the Unicode catalog bundled with the frontend."`
	Sources     []string   `json:"sources" nullable:"false"`
}

type appEmojiOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         AppEmojiCatalog
}

//go:embed assets/7tv-global.json
var bundledSevenTV []byte

var (
	sevenTVName = regexp.MustCompile("^[A-Za-z0-9_-]{1,64}$")
	sevenTVFile = regexp.MustCompile("^[A-Za-z0-9_-]{1,64}\\.webp$")
	shardFile   = regexp.MustCompile("^shard-([0-9]{3})\\.bin$")
	sevenTVHTTP = &http.Client{
		Timeout:       5 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
	}
	bundledEmoji = sync.OnceValue(func() []AppEmoji {
		var items []AppEmoji
		if err := json.Unmarshal(bundledSevenTV, &items); err != nil {
			panic("invalid bundled 7TV catalog")
		}
		return items
	})
)

type appEmojiCache struct {
	mu      sync.Mutex
	items   []AppEmoji
	expires time.Time
	fetch   func(context.Context) ([]AppEmoji, bool)
}

func (c *appEmojiCache) get(ctx context.Context) []AppEmoji {
	c.mu.Lock()
	defer c.mu.Unlock()
	if time.Now().Before(c.expires) {
		return c.items
	}
	fetch := c.fetch
	if fetch == nil {
		fetch = readSevenTV
	}
	// The upstream request has a five-second deadline. One canceled browser
	// request must not discard the shared refresh for the other clients.
	items, ok := fetch(context.WithoutCancel(ctx))
	if !ok {
		items = bundledEmoji()
	}
	c.items = items
	c.expires = time.Now().Add(time.Hour)
	return items
}

func readSevenTV(ctx context.Context) ([]AppEmoji, bool) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, sevenTVGlobalURL, nil)
	if err != nil {
		return nil, false
	}
	response, err := sevenTVHTTP.Do(request)
	if err != nil {
		return nil, false
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, false
	}
	const maxResponse = 2 << 20
	data, err := io.ReadAll(io.LimitReader(response.Body, maxResponse+1))
	if err != nil || len(data) > maxResponse {
		return nil, false
	}
	return decodeSevenTV(data)
}

func decodeSevenTV(data []byte) ([]AppEmoji, bool) {
	var response struct {
		Emotes []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
			Data struct {
				Owner struct {
					Name string `json:"display_name"`
				} `json:"owner"`
				Host struct {
					URL   string `json:"url"`
					Files []struct {
						Name       string `json:"name"`
						StaticName string `json:"static_name"`
					} `json:"files"`
				} `json:"host"`
			} `json:"data"`
		} `json:"emotes"`
	}
	if err := json.Unmarshal(data, &response); err != nil || response.Emotes == nil {
		return nil, false
	}
	items := []AppEmoji{}
	for _, item := range response.Emotes {
		if !sevenTVName.MatchString(item.Name) {
			continue
		}
		if !sevenTVName.MatchString(item.ID) {
			return nil, false
		}
		host := item.Data.Host
		base, err := url.Parse("https:" + host.URL)
		if err != nil || !strings.EqualFold(base.Host, "cdn.7tv.app") || base.User != nil ||
			base.Path != "/emote/"+item.ID || base.RawQuery != "" || base.Fragment != "" {
			return nil, false
		}
		name, still := "", ""
		for _, file := range host.Files {
			if file.Name == "2x.webp" || (file.Name == "1x.webp" && name == "") {
				name, still = file.Name, file.StaticName
			}
		}
		if name == "" || (still != "" && !sevenTVFile.MatchString(still)) {
			return nil, false
		}
		if still == "" {
			still = name
		}
		items = append(items, AppEmoji{
			ID: "7tv:" + item.ID, Label: item.Name, Shortcode: item.Name, Aliases: []string{}, Group: "7tv",
			URL: base.String() + "/" + name, StaticURL: base.String() + "/" + still,
			Source: "7tv", SourceURL: "https://7tv.app/emotes/" + item.ID, Creator: item.Data.Owner.Name,
		})
		if len(items) == 100 {
			break
		}
	}
	return items, true
}

type speechShardInput struct {
	Version string `path:"version" maxLength:"16" doc:"The version of the frontend's bundled model manifests."`
	Model   string `path:"model" maxLength:"24" doc:"The backbone or zac model."`
	File    string `path:"file" maxLength:"32" doc:"Name of a shard listed in the shipped model manifest."`
}

type speechShardRedirect struct {
	Location       string `header:"Location" doc:"The shard URL at the pinned model revision."`
	CacheControl   string `header:"Cache-Control"`
	ReferrerPolicy string `header:"Referrer-Policy"`
}

func speechShard(in *speechShardInput) (*speechShardRedirect, error) {
	// The v1 manifests list contiguous shard indexes. Accept only their exact names.
	count := map[string]int{"backbone": 11, "zac": 5}[in.Model]
	match := shardFile.FindStringSubmatch(in.File)
	if in.Version != "v1" || count == 0 || match == nil {
		return nil, huma.Error404NotFound("Model shard not found.")
	}
	index, _ := strconv.Atoi(match[1])
	if index >= count {
		return nil, huma.Error404NotFound("Model shard not found.")
	}
	const hub = "https://huggingface.co/Zyphra/ZONOS3-Mini/resolve/d2d142a86b1e6aeb4252679673c86ecd86177bf8/web/"
	return &speechShardRedirect{
		Location: hub + in.Model + "/" + in.File, CacheControl: "no-store", ReferrerPolicy: "no-referrer",
	}, nil
}

func (s *Server) registerAppPublic() {
	huma.Register(s.API, huma.Operation{
		Method: http.MethodGet, Path: appEmojiPath, OperationID: "get-app-emoji-catalog",
		Summary: "Read the public emoji catalog", Errors: []int{http.StatusForbidden},
		Description: "Lists bundled workspace emoji and up to 100 entries from the cached 7TV global set. Uses a bundled snapshot when 7TV is unavailable.",
	}, func(ctx context.Context, _ *struct{}) (*appEmojiOutput, error) {
		workspace := AppEmoji{
			ID: "workspace:ircord", Label: "IRCord", Shortcode: "ircord",
			Aliases: []string{"chat", "home"}, Group: "workspace", URL: "/favicon.svg",
			StaticURL: "/favicon.svg", Source: "workspace",
		}
		items := append([]AppEmoji{workspace}, s.appEmoji.get(ctx)...)
		return &appEmojiOutput{CacheControl: "public, max-age=600", Body: AppEmojiCatalog{
			Items: items, StandardURL: "/data/emoji-v18.json", Sources: []string{"workspace", "7tv"},
		}}, nil
	})
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		huma.Register(s.API, huma.Operation{
			Method: method, Path: appSpeechRoot + "{version}/models/{model}/{file}",
			OperationID: strings.ToLower(method) + "-app-speech-shard", DefaultStatus: http.StatusFound,
			Summary:     "Redirect to a public read-aloud model shard",
			Description: "Redirects a recognized shard name to the pinned public model release. Unknown names return 404.",
			Errors:      []int{http.StatusForbidden, http.StatusNotFound},
		}, func(_ context.Context, in *speechShardInput) (*speechShardRedirect, error) {
			return speechShard(in)
		})
	}
}
