package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// 91CRDJ（91crdj.com）原生站源：成人短剧 / 漫剧 / 真人剧 / 视频，MacCMS 风格 HTML。
//
// 上游页面（实测 2026-09）：
//   GET /                      首页
//   GET /{cat}/                分类第 1 页（duanju / manju / zhenrenju / shipin）
//   GET /{cat}/page/{n}/       分类翻页
//   GET /search/?keyword=      搜索
//   GET /{cat}/{id}-{slug}/    详情（h1.detail-title、#viBody、.ep-grid 分集）
//   GET /{cat}/{id}-{slug}/{ep}/          播放页（playerInitialData.current.src）
//   GET /videos/{id}/episodes/{ep}/playback  播放 JSON（data.src = 签名 m3u8）
//
// 站点经 Cloudflare；列表卡片 <a class="card" href data-track-item-name>，封面为带签名直链。

const (
	crjSiteBaseURL = "https://91crdj.com"
	crjUserAgent   = "Mozilla/5.0 (Linux; Android 13) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Mobile Safari/537.36"
)

var crjCategories = []nativeCategory{
	{ID: "duanju", Name: "风月短剧"},
	{ID: "manju", Name: "风月漫剧"},
	{ID: "zhenrenju", Name: "真人剧"},
	{ID: "shipin", Name: "风月视频"},
}

var (
	reCrjCard      = regexp.MustCompile(`(?is)<a\b[^>]*class="[^"]*\bcard\b[^"]*"[^>]*>.*?</a>`)
	reCrjCardHref  = regexp.MustCompile(`(?is)href="([^"]+)"`)
	reCrjCardName  = regexp.MustCompile(`(?is)data-track-item-name="([^"]*)"`)
	reCrjCardH3    = regexp.MustCompile(`(?is)<h3[^>]*>(.*?)</h3>`)
	reCrjCardImg   = regexp.MustCompile(`(?is)<img\b[^>]*>`)
	reCrjCardSrc   = regexp.MustCompile(`(?is)data-src="([^"]+)"`)
	reCrjCardSrc2  = regexp.MustCompile(`(?is)\ssrc="([^"]+)"`)
	reCrjCardFlag  = regexp.MustCompile(`(?is)<span[^>]+class="[^"]*eps-flag[^"]*"[^>]*>(.*?)</span>`)
	reCrjPageNum   = regexp.MustCompile(`/page/(\d+)/`)
	reCrjTitle     = regexp.MustCompile(`(?is)<h1[^>]*class="[^"]*detail-title[^"]*"[^>]*>(.*?)</h1>`)
	reCrjIntro     = regexp.MustCompile(`(?is)<p[^>]*id="viBody"[^>]*>(.*?)</p>`)
	reCrjPoster    = regexp.MustCompile(`(?is)<div[^>]*class="[^"]*d-poster[^"]*"[^>]*>.*?<img\b[^>]*>`)
	reCrjPublished = regexp.MustCompile(`(?is)<dt>\s*发布\s*</dt>\s*<dd>.*?datetime="([^"]+)`)
	reCrjStatus    = regexp.MustCompile(`(?is)<dt>\s*状态\s*</dt>\s*<dd>(.*?)</dd>`)
	reCrjTags      = regexp.MustCompile(`(?is)<div[^>]+class="[^"]*d-tags[^"]*"[^>]*>(.*?)</div>`)
	reCrjAnchorTxt = regexp.MustCompile(`(?is)<a[^>]*>(.*?)</a>`)
	reCrjEpGrid    = regexp.MustCompile(`(?is)<div[^>]+class="[^"]*ep-grid[^"]*"[^>]*>(.*?)</div>`)
	reCrjEpLink    = regexp.MustCompile(`(?is)<a[^>]+href="([^"]+)"[^>]*>(.*?)</a>`)
	reCrjTag       = regexp.MustCompile(`<[^>]+>`)
	reCrjEpPath    = regexp.MustCompile(`/(\d+)-[^/]+/(\d+)/?$`)
	reCrjVideosEp  = regexp.MustCompile(`/videos/(\d+)/episodes/(\d+)/playback`)
)

func validCrjCategory(category string) bool {
	if category == "" {
		return true
	}
	if len(category) > 16 || strings.ContainsAny(category, "|/\\\x00\r\n") {
		return false
	}
	for _, entry := range crjCategories {
		if entry.ID == category {
			return true
		}
	}
	return false
}

