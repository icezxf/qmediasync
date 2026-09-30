package avscrape

// Actor 演员信息
type Actor struct {
	Name     string   `json:"name"`
	Aliases  []string `json:"aliases,omitempty"`
	Role     string   `json:"role,omitempty"`
	Thumb    string   `json:"thumb,omitempty"`
	Birthday string   `json:"birthday,omitempty"`
	Country  string   `json:"country,omitempty"`
	Height   int      `json:"height,omitempty"`
	Image    string   `json:"image,omitempty"`
}

// ScrapeResult 统一刮削结果
type ScrapeResult struct {
	Code          string   `json:"code"`
	Title         string   `json:"title"`
	OriginalTitle string   `json:"original_title"`
	Plot          string   `json:"plot"`
	Runtime       int      `json:"runtime"`
	ReleaseDate   string   `json:"release_date"`
	Director      string   `json:"director"`
	Studio        string   `json:"studio"`
	Label         string   `json:"label"`
	Series        string   `json:"series"`
	Genres        []string `json:"genres"`
	Actors        []Actor  `json:"actors"`
	Poster        string   `json:"poster"`
	Fanart        string   `json:"fanart"`
	PreviewImages []string `json:"preview_images"`
	Trailer       string   `json:"trailer"`
	Rating        float64  `json:"rating"`  // JavDB 评分转 10 分制（如 8.8）
	Votes         int      `json:"votes"`   // 评分人数
	Urls          []string `json:"urls"`
	Source        string   `json:"source"`
	HasChinese    bool     `json:"has_chinese"`

	ImageCandidates []string `json:"-"`
}

// Provider 刮削源接口
type Provider interface {
	Name() string
	Search(code string) ([]*ScrapeResult, error)
	Detail(code string, providerID string) (*ScrapeResult, error)
}

func containsChinese(s string) bool {
	for _, r := range s {
		if r >= 0x4E00 && r <= 0x9FFF {
			return true
		}
	}
	return false
}

func isJapanese(s string) bool {
	for _, r := range s {
		if (r >= 0x3040 && r <= 0x309F) || (r >= 0x30A0 && r <= 0x30FF) {
			return true
		}
	}
	return false
}
