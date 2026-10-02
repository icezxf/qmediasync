package avscrape

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

type MetaTubeClient struct {
	Server string
	HTTP   *http.Client
}

func NewMetaTubeClient(server string) *MetaTubeClient {
	return &MetaTubeClient{Server: strings.TrimRight(server, "/"), HTTP: &http.Client{Timeout: 30 * time.Second}}
}

func (m *MetaTubeClient) Name() string { return "metatube" }

type metaTubeSearchResp struct {
	Data []struct {
		ID          string `json:"id"`
		Number      string `json:"number"`
		Title       string `json:"title"`
		Provider    string `json:"provider"`
		CoverURL    string `json:"cover_url"`
		ThumbURL    string `json:"thumb_url"`
		ReleaseDate string `json:"release_date"`
	} `json:"data"`
}

type metaTubeDetailResp struct {
	Data struct {
		ID            string   `json:"id"`
		Number        string   `json:"number"`
		Title         string   `json:"title"`
		Summary       string   `json:"summary"`
		Provider      string   `json:"provider"`
		Director      string   `json:"director"`
		Actors        []string `json:"actors"`
		ThumbURL      string   `json:"thumb_url"`
		CoverURL      string   `json:"cover_url"`
		BigThumbURL   string   `json:"big_thumb_url"`
		PreviewImages []string `json:"preview_images"`
		PreviewVideo  string   `json:"preview_video_url"`
		Maker         string   `json:"maker"`
		Label         string   `json:"label"`
		Series        string   `json:"series"`
		Genres        []string `json:"genres"`
		Runtime       int      `json:"runtime"`
		ReleaseDate   string   `json:"release_date"`
		Score         float64  `json:"score"`
		Homepage      string   `json:"homepage"`
	} `json:"data"`
}

func (m *MetaTubeClient) Search(code string) ([]*ScrapeResult, error) {
	u := fmt.Sprintf("%s/v1/movies/search?q=%s", m.Server, url.QueryEscape(code))
	resp, err := m.HTTP.Get(u)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var sr metaTubeSearchResp
	if err := json.Unmarshal(body, &sr); err != nil {
		return nil, fmt.Errorf("metatube search decode: %w", err)
	}

	var out []*ScrapeResult
	for _, item := range sr.Data {
		// poster 优先 thumb_url（竖版），兜底 cover_url
		poster := item.ThumbURL
		if poster == "" {
			poster = item.CoverURL
		}
		out = append(out, &ScrapeResult{
			Code:        item.Number,
			Title:       item.Title,
			Poster:      poster,
			ReleaseDate: item.ReleaseDate,
			Source:      "metatube:" + item.Provider,
			HasChinese:  !isJapanese(item.Title) && containsChinese(item.Title),
		})
	}
	return out, nil
}

func (m *MetaTubeClient) Detail(code string, providerID string) (*ScrapeResult, error) {
	u := fmt.Sprintf("%s/v1/movies/%s", m.Server, providerID)
	resp, err := m.HTTP.Get(u)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var dr metaTubeDetailResp
	if err := json.Unmarshal(body, &dr); err != nil {
		return nil, fmt.Errorf("metatube detail decode: %w", err)
	}

	// poster 优先 thumb_url（竖版 ps.jpg），兜底 cover_url（横版 pl.jpg）
	poster := dr.Data.ThumbURL
	if poster == "" {
		poster = dr.Data.CoverURL
	}

	r := &ScrapeResult{
		Code:          dr.Data.Number,
		Title:         dr.Data.Title,
		OriginalTitle: dr.Data.Title,
		Plot:          dr.Data.Summary,
		Runtime:       dr.Data.Runtime,
		ReleaseDate:   dr.Data.ReleaseDate,
		Director:      dr.Data.Director,
		Studio:        dr.Data.Maker,
		Label:         dr.Data.Label,
		Series:        dr.Data.Series,
		Genres:        dr.Data.Genres,
		Poster:        poster,
		Fanart:        dr.Data.BigThumbURL,
		PreviewImages: dr.Data.PreviewImages,
		Trailer:       dr.Data.PreviewVideo,
		Rating:        dr.Data.Score,
		Source:        "metatube:" + dr.Data.Provider,
	}
	if dr.Data.Homepage != "" {
		r.Urls = append(r.Urls, dr.Data.Homepage)
	}
	for _, name := range dr.Data.Actors {
		cleanName := cleanActorName(name)
		if cleanName == "" {
			continue
		}
		r.Actors = append(r.Actors, Actor{Name: cleanName})
	}
	r.HasChinese = !isJapanese(r.Title) && containsChinese(r.Title)
	return r, nil
}

// actorNameBracketRe 去掉演员名里的中文/英文括号补充
// 例："河北彩花（河北彩咖）" → "河北彩花"
var actorNameBracketRe = regexp.MustCompile(`[（(][^）)]*[）)]`)

// cleanActorName 清理演员名
func cleanActorName(name string) string {
	name = strings.TrimSpace(name)
	name = actorNameBracketRe.ReplaceAllString(name, "")
	return strings.TrimSpace(name)
}