func crjClean(text string) string {
	if text == "" {
		return ""
	}
	text = reCrjTag.ReplaceAllString(text, "")
	text = guipianUnescape(text)
	return strings.Join(strings.Fields(strings.ReplaceAll(text, "\u3000", " ")), " ")
}

func crjFixURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if strings.HasPrefix(raw, "//") {
		raw = "https:" + raw
	} else if strings.HasPrefix(raw, "/") {
		raw = crjSiteBaseURL + raw
	}
	parsed, err := url.Parse(raw)
	if err != nil || !validNativeCoverURL(parsed) {
		return ""
	}
	return parsed.String()
}

// crjImgSrc 从 <img> 标签提取真实封面：优先 data-src，其次 src（跳过 base64 占位）。
func crjImgSrc(imgTag string) string {
	if m := reCrjCardSrc.FindStringSubmatch(imgTag); len(m) > 1 {
		return m[1]
	}
	if m := reCrjCardSrc2.FindStringSubmatch(imgTag); len(m) > 1 && !strings.HasPrefix(m[1], "data:") {
		return m[1]
	}
	return ""
}

func (d *Downloader) fetchCrjCategories() []nativeCategory {
	return append([]nativeCategory(nil), crjCategories...)
}

func (d *Downloader) crjGet(ctx context.Context, path string) (string, error) {
	site := d.providerBaseURL(source91crj)
	address := path
	if !strings.HasPrefix(address, "http") {
		address = site + path
	}
	pageContext := context.WithValue(ctx, providerTextUserAgentKey{}, crjUserAgent)
	return d.fetchProviderText(pageContext, address, site+"/")
}

// parseCrjCards 解析 card 列表。
func parseCrjCards(body string) []Drama {
	items := make([]Drama, 0, 32)
	seen := map[string]bool{}
	for _, match := range reCrjCard.FindAllString(body, -1) {
		href := ""
		if m := reCrjCardHref.FindStringSubmatch(match); len(m) > 1 {
			href = m[1]
		}
		name := ""
		if m := reCrjCardName.FindStringSubmatch(match); len(m) > 1 {
			name = crjClean(m[1])
		}
		if name == "" {
			if m := reCrjCardH3.FindStringSubmatch(match); len(m) > 1 {
				name = crjClean(m[1])
			}
		}
		playURL := crjDetailURL(href)
		if playURL == "" || name == "" || seen[playURL] {
			continue
		}
		seen[playURL] = true
		cover := ""
		if im := reCrjCardImg.FindStringSubmatch(match); len(im) > 0 {
			cover = crjFixURL(crjImgSrc(im[0]))
		}
		remark := ""
		if m := reCrjCardFlag.FindStringSubmatch(match); len(m) > 1 {
			remark = crjClean(m[1])
		}
		id := crjSourceID(playURL)
		if id == "" {
			continue
		}
		items = append(items, Drama{
			ID: providerDramaID(source91crj, id), Source: source91crj, SourceID: id,
			Title: truncate(name, 512), Name: truncate(name, 512),
			Cover: cover, CoverURL: cover, ChannelName: "91CRDJ",
			CategoryName: "成人短剧", Remark: truncate(remark, 64),
		})
		if len(items) > 500 {
			break
		}
	}
	return items
}

// crjDetailURL 校验并归一详情/播放页 URL，限定同源。
func crjDetailURL(href string) string {
	href = strings.TrimSpace(href)
	if href == "" {
		return ""
	}
	if !strings.HasPrefix(href, "http") {
		href = crjSiteBaseURL + "/" + strings.TrimPrefix(href, "/")
	}
	parsed, err := url.Parse(href)
	if err != nil || parsed.User != nil {
		return ""
	}
	host := strings.ToLower(parsed.Hostname())
	if host != "91crdj.com" && host != "www.91crdj.com" {
		return ""
	}
	if !strings.HasSuffix(parsed.Path, "/") {
		parsed.Path += "/"
	}
	return parsed.String()
}

