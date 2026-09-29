package avscrape

import (
"encoding/json"
"fmt"
"io"
"net/http"
"net/url"
"strings"
"time"
)

type Translator struct {
Engine string
Target string
HTTP   *http.Client
}
func NewTranslator(engine, target string) *Translator {
if target == "" {
target = "zh"
}
return &Translator{Engine: engine, Target: target, HTTP: &http.Client{Timeout: 15 * time.Second}}
}
func (t *Translator) Translate(text string) (string, error) {
if text == "" {
return "", nil
}
switch t.Engine {
case "google_free":
return t.googleFree(text)
case "libretranslate":
return t.libretranslate(text)
default:
return text, nil
}
}
func (t *Translator) googleFree(text string) (string, error) {
u := fmt.Sprintf("https://translate.googleapis.com/translate_a/single?client=gtx&sl=auto&tl=%s&dt=t&q=%s", t.Target, url.QueryEscape(text))
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
func (t *Translator) libretranslate(text string) (string, error) {
endpoint := "http://localhost:5000/translate"
payload := map[string]string{"q": text, "source": "auto", "target": t.Target, "format": "text"}
data, _ := json.Marshal(payload)
resp, err := t.HTTP.Post(endpoint, "application/json", strings.NewReader(string(data)))
if err != nil {
return text, err
}
defer resp.Body.Close()
body, _ := io.ReadAll(resp.Body)
var out struct{ TranslatedText string `json:"translatedText"` }
if err := json.Unmarshal(body, &out); err != nil {
return text, err
}
return out.TranslatedText, nil
}
func (t *Translator) TranslateResult(r *ScrapeResult) {
if r == nil {
return
}
if s, err := t.Translate(r.Title); err == nil && s != "" {
r.Title = s
}
if s, err := t.Translate(r.Plot); err == nil && s != "" {
r.Plot = s
}
for i, g := range r.Genres {
if s, err := t.Translate(g); err == nil && s != "" {
r.Genres[i] = s
}
}
}
