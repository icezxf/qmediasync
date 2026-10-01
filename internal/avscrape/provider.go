package avscrape

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
	Rating        float64  `json:"rating"`
	Votes         int      `json:"votes"`
	Urls          []string `json:"urls"`
	Source        string   `json:"source"`
	HasChinese    bool     `json:"has_chinese"`

	Resolution    string   `json:"resolution"`
	IsHDR         bool     `json:"is_hdr"`
	IsUncensored  bool     `json:"is_uncensored"`
	HasChineseSub bool     `json:"has_chinese_sub"`
	ExtraTags     []string `json:"extra_tags"`

	ImageCandidates []string `json:"-"`
}

type Provider interface {
	Name() string
	Search(code string) ([]*ScrapeResult, error)
	Detail(code string, providerID string) (*ScrapeResult, error)
}

// containsChinese 含汉字（宽松），仅用于判断标题/简介里是否含中文
func containsChinese(s string) bool {
	for _, r := range s {
		if r >= 0x4E00 && r <= 0x9FFF {
			return true
		}
	}
	return false
}

// isJapanese 含日文假名
func isJapanese(s string) bool {
	for _, r := range s {
		if (r >= 0x3040 && r <= 0x309F) || (r >= 0x30A0 && r <= 0x30FF) {
			return true
		}
	}
	return false
}

// isChineseName 严格判断是否为中文名
// 规则：
//   - 至少含一个汉字
//   - 不含假名（平假名/片假名）
//   - 不含罗马字母
// 注意：像"岩谷志季"这种纯汉字日文名仍会被误判为中文名，无法完全区分
func isChineseName(s string) bool {
	if s == "" {
		return false
	}
	hasChinese := false
	for _, r := range s {
		// 假名
		if (r >= 0x3040 && r <= 0x309F) || (r >= 0x30A0 && r <= 0x30FF) {
			return false
		}
		// 罗马字母
		if (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') {
			return false
		}
		// 汉字
		if r >= 0x4E00 && r <= 0x9FFF {
			hasChinese = true
		}
	}
	return hasChinese
}
