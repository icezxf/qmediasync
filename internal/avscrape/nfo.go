package avscrape

import (
	"encoding/json"
	"fmt"
	"strings"

	"Q115-STRM/internal/models"
)

// GenerateNFO 生成 NFO 文本
func GenerateNFO(r *ScrapeResult) string {
	if r == nil {
		return ""
	}
	var sb strings.Builder
	sb.WriteString(`<?xml version="1.0" encoding="UTF-8" ?>` + "\n")
	sb.WriteString("<movie>\n")
	sb.WriteString(fmt.Sprintf("  <title><![CDATA[%s]]></title>\n", r.Title))
	sb.WriteString(fmt.Sprintf("  <originaltitle><![CDATA[%s]]></originaltitle>\n", r.OriginalTitle))
	sb.WriteString(fmt.Sprintf("  <sorttitle><![CDATA[%s]]></sorttitle>\n", r.Code))
	sb.WriteString(fmt.Sprintf("  <num>%s</num>\n", r.Code))
	sb.WriteString(fmt.Sprintf("  <uniqueid type=\"num\" default=\"true\">%s</uniqueid>\n", r.Code))

	if r.ReleaseDate != "" {
		if len(r.ReleaseDate) >= 4 {
			sb.WriteString(fmt.Sprintf("  <year>%s</year>\n", r.ReleaseDate[:4]))
		}
		sb.WriteString(fmt.Sprintf("  <premiered>%s</premiered>\n", r.ReleaseDate))
		sb.WriteString(fmt.Sprintf("  <releasedate>%s</releasedate>\n", r.ReleaseDate))
	}

	if r.Plot != "" {
		sb.WriteString(fmt.Sprintf("  <plot><![CDATA[%s]]></plot>\n", r.Plot))
		sb.WriteString(fmt.Sprintf("  <outline><![CDATA[%s]]></outline>\n", r.Plot))
	}
	if r.Runtime > 0 {
		sb.WriteString(fmt.Sprintf("  <runtime>%d</runtime>\n", r.Runtime))
	}
	if r.Director != "" {
		sb.WriteString(fmt.Sprintf("  <director><![CDATA[%s]]></director>\n", r.Director))
	}
	if r.Studio != "" {
		sb.WriteString(fmt.Sprintf("  <studio><![CDATA[%s]]></studio>\n", r.Studio))
	}
	if r.Label != "" {
		sb.WriteString(fmt.Sprintf("  <label><![CDATA[%s]]></label>\n", r.Label))
	}
	if r.Series != "" {
		sb.WriteString(fmt.Sprintf("  <series><![CDATA[%s]]></series>\n", r.Series))
	}
	// 多演员时输出"共演"作为系列
	if len(r.Actors) >= 2 {
		sb.WriteString("  <series>共演</series>\n")
	}

	sb.WriteString("  <country>JP</country>\n")
	sb.WriteString("  <mpaa>JP-18+</mpaa>\n")
	sb.WriteString("  <customrating>JP-18+</customrating>\n")

	if r.Rating > 0 {
		sb.WriteString(fmt.Sprintf("  <rating>%.1f</rating>\n", r.Rating))
		sb.WriteString(fmt.Sprintf("  <criticrating>%.1f</criticrating>\n", r.Rating*10))
		sb.WriteString("  <ratings>\n")
		sb.WriteString("    <rating name=\"javdb\" max=\"10\" default=\"true\">\n")
		sb.WriteString(fmt.Sprintf("      <value>%.1f</value>\n", r.Rating))
		if r.Votes > 0 {
			sb.WriteString(fmt.Sprintf("      <votes>%d</votes>\n", r.Votes))
		} else {
			sb.WriteString("      <votes/>\n")
		}
		sb.WriteString("    </rating>\n")
		sb.WriteString("  </ratings>\n")
	}
	if r.Votes > 0 {
		sb.WriteString(fmt.Sprintf("  <votes>%d</votes>\n", r.Votes))
	} else {
		sb.WriteString("  <votes/>\n")
	}

	sb.WriteString("  <poster>poster.jpg</poster>\n")
	sb.WriteString("  <thumb>thumb.jpg</thumb>\n")
	sb.WriteString("  <fanart>fanart.jpg</fanart>\n")

	if r.Trailer != "" {
		sb.WriteString(fmt.Sprintf("  <trailer>%s</trailer>\n", r.Trailer))
	}

	for _, g := range r.Genres {
		sb.WriteString(fmt.Sprintf("  <genre><![CDATA[%s]]></genre>\n", g))
		sb.WriteString(fmt.Sprintf("  <tag><![CDATA[%s]]></tag>\n", g))
	}

	// 多演员时输出"共演"合集
	if len(r.Actors) >= 2 {
		sb.WriteString("  <set>\n")
		sb.WriteString("    <name>共演</name>\n")
		sb.WriteString("  </set>\n")
	}

	for _, a := range r.Actors {
		sb.WriteString("  <actor>\n")
		sb.WriteString(fmt.Sprintf("    <name><![CDATA[%s]]></name>\n", a.Name))
		sb.WriteString("    <type>Actor</type>\n")
		if a.Role != "" {
			sb.WriteString(fmt.Sprintf("    <role><![CDATA[%s]]></role>\n", a.Role))
		}
		if a.Image != "" {
			sb.WriteString(fmt.Sprintf("    <thumb>%s</thumb>\n", a.Image))
		}
		if a.Birthday != "" {
			sb.WriteString(fmt.Sprintf("    <birthday>%s</birthday>\n", a.Birthday))
		}
		if a.Country != "" {
			sb.WriteString(fmt.Sprintf("    <country>%s</country>\n", a.Country))
		}
		if a.Height > 0 {
			sb.WriteString(fmt.Sprintf("    <height>%d</height>\n", a.Height))
		}
		sb.WriteString("  </actor>\n")
	}
	for _, u := range r.Urls {
		sb.WriteString(fmt.Sprintf("  <website>%s</website>\n", u))
	}
	sb.WriteString("</movie>\n")
	return sb.String()
}

// MediaFromResult
func MediaFromResult(r *ScrapeResult) *models.AVMedia {
	if r == nil {
		return nil
	}
	genres, _ := json.Marshal(r.Genres)
	actors, _ := json.Marshal(r.Actors)
	previews, _ := json.Marshal(r.PreviewImages)
	urls, _ := json.Marshal(r.Urls)
	return &models.AVMedia{
		Code:          r.Code,
		Title:         r.Title,
		OriginalTitle: r.OriginalTitle,
		Plot:          r.Plot,
		Runtime:       r.Runtime,
		ReleaseDate:   r.ReleaseDate,
		Director:      r.Director,
		Studio:        r.Studio,
		Label:         r.Label,
		Series:        r.Series,
		Genres:        string(genres),
		Actors:        string(actors),
		Poster:        r.Poster,
		Fanart:        r.Fanart,
		PreviewImages: string(previews),
		Trailer:       r.Trailer,
		Rating:        r.Rating,
		Urls:          string(urls),
		NFOContent:    GenerateNFO(r),
		Source:        r.Source,
	}
}
