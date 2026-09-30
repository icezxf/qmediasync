package avscrape

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"Q115-STRM/internal/helpers"
)

type Translator struct {
	Engine     string
	Target     string
	DeepLKey   string
	BingKey    string
	BingRegion string
	HTTP       *http.Client
}

func NewTranslator(engine, target string) *Translator {
	if target == "" {
		target = "zh"
	}
	return &Translator{
		Engine: engine,
		Target: target,
		HTTP:   &http.Client{Timeout: 20 * time.Second},
	}
}

func (t *Translator) Translate(text string) (string, error) {
	if text == "" {
		return "", nil
	}
	var result string
	var err error
	switch t.Engine {
	case "deepl":
		result, err = t.deepl(text)
	case "bing":
		result, err = t.bing(text)
	case "google_free":
		result, err = t.googleFree(text)
	case "mymemory":
		result, err = t.myMemory(text)
	default:
		return text, nil
	}
	if err != nil {
		helpers.AppLogger.Warnf("[翻译] 失败(%s): %v", t.Engine, err)
		return text, err
	}
	return result, nil
}

// deepl DeepL 官方 API
func (t *Translator) deepl(text string) (string, error) {
	if t.DeepLKey == "" {
		return text, fmt.Errorf("DeepL API Key 未配置")
	}
	// 免费版 key 以 :fx 结尾，走 api-free.deepl.com
	endpoint := "https://api.deepl.com/v2/translate"
	if strings.HasSuffix(t.DeepLKey, ":fx") {
		endpoint = "https://api-free.deepl.com/v2/translate"
	}

	// DeepL 目标语言：ZH = 简体中文
	targetLang := "ZH"
	switch t.Target {
	case "zh", "zh-Hans", "zh-CN":
		targetLang = "ZH"
	case "zh-Hant", "zh-TW":
		targetLang = "ZH-HANT"
	case "en":
		targetLang = "EN-US"
	case "ja":
		targetLang = "JA"
	}

	form := url.Values{}
	form.Set("text", text)
	form.Set("target_lang", targetLang)
	// 不指定 source_lang，让 DeepL 自动检测

	req, _ := http.NewRequest("POST", endpoint, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Authorization", "DeepL-Auth-Key "+t.DeepLKey)

	resp, err := t.HTTP.Do(req)
	if err != nil {
		return text, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		return text, fmt.Errorf("DeepL HTTP %d: %s", resp.StatusCode, string(body))
	}

	var out struct {
		Translations []struct {
			DetectedSourceLanguage string `json:"detected_source_language"`
			Text                   string `json:"text"`
		} `json:"translations"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return text, err
	}
	if len(out.Translations) > 0 {
		return out.Translations[0].Text, nil
	}
	return text, nil
}

// bing 微软必应翻译
func (t *Translator) bing(text string) (string, error) {
	if t.BingKey == "" {
		return text, fmt.Errorf("Bing API Key 未配置")
	}
	endpoint := "https://api.cognitive.microsofttranslator.com/translate"
	u, _ := url.Parse(endpoint)
	q := u.Query()
	q.Set("api-version", "3.0")
	q.Set("from", "ja")
	q.Set("to", "zh-Hans")
	u.RawQuery = q.Encode()

	body, _ := json.Marshal([]map[string]string{{"Text": text}})
	req, _ := http.NewRequest("POST", u.String(), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Ocp-Apim-Subscription-Key", t.BingKey)
	req.Header.Set("Ocp-Apim-Subscription-Region", t.BingRegion)

	resp, err := t.HTTP.Do(req)
	if err != nil {
		return text, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		return text, fmt.Errorf("Bing HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	var result []struct {
		Translations []struct {
			Text string `json:"text"`
			To   string `json:"to"`
		} `json:"translations"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return text, err
	}
	if len(result) > 0 && len(result[0].Translations) > 0 {
		return result[0].Translations[0].Text, nil
	}
	return text, nil
}

// googleFree
func (t *Translator) googleFree(text string) (string, error) {
	u := fmt.Sprintf("https://translate.googleapis.com/translate_a/single?client=gtx&sl=auto&tl=%s&dt=t&q=%s",
		t.Target, url.QueryEscape(text))
	resp, err := t.HTTP.Get(u)
	if err != nil {
		return text, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var raw []interface{}
	if err := json.Unmarshal(body, &raw); err != nil {
		return text, err
	}
	if len(raw) == 0 {
		return text, nil
	}
	segments, ok := raw[0].([]interface{})
	if !ok {
		return text, nil
	}
	var sb strings.Builder
	for _, seg := range segments {
		if arr, ok := seg.([]interface{}); ok && len(arr) > 0 {
			if s, ok := arr[0].(string); ok {
				sb.WriteString(s)
			}
		}
	}
	return sb.String(), nil
}

// myMemory
func (t *Translator) myMemory(text string) (string, error) {
	langPair := "ja|zh-CN"
	u := fmt.Sprintf("https://api.mymemory.translated.net/get?q=%s&langpair=%s",
		url.QueryEscape(text), url.QueryEscape(langPair))
	resp, err := t.HTTP.Get(u)
	if err != nil {
		return text, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var out struct {
		ResponseData struct {
			TranslatedText string `json:"translatedText"`
		} `json:"responseData"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return text, err
	}
	if out.ResponseData.TranslatedText == "" {
		return text, fmt.Errorf("MyMemory 返回空")
	}
	return out.ResponseData.TranslatedText, nil
}

// TranslateResult 翻译整个 ScrapeResult
func (t *Translator) TranslateResult(r *ScrapeResult) {
	if r == nil {
		return
	}
	if s, err := t.Translate(r.Title); err == nil && s != "" {
		r.Title = s
		helpers.AppLogger.Infof("[翻译] 标题 -> %s", s)
	}
	if s, err := t.Translate(r.Plot); err == nil && s != "" {
		r.Plot = s
		helpers.AppLogger.Infof("[翻译] 简介 -> %s", truncate(s, 50))
	}
	for i, g := range r.Genres {
		if s, err := t.Translate(g); err == nil && s != "" {
			r.Genres[i] = s
		}
	}
	// 翻译演员名（不含中文的才翻）
	for i := range r.Actors {
		if !containsChinese(r.Actors[i].Name) {
			if s, err := t.Translate(r.Actors[i].Name); err == nil && s != "" && s != r.Actors[i].Name {
				r.Actors[i].Name = s
			}
		}
	}
}

func truncate(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "..."
}