package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// StripChat（stripchat）原生站源：成人直播（主播分类 → 主播 → 房间 → HLS 直播流）。
//
// 上游接口（实测 2026-09）：
//   GET /api/front/models?...&primaryTag={cat}&limit=60&offset=N   分类主播列表
//   GET /api/front/v4/models/search/group/username?query=&limit=900&primaryTag=  搜索
//   GET /api/front/v2/models/{id}/cam                             房间详情
//   GET https://edge-hls.doppiocdn.media/hls/{id}/master/{id}_auto.m3u8?playlistType=standard
//         master 播放列表，含 #EXT-X-MOUFLON:PSCH:v2:<key> 密钥池
//   GET {variant}?psch=v2&pkey={池内key}&preferredVideoCodec=H264   带鉴权的 variant
//
// 说明：本实现只做「分类 / 主播 / 房间 / 播放地址」，不含实时弹幕（guoapp 无直播弹幕通道）。
// variant 播放列表必须带 pkey 才能取到分片，故 playerContent 返回已拼好鉴权参数的 variant。

const (
	scUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:153.0) Gecko/20100101 Firefox/153.0"
	scPreferred = "H264"
	scPageSize  = 60
)

// scHosts 是可用站点域名（按顺序尝试）。
var scHosts = []string{
	"https://zh.stripchat.global",
	"https://zh.stripol.com",
	"https://zh.stripchat.com",
}

var scCategories = []nativeCategory{
	{ID: "girls", Name: "女主播"},
	{ID: "couples", Name: "情侣"},
	{ID: "men", Name: "男主播"},
	{ID: "trans", Name: "跨性别"},
}

var (
	reScPSCH     = regexp.MustCompile(`#EXT-X-MOUFLON:PSCH:v2:([A-Za-z0-9]+)`)
	reScVariant  = regexp.MustCompile(`(?m)^(https?://\S+\.m3u8[^\s]*)$`)
	reScStreamIn = regexp.MustCompile(`(?m)^#EXT-X-STREAM-INF[^\n]*\n(https?://\S+)`)
)

func validScCategory(category string) bool {
	if category == "" {
		return true
	}
	if len(category) > 24 || strings.ContainsAny(category, "|/\\\x00\r\n") {
		return false
	}
	for _, entry := range scCategories {
		if entry.ID == category {
			return true
		}
	}
	return false
}

func (d *Downloader) fetchScCategories() []nativeCategory {
	return append([]nativeCategory(nil), scCategories...)
}

// scGetJSON 逐个域名尝试请求 JSON（列表/详情）。
func (d *Downloader) scGetJSON(ctx context.Context, path string) (map[string]any, error) {
	var lastErr error
	for _, host := range scHosts {
		address := strings.TrimRight(host, "/") + path
		pageContext := context.WithValue(ctx, providerTextUserAgentKey{}, scUserAgent)
		body, err := d.fetchProviderText(pageContext, address, host+"/")
		if err != nil {
			lastErr = err
			continue
		}
		var decoded map[string]any
		if json.Unmarshal([]byte(body), &decoded) != nil {
			lastErr = errors.New("StripChat 返回的数据格式无效")
			continue
		}
		return decoded, nil
	}
	if lastErr == nil {
		lastErr = errors.New("StripChat 站点不可达")
	}
	return nil, lastErr
}

// scFlag 国家代码 → 旗帜 emoji。
func scFlag(code string) string {
	code = strings.ToUpper(strings.TrimSpace(code))
	if len(code) != 2 {
		return ""
	}
	runes := []rune{}
	for _, r := range code {
		if r < 'A' || r > 'Z' {
			return ""
		}
		runes = append(runes, 0x1F1E6+(r-'A'))
	}
	return string(runes)
}

func scRemark(model map[string]any) string {
	isLive, _ := model["isLive"].(bool)
	status := nativeText(model["status"])
	viewers := intValueAny(model["viewersCount"])
	state := "🎫 门票房"
	switch {
	case !isLive || status == "off":
		state = "⚫ 已下播"
	case status == "public":
		state = "🔴 直播中"
	}
	if viewers > 0 {
		return fmt.Sprintf("👤 %d人 | %s", viewers, state)
	}
	return state
}

