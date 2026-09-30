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
	Rating        float64  `json:"rating"`
	Urls          []string `json:"urls"`
	Source        string   `json:"source"`
	HasChinese    bool     `json:"has_chinese"`

	// 统一的图片候选池（按优先级排序），下载时按方向自动分派
	ImageCandidates []string `json:"-"`
}

// Provider 刮削源接口
type Provider interface {
	Name() string
	Search(code string) ([]*ScrapeResult, error)
	Detail(code string, providerID string) (*ScrapeResult, error)
}