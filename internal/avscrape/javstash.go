package avscrape

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

type JavStashClient struct {
	Endpoint string
	APIKey   string
	HTTP     *http.Client
}

func NewJavStashClient(endpoint, apiKey string) *JavStashClient {
	return &JavStashClient{Endpoint: endpoint, APIKey: apiKey, HTTP: &http.Client{Timeout: 30 * time.Second}}
}

func (j *JavStashClient) Name() string { return "javstash" }

type gqlRequest struct {
	Query     string                 `json:"query"`
	Variables map[string]interface{} `json:"variables"`
}

type gqlResponse struct {
	Data struct {
		SearchScene []struct {
			ID       string `json:"id"`
			Title    string `json:"title"`
			Code     string `json:"code"`
			Details  string `json:"details"`
			Date     string `json:"date"`
			Duration int    `json:"duration"`
			Studio   *struct {
				Name string `json:"name"`
			} `json:"studio"`
			Performers []struct {
				Performer struct {
					Name    string   `json:"name"`
					Aliases []string `json:"aliases"`
					Birthdate *struct {
						Date string `json:"date"`
					} `json:"birthdate"`
					Country string `json:"country"`
					Height  int    `json:"height"`
					Images  []struct {
						URL string `json:"url"`
					} `json:"images"`
				} `json:"performer"`
			} `json:"performers"`
			Tags []struct {
				Name string `json:"name"`
			} `json:"tags"`
			Images []struct {
				URL string `json:"url"`
			} `json:"images"`
			Urls []struct {
				URL string `json:"url"`
			} `json:"urls"`
		} `json:"searchScene"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

const javStashQuery = `query($term:String!){searchScene(term:$term){id title code details date duration studio{name} performers{performer{name aliases birthdate{date} country height images{url}}} tags{name} images{url} urls{url}}}`

func (j *JavStashClient) do(query string, vars map[string]interface{}) (*gqlResponse, error) {
	payload, _ := json.Marshal(gqlRequest{Query: query, Variables: vars})
	req, _ := http.NewRequest("POST", j.Endpoint, bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	if j.APIKey != "" {
		req.Header.Set("ApiKey", j.APIKey)
	}
	resp, err := j.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var gr gqlResponse
	if err := json.Unmarshal(body, &gr); err != nil {
		return nil, fmt.Errorf("javstash decode: %w", err)
	}
	if len(gr.Errors) > 0 {
		return nil, fmt.Errorf("javstash graphql: %s", gr.Errors[0].Message)
	}
	return &gr, nil
}

func (j *JavStashClient) Search(code string) ([]*ScrapeResult, error) {
	gr, err := j.do(javStashQuery, map[string]interface{}{"term": code})
	if err != nil {
		return nil, err
	}
	var out []*ScrapeResult
	for _, item := range gr.Data.SearchScene {
		r := &ScrapeResult{
			Code:          item.Code,
			Title:         item.Title,
			OriginalTitle: item.Title,
			Plot:          item.Details,
			ReleaseDate:   item.Date,
			Runtime:       item.Duration / 60,
			Source:        "javstash",
			HasChinese:    !isJapanese(item.Title) && containsChinese(item.Title),
		}
		if item.Studio != nil {
			r.Studio = item.Studio.Name
		}
		for _, t := range item.Tags {
			r.Genres = append(r.Genres, t.Name)
		}
		if len(item.Images) > 0 {
			r.Fanart = item.Images[0].URL
		}
		for _, p := range item.Performers {
			// 保留主名字，不覆盖。aliases 全部存进 Actor.Aliases
			// 中文名由后续维基百科或 aliases 交叉匹配处理
			a := Actor{
				Name:    p.Performer.Name,
				Aliases: p.Performer.Aliases,
				Country: p.Performer.Country,
				Height:  p.Performer.Height,
			}
			if p.Performer.Birthdate != nil {
				a.Birthday = p.Performer.Birthdate.Date
			}
			if len(p.Performer.Images) > 0 {
				a.Image = p.Performer.Images[0].URL
			}
			r.Actors = append(r.Actors, a)
		}
		for _, u := range item.Urls {
			r.Urls = append(r.Urls, u.URL)
		}
		out = append(out, r)
	}
	return out, nil
}

func (j *JavStashClient) Detail(code string, providerID string) (*ScrapeResult, error) {
	results, err := j.Search(code)
	if err != nil {
		return nil, err
	}
	if len(results) == 0 {
		return nil, fmt.Errorf("javstash: no result for %s", code)
	}
	return results[0], nil
}