func scDramaFromModel(model map[string]any) (Drama, bool) {
	id := nativeText(model["id"])
	username := mapString(model, "username")
	if id == "" || username == "" {
		return Drama{}, false
	}
	name := scFlag(mapString(model, "country")) + username
	cover := ""
	if ts := nativeText(model["snapshotTimestamp"]); ts != "" {
		cover = fmt.Sprintf("https://img.doppiocdn.org/snapshot/%s/%s", id, ts)
	} else if avatar := mapString(model, "avatarUrl"); avatar != "" {
		cover = avatar
	}
	return Drama{
		ID: providerDramaID(sourceStripchat, id), Source: sourceStripchat, SourceID: id,
		Title: truncate(name, 256), Name: truncate(name, 256),
		Cover: cover, CoverURL: cover, ChannelName: "StripChat",
		CategoryName: "直播", Remark: truncate(scRemark(model), 64),
	}, true
}

func (d *Downloader) fetchScCatalogPage(ctx context.Context, page int, category, query string) ([]Drama, bool, error) {
	if page < 1 || page > 100000 {
		return nil, false, errors.New("StripChat 目录页码无效")
	}
	if !validScCategory(category) {
		return nil, false, errors.New("StripChat 内容分类无效")
	}
	if category == "" {
		category = scCategories[0].ID
	}
	var decoded map[string]any
	var err error
	if query != "" {
		if len(query) > 128 {
			return nil, false, errors.New("StripChat 搜索关键词过长")
		}
		decoded, err = d.scGetJSON(ctx, "/api/front/v4/models/search/group/username?query="+
			url.QueryEscape(query)+"&limit=900&primaryTag="+url.QueryEscape(category))
	} else {
		offset := scPageSize * (page - 1)
		decoded, err = d.scGetJSON(ctx, fmt.Sprintf(
			"/api/front/models?improveTs=false&removeShows=false&limit=%d&offset=%d&primaryTag=%s&sortBy=stripRanking&rcmGrp=A&rbCnGr=true&prxCnGr=false&nic=false",
			scPageSize, offset, url.QueryEscape(category)))
	}
	if err != nil {
		return nil, false, err
	}
	rows, _ := decoded["models"].([]any)
	items := make([]Drama, 0, len(rows))
	seen := map[string]bool{}
	for _, row := range rows {
		model, _ := row.(map[string]any)
		drama, ok := scDramaFromModel(model)
		if !ok || seen[drama.ID] {
			continue
		}
		seen[drama.ID] = true
		items = append(items, drama)
	}
	if query != "" {
		return items, false, nil
	}
	total := intValueAny(decoded["filteredCount"])
	hasMore := total > 0 && page*scPageSize < total
	if !hasMore && len(rows) >= scPageSize {
		hasMore = true
	}
	return items, hasMore, nil
}

func (d *Downloader) fetchScDetail(ctx context.Context, sourceID string) (Drama, []Chapter, error) {
	if !webProviderNumericID.MatchString(sourceID) {
		return Drama{}, nil, errors.New("StripChat 主播 ID 无效")
	}
	decoded, err := d.scGetJSON(ctx, "/api/front/v2/models/"+url.PathEscape(sourceID)+"/cam")
	if err != nil {
		return Drama{}, nil, err
	}
	cam, _ := decoded["cam"].(map[string]any)
	userWrap, _ := decoded["user"].(map[string]any)
	user, _ := userWrap["user"].(map[string]any)
	if user == nil {
		user = userWrap
	}
	uid := nativeText(user["id"])
	if uid == "" {
		uid = sourceID
	}
	username := mapString(user, "username")
	title := mapString(cam, "topic")
	if title == "" {
		title = username
	}
	if title == "" {
		title = sourceID
	}
	cover := mapString(user, "avatarUrl")
	remark := "🔴 直播中"
	if live, _ := user["isLive"].(bool); !live {
		remark = "⚫ 已下播"
	}
	drama := Drama{
		ID: providerDramaID(sourceStripchat, sourceID), Source: sourceStripchat, SourceID: sourceID,
		Title: truncate(title, 256), Name: truncate(title, 256),
		Cover: cover, CoverURL: cover, ChannelName: "StripChat", CategoryName: "直播",
		Remark: truncate(remark, 64),
	}
	chapters := []Chapter{{
		ID: providerChapterID(sourceStripchat, sourceID, uid), Source: sourceStripchat,
		Title: "直播", CurrentEpisode: rawEpisode(1),
		VideoURL: "sc-live://" + uid, PageURL: "sc-live://" + uid,
		Referer: scHosts[0] + "/",
	}}
	drama.TotalEpisode, drama.EpisodeCount = 1, 1
	return drama, chapters, nil
}

