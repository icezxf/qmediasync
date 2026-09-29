package avscrape

import (
"io"
"net/http"
"time"
)

var httpClient = &http.Client{Timeout: 30 * time.Second}

func downloadImage(url string) ([]byte, error) {
resp, err := httpClient.Get(url)
if err != nil {
return nil, err
}
defer resp.Body.Close()
return io.ReadAll(resp.Body)
}
func now() time.Time { return time.Now() }
