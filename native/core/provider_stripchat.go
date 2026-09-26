package core

import (
	"context"
	"encoding/base64"
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

// scContext 给 StripChat 请求附加 UA 与 Origin（部分 CDN 需要 Origin 才返回真实流）。
func scContext(ctx context.Context) context.Context {
	ctx = context.WithValue(ctx, providerTextUserAgentKey{}, scUserAgent)
	ctx = context.WithValue(ctx, providerTextOriginKey{}, scHosts[0])
	return ctx
}

// scGetJSON 逐个域名尝试请求 JSON（列表/详情）。
func (d *Downloader) scGetJSON(ctx context.Context, path string) (map[string]any, error) {
	var lastErr error
	for _, host := range scHosts {
		address := strings.TrimRight(host, "/") + path
		pageContext := scContext(ctx)
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

// scEdges 是 master 播放列表的候选边缘主机（.org 为主线路，实测有效）。
var scEdges = []string{
	"edge-hls.doppiocdn.org",
	"edge-hls.doppiocdn.media",
	"edge-hls.growcdnssedge.com",
	"edge-hls.sacfedge.com",
}

// scKey 是解密分片 URL 的固定密钥（Base64）。
const scKey = "YzWScuyQRGAGcxx1KIJmiQ7BY9Vi35ftwLqUOVO8uoo="

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
	pageContext := scContext(ctx)
	// 逐个 edge、逐个 pkey（内置 key 优先）、逐个 variant 尝试，
	// 直到取到含真实分片行的播放列表（pkey 失效时 CDN 返回占位列表）。
	var diag []string
	for _, edge := range scEdges {
		masterURL := fmt.Sprintf("https://%s/hls/%s/master/%s_auto.m3u8?playlistType=standard", edge, uid, uid)
		master, err := d.fetchProviderText(pageContext, masterURL, host+"/")
		if err != nil {
			diag = append(diag, edge+":master错误")
			continue
		}
		if !strings.Contains(master, "#EXTM3U") {
			diag = append(diag, edge+":master无效")
			continue
		}
		variants := scAllVariants(master)
		if len(variants) == 0 {
			diag = append(diag, edge+":无variant")
			continue
		}
		pkeys := scPkeys(master)
		for _, pkey := range pkeys {
			for _, variant := range variants {
				address := scWithAuth(variant, pkey)
				body, err := d.fetchProviderText(pageContext, address, host+"/")
				if err != nil {
					continue
				}
				if !strings.Contains(body, "#EXTM3U") {
					continue
				}
				if scIsAdvert(body) {
					continue
				}
				// 直播流需实时代理：不设静态 Playlist，改用回调在每次拉取
				// variant 时解密分片 URL（把 media.mp4 占位替换成真实地址）。
				return providerMedia{URL: address, RewritePlaylist: scDecryptPlaylist, Referer: host + "/"}, nil
			}
		}
		diag = append(diag, fmt.Sprintf("%s:占位(pkey×%d,var×%d)", edge, len(pkeys), len(variants)))
	}
	if len(diag) > 0 {
		return providerMedia{}, fmt.Errorf("StripChat 暂无可播放直播流（%s）", strings.Join(diag, "；"))
	}
	return providerMedia{}, errors.New("StripChat 该房间暂无可播放的直播流，可能未开播或已下播")
}

// scPkeys 返回候选 pkey 列表：内置 key 优先，其后是 master 池中其余 key。
func scPkeys(master string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, 8)
	add := func(k string) {
		if k != "" && !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	add(scBuiltinPkey)
	for _, m := range reScPSCH.FindAllStringSubmatch(master, -1) {
		add(m[1])
	}
	return out
}

// scIsAdvert 判断是否为 pkey 失效时返回的广告占位列表（无 MOUFLON:URI 行）。
func scIsAdvert(body string) bool {
	if !strings.Contains(body, "#EXTM3U") {
		return true
	}
	return !strings.Contains(body, "#EXT-X-MOUFLON:URI:")
}

// scDecryptPlaylist 逐行处理 variant：把每个分片的占位 media.mp4 替换成
// 由 #EXT-X-MOUFLON:URI 行解密得到的真实地址，并移除该辅助行。
func scDecryptPlaylist(body string) string {
	lines := strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n")
	out := make([]string, 0, len(lines))
	for i := 0; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if strings.HasPrefix(line, "#EXT-X-MOUFLON:URI:") {
			mouflon := strings.TrimSpace(strings.TrimPrefix(line, "#EXT-X-MOUFLON:URI:"))
			if i+1 < len(lines) && strings.Contains(lines[i+1], "media.mp4") {
				if real, ok := scDecryptSegment(mouflon); ok {
					lines[i+1] = real
				}
			}
			// 跳过该辅助行本身，避免播放器遇到未知标签。
			continue
		}
		out = append(out, lines[i])
	}
	return strings.Join(out, "\n")
}

// scDecryptSegment 从 MOUFLON URL 还原真实分片地址。
// 加密段 = URL 去掉 .mp4 后缀后按 "_" 取倒数第二段；解密输入为该段反转，
// 结果替换回原 URL。
func scDecryptSegment(mouflon string) (string, bool) {
	core := scMouflonTail.ReplaceAllString(mouflon, "")
	parts := strings.Split(core, "_")
	if len(parts) < 2 {
		return "", false
	}
	encrypted := parts[len(parts)-2]
	if encrypted == "" {
		return "", false
	}
	decoded, ok := scDecode(encrypted)
	if !ok {
		return "", false
	}
	return strings.Replace(mouflon, encrypted, decoded, 1), true
}

var scMouflonTail = regexp.MustCompile(`(_part\d+)?\.mp4$`)

// scDecode 复刻上游解密：Base64 解码加密段的反转，再与固定密钥逐字节异或。
func scDecode(encrypted string) (string, bool) {
	reversed := reverseString(encrypted)
	if pad := (4 - len(reversed)%4) % 4; pad > 0 {
		reversed += strings.Repeat("=", pad)
	}
	key, err := base64.StdEncoding.DecodeString(scKey)
	if err != nil || len(key) == 0 {
		return "", false
	}
	data, err := base64.StdEncoding.DecodeString(reversed)
	if err != nil {
		return "", false
	}
	out := make([]byte, len(data))
	for i := range data {
		out[i] = data[i] ^ key[i%len(key)]
	}
	return string(out), true
}

func reverseString(s string) string {
	b := []byte(s)
	for i, j := 0, len(b)-1; i < j; i, j = i+1, j-1 {
		b[i], b[j] = b[j], b[i]
	}
	return string(b)
}

// scAllVariants 从 master 提取所有 variant 播放列表地址。
func scAllVariants(master string) []string {
	var out []string
	for _, m := range reScStreamIn.FindAllStringSubmatch(master, -1) {
		if v := strings.TrimSpace(m[1]); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// scBuiltinPkey 是上游内置的首选 pkey（在池中时优先复用）。
const scBuiltinPkey = "Fq6m2TO2ZeBkRPm9"

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
