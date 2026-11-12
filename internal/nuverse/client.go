// Package nuverse 是 Nuverse 服（第一版只有 cn）的客户端：不登录的 login 拿当前版本，CDN 下载 master。
// 依据：cn 6.4.1 安装包静态分析 + 2026-11-11 实测（设计文档 §5.2）。
package nuverse

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"time"

	"github.com/StarMoe-org/moesekai-masterdata-sync/internal/appmeta"
	"github.com/StarMoe-org/moesekai-masterdata-sync/internal/crypto"
	"github.com/StarMoe-org/moesekai-masterdata-sync/internal/msgpack"
)

// Config 是一个 Nuverse 区服的地址。
type Config struct {
	Region   string
	APIBase  string   // 例：https://mkcn-prod-public-60001-1.dailygn.com/api/
	CDNBases []string // 客户端的 CDNList（以 AssetBundle/ 结尾），下载时换成 MasterData/
	ServerID int      // cn 为 60001
	Platform string   // X-Platform，默认 Android
	UserID   uint64   // login 路径里的用户 id：必须是已存在的账号（不需要它的 token）；随机 id 服务器返回 500
}

// CN 是 cn 的默认地址（客户端里写死的 CDN 列表；API 地址由 SDK 下发，但正式服只有 60001 一个）。
var CN = Config{
	Region:  "cn",
	APIBase: "https://mkcn-prod-public-60001-1.dailygn.com/api/",
	CDNBases: []string{
		"https://lf3-mkcncdn-tos.dailygn.com/obj/sf-game-lf/gdl_app_5236/AssetBundle/",
		"https://lf6-mkcncdn-tos.dailygn.com/obj/sf-game-lf/gdl_app_5236/AssetBundle/",
		"https://lf9-mkcncdn-tos.dailygn.com/obj/sf-game-lf/gdl_app_5236/AssetBundle/",
		"https://lf26-mkcncdn-tos.dailygn.com/obj/sf-game-lf/gdl_app_5236/AssetBundle/",
	},
	ServerID: 60001,
	Platform: "Android",
}

// HMT 是繁中服（Nuverse，ByteDance 海外）。CDN 基址和 serverId 2026-11-12 实测确认
// （serverId 60001，与 cn 相同；gdl_app_5245）。master 下载、解码、还原已验证（与 Haruki tc 一致）。
// APIBase（login 查版本用）来自 sssekai，尚未实测；登录需要一个已存在账号的 uid（见 §5.2，与 cn 同理）。
var HMT = Config{
	Region:  "hmt",
	APIBase: "https://mkovs-20004-cdn.bytedgame.com/api/",
	CDNBases: []string{
		"https://lf16-mkovscdn-sg.bytedgame.com/obj/sf-game-alisg/gdl_app_5245/AssetBundle/",
		"https://lf19-mkovscdn-sg.bytedgame.com/obj/sf-game-alisg/gdl_app_5245/AssetBundle/",
		"https://lf21-mkovscdn-sg.bytedgame.com/obj/sf-game-alisg/gdl_app_5245/AssetBundle/",
	},
	ServerID: 60001,
	Platform: "Android",
}

// KR 是韩服（Nuverse）。CDN 基址和 serverId 2026-11-12 实测确认（serverId 60001；gdl_app_292248）。
// APIBase 来自 sssekai（mkkorea-obt-prod01，obt 可能过时），尚未实测；登录需要已存在账号的 uid。
var KR = Config{
	Region:  "kr",
	APIBase: "https://mkkorea-obt-prod01-cdn.bytedgame.com/api/",
	CDNBases: []string{
		"https://lf16-mkkr.bytedgame.com/obj/sf-game-alisg/gdl_app_292248/AssetBundle/",
		"https://lf19-mkkr.bytedgame.com/obj/sf-game-alisg/gdl_app_292248/AssetBundle/",
	},
	ServerID: 60001,
	Platform: "Android",
}

// Configs 是所有 Nuverse 服的配置，按区服索引。
var Configs = map[string]Config{"cn": CN, "hmt": HMT, "kr": KR}

// Status 是 login 响应里本服务用到的字段。
type Status struct {
	HTTPStatus       int
	AppVersion       string // 回显请求的版本（三段），不是最新版本
	AppVersionStatus string // available / not_available
	DataVersion      string
	AssetVersion     string
	MultiPlayVersion string
	CDNVersion       int64
}

// Outdated 表示服务器认为请求的客户端版本已被淘汰（426 或 not_available）。
func (s *Status) Outdated() bool {
	return s.HTTPStatus == http.StatusUpgradeRequired || s.AppVersionStatus == "not_available"
}

// ErrMaintenance 表示服务器维护中（503）。
var ErrMaintenance = errors.New("nuverse: 服务器维护中")

// Client 发请求。
type Client struct {
	Config Config
	HTTP   *http.Client
}

// New 用 30 秒超时（下载 master 另设更长的超时）。
func New(cfg Config) *Client {
	if cfg.Platform == "" {
		cfg.Platform = "Android"
	}
	return &Client{Config: cfg, HTTP: &http.Client{Timeout: 2 * time.Minute}}
}

// ErrNoUser 表示没有配置用户 id。
var ErrNoUser = errors.New("nuverse: 没有配置用户 id（MOESEKAI_CN_UID）")