// crjSourceID 从详情 URL 提取稳定 ID（"{cat}/{num}-{slug}"）。
func crjSourceID(detailURL string) string {
	parsed, err := url.Parse(detailURL)
	if err != nil {
		return ""
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(parts) < 2 {
		return ""
	}
	return parts[0] + "/" + parts[1]
}

func crjPageCount(body, category string, page, count int) int {
	maxPage := page
	pattern := regexp.MustCompile(fmt.Sprintf(`(?is)/%s/page/(\d+)/`, regexp.QuoteMeta(category)))
	for _, match := range pattern.FindAllStringSubmatch(body, -1) {
		if value, err := strconv.Atoi(match[1]); err == nil && value > maxPage && value < 100000 {
			maxPage = value
		}
	}
	if maxPage > page {
		return maxPage
	}
	if count >= 24 {
		return page + 1
	}
	return page
}

func (d *Downloader) fetchCrjCatalogPage(ctx context.Context, page int, category, query string) ([]Drama, bool, error) {
	if page < 1 || page > 100000 {
		return nil, false, errors.New("91CRDJ 目录页码无效")
	}
	if query != "" {
		return d.searchCrj(ctx, query)
	}
	if !validCrjCategory(category) {
		return nil, false, errors.New("91CRDJ 内容分类无效")
	}
	if category == "" {
		category = crjCategories[0].ID
	}
	path := "/" + category + "/"
	if page > 1 {
		path = fmt.Sprintf("/%s/page/%d/", category, page)
	}
	body, err := d.crjGet(ctx, path)
	if err != nil {
		return nil, false, err
	}
	items := parseCrjCards(body)
	if len(items) == 0 {
		return []Drama{}, false, nil
	}
	pageCount := crjPageCount(body, category, page, len(items))
	return items, page < pageCount, nil
}

func (d *Downloader) fetchCrjDetail(ctx context.Context, sourceID string) (Drama, []Chapter, error) {
	detailURL := crjSiteBaseURL + "/" + strings.Trim(sourceID, "/") + "/"
	if crjDetailURL(detailURL) == "" {
		return Drama{}, nil, errors.New("91CRDJ 视频 ID 无效")
	}
	body, err := d.crjGet(ctx, detailURL)
	if err != nil {
		return Drama{}, nil, err
	}
	name := ""
	if m := reCrjTitle.FindStringSubmatch(body); len(m) > 1 {
		name = crjClean(m[1])
	}
	if name == "" {
		return Drama{}, nil, errors.New("91CRDJ 详情缺少视频名称")
	}
	cover := ""
	if m := reCrjPoster.FindStringSubmatch(body); len(m) > 0 {
		cover = crjFixURL(crjImgSrc(m[0]))
	}
	description := ""
	if m := reCrjIntro.FindStringSubmatch(body); len(m) > 1 {
		description = crjClean(m[1])
	}
	year := ""
	if m := reCrjPublished.FindStringSubmatch(body); len(m) > 1 && len(m[1]) >= 4 {
		year = m[1][:4]
	}
	status := ""
	if m := reCrjStatus.FindStringSubmatch(body); len(m) > 1 {
		status = crjClean(m[1])
	}
	var tags []string
	seenTag := map[string]bool{}
	addTag := func(v string) {
		v = strings.TrimSpace(v)
		if v != "" && !seenTag[v] && len([]rune(v)) <= 48 && len(tags) < 16 {
			seenTag[v] = true
			tags = append(tags, v)
		}
	}
	for _, block := range reCrjTags.FindAllStringSubmatch(body, -1) {
		for _, a := range reCrjAnchorTxt.FindAllStringSubmatch(block[1], -1) {
			addTag(crjClean(a[1]))
		}
	}
	if year != "" {
		addTag(year + "年")
	}

	drama := Drama{
		ID: providerDramaID(source91crj, sourceID), Source: source91crj, SourceID: sourceID,
		Title: truncate(name, 512), Name: truncate(name, 512),
		Desc: truncate(description, 12000), Intro: truncate(description, 12000),
		Cover: cover, CoverURL: cover, ChannelName: "91CRDJ", CategoryName: "成人短剧",
		Remark: truncate(status, 64), Tags: tags,
	}

	chapters := crjChapters(body, sourceID)
	if len(chapters) == 0 {
		return Drama{}, nil, errors.New("91CRDJ 暂无可播放分集")
	}
	drama.TotalEpisode, drama.EpisodeCount = len(chapters), len(chapters)
	return drama, chapters, nil
}

// crjChapters 从 ep-grid 提取分集；播放值携带完整播放页 URL 以便实时换取 m3u8。
func crjChapters(body, sourceID string) []Chapter {
	grid := ""
	if m := reCrjEpGrid.FindStringSubmatch(body); len(m) > 1 {
		grid = m[1]
	}
	if grid == "" {
		grid = body
	}
	chapters := make([]Chapter, 0, 32)
	seen := map[string]bool{}
	for index, m := range reCrjEpLink.FindAllStringSubmatch(grid, -1) {
		href := crjDetailURL(m[1])
		if href == "" || seen[href] {
			continue
		}
		seen[href] = true
		title := crjClean(m[2])
		order := index + 1
		if num := reCrjPageNumNum(title); num > 0 {
			order = num
		}
		if title == "" {
			title = fmt.Sprintf("第 %d 集", order)
		}
		chapters = append(chapters, Chapter{
			ID:             providerChapterID(source91crj, sourceID, href),
			Source:         source91crj,
			Title:          truncate(title, 128),
			CurrentEpisode: rawEpisode(order),
			VideoURL:       "crj-play://" + href,
			PageURL:        href,
			Referer:        crjSiteBaseURL + "/",
		})
	}
	sort.SliceStable(chapters, func(i, j int) bool {
		left, _ := strconv.Atoi(chapters[i].EpisodeString(i + 1))
		right, _ := strconv.Atoi(chapters[j].EpisodeString(j + 1))
		if left == right {
			return chapters[i].ID < chapters[j].ID
		}
		return left < right
	})
	return chapters
}

var reCrjNum = regexp.MustCompile(`(\d+)`)

func reCrjPageNumNum(s string) int {
	if m := reCrjNum.FindString(s); m != "" {
		if v, err := strconv.Atoi(m); err == nil && v > 0 && v <= 100000 {
			return v
		}
	}
	return 0
}

func (d *Downloader) resolveCrjMedia(ctx context.Context, task Task) (providerMedia, error) {
	source, sourceID, valid := splitProviderDramaID(task.DramaID)
	prefix := providerChapterID(source91crj, sourceID, "")
	if !valid || source != source91crj || !strings.HasPrefix(task.Chapter.ID, prefix) {
		return providerMedia{}, errors.New("91CRDJ 播放分集信息无效，请刷新详情")
	}
	pageURL := strings.TrimPrefix(task.Chapter.ID, prefix)
	if pageURL == "" {
		pageURL = task.Chapter.PageURL
	}
	if crjDetailURL(pageURL) == "" {
		return providerMedia{}, errors.New("91CRDJ 播放页地址无效")
	}
	// 优先调用 playback JSON 接口，失败回退解析播放页内嵌 JSON。
	address := d.crjPlaybackURL(ctx, pageURL)
	if address == "" {
		body, err := d.crjGet(ctx, pageURL)
		if err != nil {
			return providerMedia{}, err
		}
		address = crjExtractSrc(body)
	}
	if address == "" {
		return providerMedia{}, errors.New("91CRDJ 该集暂无可用播放地址")
	}
	if strings.HasPrefix(address, "//") {
		address = "https:" + address
	}
	if !isProviderHTTPMediaURL(address) {
		return providerMedia{}, errors.New("91CRDJ 播放地址无效")
	}
	media := providerMedia{URL: address, Referer: crjSiteBaseURL + "/"}
	if parsed, err := url.Parse(address); err == nil && strings.HasSuffix(strings.ToLower(parsed.Path), ".m3u8") {
		return d.prepareWebProviderMedia(ctx, media, "91CRDJ")
	}
	return media, nil
}

// crjPlaybackURL 调 /videos/{id}/episodes/{ep}/playback 取 src。
func (d *Downloader) crjPlaybackURL(ctx context.Context, pageURL string) string {
	m := reCrjEpPath.FindStringSubmatch(strings.TrimRight(pageURL, "/"))
	if len(m) < 3 {
		return ""
	}
	api := fmt.Sprintf("%s/videos/%s/episodes/%s/playback", crjSiteBaseURL, m[1], m[2])
	body, err := d.crjGet(ctx, api)
	if err != nil {
		return ""
	}
	var payload struct {
		Data struct {
			Src string `json:"src"`
		} `json:"data"`
	}
	if json.Unmarshal([]byte(body), &payload) != nil {
		return ""
	}
	return strings.TrimSpace(payload.Data.Src)
}

// crjExtractSrc 从播放页内嵌 playerInitialData 提取 src。
func crjExtractSrc(body string) string {
	m := regexp.MustCompile(`"src"\s*:\s*"([^"]+)"`).FindStringSubmatch(body)
	if len(m) > 1 {
		return strings.ReplaceAll(strings.ReplaceAll(m[1], `\u0026`, "&"), `\/`, "/")
	}
	return ""
}

func (d *Downloader) searchCrj(ctx context.Context, query string) ([]Drama, bool, error) {
	query = strings.TrimSpace(query)
	if query == "" || len(query) > 256 {
		return nil, false, errors.New("91CRDJ 搜索关键词无效")
	}
	body, err := d.crjGet(ctx, "/search/?"+url.Values{"keyword": {query}}.Encode())
	if err != nil {
		return nil, false, err
	}
	return parseCrjCards(body), false, nil
}