func (d *Downloader) resolveScMedia(ctx context.Context, task Task) (providerMedia, error) {
	source, sourceID, valid := splitProviderDramaID(task.DramaID)
	prefix := providerChapterID(sourceStripchat, sourceID, "")
	if !valid || source != sourceStripchat || !strings.HasPrefix(task.Chapter.ID, prefix) {
		return providerMedia{}, errors.New("StripChat 播放信息无效，请刷新详情")
	}
	uid := strings.TrimPrefix(task.Chapter.ID, prefix)
	if uid == "" || !webProviderNumericID.MatchString(uid) {
		return providerMedia{}, errors.New("StripChat 房间 ID 无效")
	}
	host := scHosts[0]
	// master 播放列表 + PSCH 密钥池
	masterURL := fmt.Sprintf("https://edge-hls.doppiocdn.media/hls/%s/master/%s_auto.m3u8?playlistType=standard", uid, uid)
	pageContext := context.WithValue(ctx, providerTextUserAgentKey{}, scUserAgent)
	master, err := d.fetchProviderText(pageContext, masterURL, host+"/")
	if err != nil {
		return providerMedia{}, err
	}
	pkey := scPickPkey(master)
	variant := scFirstVariant(master)
	if variant == "" {
		return providerMedia{}, errors.New("StripChat 未返回可用播放线路")
	}
	address := scWithAuth(variant, pkey)
	media := providerMedia{URL: address, Referer: host + "/"}
	// variant 需带 pkey 才能取到分片，交给播放器直接请求；不再预取列表（避免 pkey 过期）。
	return media, nil
}

func scPickPkey(master string) string {
	keys := reScPSCH.FindAllStringSubmatch(master, -1)
	if len(keys) == 0 {
		return "Fq6m2TO2ZeBkRPm9"
	}
	return keys[0][1]
}

func scFirstVariant(master string) string {
	if m := reScStreamIn.FindStringSubmatch(master); len(m) > 1 {
		return strings.TrimSpace(m[1])
	}
	if m := reScVariant.FindStringSubmatch(master); len(m) > 1 {
		return strings.TrimSpace(m[1])
	}
	return ""
}

// scWithAuth 去掉旧鉴权参数，再拼上 psch/pkey/preferredVideoCodec。
func scWithAuth(variant, pkey string) string {
	cleaned := regexp.MustCompile(`&?(psch|pkey|preferredVideoCodec)=[^&]*`).ReplaceAllString(variant, "")
	sep := "?"
	if strings.Contains(cleaned, "?") {
		sep = "&"
	}
	return cleaned + sep + "psch=v2&pkey=" + url.QueryEscape(pkey) + "&preferredVideoCodec=" + scPreferred
}

func (d *Downloader) searchSc(ctx context.Context, query, category string) ([]Drama, bool, error) {
	items, more, err := d.fetchScCatalogPage(ctx, 1, category, query)
	return items, more, err
}

func intValueAny(value any) int {
	switch v := value.(type) {
	case float64:
		return int(v)
	case int:
		return v
	case json.Number:
		n, _ := v.Int64()
		return int(n)
	case string:
		n, _ := strconv.Atoi(strings.TrimSpace(v))
		return n
	}
	return 0
}