// Login 发不带 token 的 POST user/{uid}/login，返回当前版本状态。
// 请求体是空明文用 [0] 密钥加密；426 时服务器用 [1] 密钥加密响应，所以解密时两套都试。
func (c *Client) Login(ctx context.Context, b *appmeta.Bundle) (*Status, error) {
	if c.Config.UserID == 0 {
		return nil, ErrNoUser
	}
	uid := c.Config.UserID
	url := fmt.Sprintf("%suser/%d/login", c.Config.APIBase, uid)
	body := b.Keys.API().Encrypt(nil)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	h := req.Header
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Accept", "application/octet-stream")
	h.Set("X-App-Version", b.Meta.NuverseAppVersion())
	h.Set("X-App-Hash", b.Meta.AppHash)
	h.Set("X-Platform", c.Config.Platform)
	h.Set("X-Install-Id", uuid4())
	h.Set("X-Request-Id", uuid4())
	ctx2, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	resp, err := c.HTTP.Do(req.WithContext(ctx2))
	if err != nil {
		return nil, fmt.Errorf("login：%w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, fmt.Errorf("login：读取响应：%w", err)
	}
	switch resp.StatusCode {
	case http.StatusOK, http.StatusUpgradeRequired:
	case http.StatusServiceUnavailable:
		return nil, ErrMaintenance
	default:
		return nil, fmt.Errorf("login：HTTP %d", resp.StatusCode)
	}
	v, err := decryptAny(b.Keys, raw)
	if err != nil {
		return nil, fmt.Errorf("login：HTTP %d，%w", resp.StatusCode, err)
	}
	m, ok := v.(*msgpack.Map)
	if !ok {
		return nil, fmt.Errorf("login：响应不是 map（%T）", v)
	}
	st := &Status{HTTPStatus: resp.StatusCode}
	st.AppVersion = str(m, "appVersion")
	st.AppVersionStatus = str(m, "appVersionStatus")
	st.DataVersion = str(m, "dataVersion")
	st.AssetVersion = str(m, "assetVersion")
	st.MultiPlayVersion = str(m, "multiPlayVersion")
	if cv, ok := m.Get("cdnVersion"); ok {
		if n, ok := cv.(int64); ok {
			st.CDNVersion = n
		}
	}
	if st.CDNVersion <= 0 || st.DataVersion == "" {
		return nil, fmt.Errorf("login：响应里没有 cdnVersion / dataVersion")
	}
	return st, nil
}

func decryptAny(keys appmeta.Keys, raw []byte) (any, error) {
	var lastErr error
	for i, k := range keys {
		plain, err := k.Decrypt(raw)
		if err != nil {
			lastErr = err
			continue
		}
		v, err := msgpack.Decode(plain)
		if err != nil {
			lastErr = fmt.Errorf("密钥 [%d] 解密后不是 msgpack：%w", i, err)
			continue
		}
		return v, nil
	}
	return nil, fmt.Errorf("响应解不开：%w", lastErr)
}

func str(m *msgpack.Map, k string) string {
	v, _ := m.Get(k)
	s, _ := v.(string)
	return s
}

// MasterURLs 按客户端 AssetBundleManager.GetMasterDataUrl 的拼法，返回每个 CDN 的 master 地址
// （从一个随机位置开始轮换，客户端也是随机选）。
func (c *Client) MasterURLs(cdnVersion int64) []string {
	n := len(c.Config.CDNBases)
	start := 0
	if n > 1 {
		r, _ := rand.Int(rand.Reader, big.NewInt(int64(n)))
		start = int(r.Int64())
	}
	urls := make([]string, 0, n)
	for i := 0; i < n; i++ {
		base := c.Config.CDNBases[(start+i)%n]
		base = replaceLast(base, "AssetBundle", "MasterData")
		urls = append(urls, fmt.Sprintf("%s%d/master-data-%d.info", base, c.Config.ServerID, cdnVersion))
	}
	return urls
}

// ErrNotFound 表示所有 CDN 都返回 404。
var ErrNotFound = errors.New("nuverse: master 在所有 CDN 上都不存在")

// DownloadMaster 依次尝试各个 CDN，返回密文。
func (c *Client) DownloadMaster(ctx context.Context, cdnVersion int64) ([]byte, string, error) {
	var errs []error
	notFound := 0
	urls := c.MasterURLs(cdnVersion)
	for _, u := range urls {
		data, status, err := c.get(ctx, u)
		if err == nil {
			return data, u, nil
		}
		if status == http.StatusNotFound {
			notFound++
		}
		errs = append(errs, err)
	}
	if notFound == len(urls) {
		return nil, "", ErrNotFound
	}
	return nil, "", errors.Join(errs...)
}

func (c *Client) get(ctx context.Context, url string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("%s：%w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, resp.StatusCode, fmt.Errorf("%s：HTTP %d", url, resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 512<<20))
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("%s：%w", url, err)
	}
	if resp.ContentLength >= 0 && int64(len(data)) != resp.ContentLength {
		return nil, resp.StatusCode, fmt.Errorf("%s：只收到 %d / %d 字节", url, len(data), resp.ContentLength)
	}
	return data, resp.StatusCode, nil
}

// Decrypt 用 [0] 密钥解密 master 并解码。
func Decrypt(key crypto.Key, raw []byte) (any, error) {
	plain, err := key.Decrypt(raw)
	if err != nil {
		return nil, fmt.Errorf("解密 master：%w", err)
	}
	v, err := msgpack.Decode(plain)
	if err != nil {
		return nil, fmt.Errorf("解码 master：%w", err)
	}
	return v, nil
}

func uuid4() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func replaceLast(s, old, new string) string {
	i := bytes.LastIndex([]byte(s), []byte(old))
	if i < 0 {
		return s
	}
	return s[:i] + new + s[i+len(old):]
}
