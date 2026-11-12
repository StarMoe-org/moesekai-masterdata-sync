// Package sega 是 CP 服（jp）的客户端：game-version → signature cookie → 注册 / 登录 → 按分片下载 master。
// 依据：设计文档 §5.1；请求格式与公开实现（sssekai 等）一致，2026-11-11 实测 jp 7.1.0。
package sega

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/StarMoe-org/moesekai-masterdata-sync/internal/appmeta"
	"github.com/StarMoe-org/moesekai-masterdata-sync/internal/msgpack"
)

// Config 是一个 CP 区服的地址和设备信息。
type Config struct {
	Region          string
	GameVersionBase string // 例：https://game-version.sekai.colorfulpalette.org
	IssueBase       string // 例：https://issue.sekai.colorfulpalette.org（拿 CloudFront cookie）
	// DefaultAPIDomain：game-version 返回 HTTP 错误时用的 API 域名。game-version 对部分机房 IP 返回
	// CloudFront 403（2026-11-11 JP2 实测），而正式服域名是固定的，游戏 API 本身可以访问。
	DefaultAPIDomain string
	Platform         string // X-Platform / 注册时的 platform
	DeviceModel      string
	OperatingSystem  string
}

// JP 是 jp 的默认配置。设备信息是一台普通的 Android 手机。
var JP = Config{
	Region:           "jp",
	GameVersionBase:  "https://game-version.sekai.colorfulpalette.org",
	IssueBase:        "https://issue.sekai.colorfulpalette.org",
	DefaultAPIDomain: "production-game-api.sekai.colorfulpalette.org",
	Platform:         "Android",
	DeviceModel:      "Google Pixel 7",
	OperatingSystem:  "Android OS 14 / API-34 (AP2A.240805.005/12025142)",
}

// EN 是 en（Global 服，CP 体系）的默认配置。地址与 jp 平行，profile 前缀 n-；
// 2026-11-12 实测 game-version、signature、/api/system 均正常。
var EN = Config{
	Region:           "en",
	GameVersionBase:  "https://game-version.sekai-en.com",
	IssueBase:        "", // en 的游戏 API 不走 CloudFront 签名 cookie（2026-11-12 实测 /api/system 无 cookie 即 200）
	DefaultAPIDomain: "n-production-game-api.sekai-en.com",
	Platform:         "Android",
	DeviceModel:      "Google Pixel 7",
	OperatingSystem:  "Android OS 14 / API-34 (AP2A.240805.005/12025142)",
}

// Configs 是所有 CP 服（Sega）的配置，按区服索引。
var Configs = map[string]Config{"jp": JP, "en": EN}

// Account 是一个游戏账号。Credential 相当于密码：只存在账号存储里，不写日志。
type Account struct {
	UserID       int64  `json:"userId"`
	Credential   string `json:"credential"`
	InstallID    string `json:"installId"`
	RegisteredAt int64  `json:"registeredAt,omitempty"`
}

// Session 是一次登录的结果。
type Session struct {
	Token              string
	AppVersion         string
	AppVersionStatus   string
	DataVersion        string
	AssetVersion       string
	AssetHash          string
	MultiPlayVersion   string
	RemoveAssetVersion string
	SplitPaths         []string // suiteMasterSplitPath
}

var (
	// ErrOutdated 表示 app-meta 里的版本已被服务器淘汰（426）。
	ErrOutdated = errors.New("sega: 客户端版本已被淘汰（426）")
	// ErrMaintenance 表示服务器维护中（503）。
	ErrMaintenance = errors.New("sega: 服务器维护中")
	// ErrAccount 表示账号不可用（登录被拒，重新拿 cookie 后仍然失败）。
	ErrAccount = errors.New("sega: 账号登录被拒")
)

// HTTPError 是其他非 2xx 响应。
type HTTPError struct {
	Method, Path string
	Status       int
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("sega: %s %s：HTTP %d", e.Method, e.Path, e.Status)
}

// Client 维护 cookie 和会话。不是并发安全的：一个区服一个 Client，顺序使用。
type Client struct {
	Config Config
	Bundle *appmeta.Bundle
	HTTP   *http.Client

	apiBase   string // https://{domain}
	profile   string
	hostHash  string
	cookie    string
	installID string
	session   *Session
}

// New 创建客户端；b 提供 appVersion、appHash、Unity 版本和密钥。
func New(cfg Config, b *appmeta.Bundle) *Client {
	return &Client{Config: cfg, Bundle: b, HTTP: &http.Client{Timeout: 2 * time.Minute}}
}

// Prepared 表示已经拿到 API 域名和 cookie（之后的轮次可以跳过 Prepare）。
func (c *Client) Prepared() bool {
	return c.apiBase != "" && (c.Config.IssueBase == "" || c.cookie != "")
}

// Profile 是 game-version 返回的环境名（production）。
func (c *Client) Profile() string { return c.profile }

// Prepare 查 game-version 拿 API 域名，再拿 CloudFront cookie。第一轮和换版本时调用。
// game-version 返回 HTTP 错误（不含 426 / 503）且配置了 DefaultAPIDomain 时，改用默认域名继续。
func (c *Client) Prepare(ctx context.Context) error {
	m := c.Bundle.Meta
	var gv struct {
		Profile, Domain, AssetbundleHostHash string
	}
	err := c.call(ctx, http.MethodGet, c.Config.GameVersionBase+"/"+m.AppVersion+"/"+m.AppHash, nil, func(v *msgpack.Map) {
		gv.Profile, _ = get(v, "profile").(string)
		gv.Domain, _ = get(v, "domain").(string)
		gv.AssetbundleHostHash, _ = get(v, "assetbundleHostHash").(string)
	})
	var he *HTTPError
	if errors.As(err, &he) && c.Config.DefaultAPIDomain != "" {
		slog.Warn("game-version 不可用，改用默认 API 域名", "region", c.Config.Region, "status", he.Status, "domain", c.Config.DefaultAPIDomain)
		gv.Profile, gv.Domain, err = "production", c.Config.DefaultAPIDomain, nil
	}
	if err != nil {
		return fmt.Errorf("game-version：%w", err)
	}
	if gv.Domain == "" {
		return errors.New("sega: game-version 没有返回 domain")
	}
	c.profile, c.hostHash = gv.Profile, gv.AssetbundleHostHash
	c.apiBase = "https://" + strings.ReplaceAll(gv.Domain, "{0}", gv.Profile)
	return c.refreshCookie(ctx)
}

// refreshCookie 调 issue 的 signature 接口，把 Set-Cookie 里的 CloudFront-* 拼成 Cookie 请求头。
func (c *Client) refreshCookie(ctx context.Context) error {
	c.cookie = ""
	if c.Config.IssueBase == "" {
		return nil // 这个区服不需要 CloudFront cookie（如 en）
	}
	resp, _, err := c.send(ctx, http.MethodPost, c.Config.IssueBase+"/api/signature", nil)
	if err != nil {
		return fmt.Errorf("signature：%w", err)
	}
	var parts []string
	for _, h := range resp.Header.Values("Set-Cookie") {
		for _, p := range strings.Split(h, ";") {
			if p = strings.TrimSpace(p); strings.HasPrefix(p, "CloudFront-") {
				parts = append(parts, p)
			}
		}
	}
	if len(parts) == 0 {
		return errors.New("sega: signature 没有返回 CloudFront cookie")
	}
	c.cookie = strings.Join(parts, "; ")
	return nil
}

// Register 注册一个新账号。调用方必须先把结果存好再使用（设计文档 §5.1）。
func (c *Client) Register(ctx context.Context) (*Account, error) {
	installID := newUUID()
	c.installID = installID
	body := msgpack.M("platform", c.Config.Platform, "deviceModel", c.Config.DeviceModel, "operatingSystem", c.Config.OperatingSystem)
	var acct *Account
	err := c.call(ctx, http.MethodPost, c.apiBase+"/api/user", body, func(v *msgpack.Map) {
		reg, _ := get(v, "userRegistration").(*msgpack.Map)
		cred, _ := get(v, "credential").(string)
		if reg == nil || cred == "" {
			return
		}
		id, _ := toInt(get(reg, "userId"))
		at, _ := toInt(get(reg, "registeredAt"))
		acct = &Account{UserID: id, Credential: cred, InstallID: installID, RegisteredAt: at}
	})
	if err != nil {
		return nil, fmt.Errorf("注册：%w", err)
	}
	if acct == nil || acct.UserID == 0 {
		return nil, errors.New("sega: 注册响应里没有 userId / credential")
	}
	return acct, nil
}

// Login 用账号登录。403 时重新拿一次 cookie 再试；仍然失败返回 ErrAccount。
func (c *Client) Login(ctx context.Context, a *Account) (*Session, error) {
	c.installID = a.InstallID
	if c.installID == "" {
		c.installID = newUUID()
	}
	c.session = nil
	url := fmt.Sprintf("%s/api/user/%d/auth?refreshUpdatedResources=False", c.apiBase, a.UserID)
	body := msgpack.M("credential", a.Credential, "deviceId", nil, "authTriggerType", "normal")
	var s *Session
	parse := func(v *msgpack.Map) {
		s = &Session{}
		s.Token, _ = get(v, "sessionToken").(string)
		s.AppVersion, _ = get(v, "appVersion").(string)
		s.AppVersionStatus, _ = get(v, "appVersionStatus").(string)
		s.DataVersion, _ = get(v, "dataVersion").(string)
		s.AssetVersion, _ = get(v, "assetVersion").(string)
		s.AssetHash, _ = get(v, "assetHash").(string)
		s.MultiPlayVersion, _ = get(v, "multiPlayVersion").(string)
		s.RemoveAssetVersion, _ = get(v, "removeAssetVersion").(string)
		if paths, ok := get(v, "suiteMasterSplitPath").([]any); ok {
			for _, p := range paths {
				if p, ok := p.(string); ok {
					s.SplitPaths = append(s.SplitPaths, p)
				}
			}
		}
	}
	err := c.call(ctx, http.MethodPut, url, body, parse)
	var he *HTTPError
	if errors.As(err, &he) && (he.Status == http.StatusForbidden || he.Status == http.StatusUnauthorized) {
		if err := c.refreshCookie(ctx); err != nil {
			return nil, err
		}
		err = c.call(ctx, http.MethodPut, url, body, parse)
		if errors.As(err, &he) && (he.Status == http.StatusForbidden || he.Status == http.StatusUnauthorized) {
			return nil, fmt.Errorf("%w（HTTP %d）", ErrAccount, he.Status)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("登录：%w", err)
	}
	if s == nil || s.Token == "" || s.DataVersion == "" {
		return nil, errors.New("sega: 登录响应里没有 sessionToken / dataVersion")
	}
	c.session = s
	return s, nil
}

// Master 逐个下载 master 分片，每个分片解码后把其中的表交给 fn（按服务器给的顺序）；分片之间不保留内存。
func (c *Client) Master(ctx context.Context, fn func(split *msgpack.Map) error) error {
	if c.session == nil {
		return errors.New("sega: 还没有登录")
	}
	if len(c.session.SplitPaths) == 0 {
		return errors.New("sega: 登录响应里没有 suiteMasterSplitPath")
	}
	for _, p := range c.session.SplitPaths {
		if strings.Contains(p, "..") || strings.HasPrefix(p, "/") {
			return fmt.Errorf("sega: 分片路径 %q 不合法", p)
		}
		var split *msgpack.Map
		if err := c.call(ctx, http.MethodGet, c.apiBase+"/api/"+p, nil, func(v *msgpack.Map) { split = v }); err != nil {
			return fmt.Errorf("分片 %s：%w", p, err)
		}
		if err := fn(split); err != nil {
			return err
		}
	}
	return nil
}

// call 发一个加密请求，解密并解码响应（必须是 map），交给 parse。
func (c *Client) call(ctx context.Context, method, url string, body any, parse func(*msgpack.Map)) error {
	var payload []byte
	if body != nil {
		raw, err := msgpack.Encode(body)
		if err != nil {
			return err
		}
		payload = c.Bundle.Keys.API().Encrypt(raw)
	}
	_, data, err := c.send(ctx, method, url, payload)
	if err != nil {
		return err
	}
	plain, err := c.Bundle.Keys.API().Decrypt(data)
	if err != nil {
		return fmt.Errorf("解密响应：%w", err)
	}
	v, err := msgpack.Decode(plain)
	if err != nil {
		return fmt.Errorf("解码响应：%w", err)
	}
	m, ok := v.(*msgpack.Map)
	if !ok {
		return fmt.Errorf("sega: 响应不是 map（%T）", v)
	}
	parse(m)
	return nil
}

func (c *Client) send(ctx context.Context, method, url string, body []byte) (*http.Response, []byte, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rd)
	if err != nil {
		return nil, nil, err
	}
	m := c.Bundle.Meta
	h := req.Header
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Accept", "application/octet-stream")
	h.Set("Accept-Encoding", "deflate, gzip")
	h.Set("User-Agent", "UnityPlayer/"+m.UnityVersion+" (UnityWebRequest/1.0, libcurl/8.10.1-DEV)")
	h.Set("X-Unity-Version", m.UnityVersion)
	h.Set("X-App-Version", m.AppVersion)
	h.Set("X-App-Hash", m.AppHash)
	h.Set("X-Platform", c.Config.Platform)
	h.Set("X-DeviceModel", c.Config.DeviceModel)
	h.Set("X-OperatingSystem", c.Config.OperatingSystem)
	h.Set("X-Request-Id", newUUID())
	if c.installID != "" {
		h.Set("X-Install-Id", c.installID)
	}
	if c.cookie != "" {
		h.Set("Cookie", c.cookie)
	}
	if s := c.session; s != nil {
		h.Set("X-Session-Token", s.Token)
		h.Set("X-Data-Version", s.DataVersion)
		h.Set("X-Asset-Version", s.AssetVersion)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	data, err := readBody(resp)
	if err != nil {
		return nil, nil, err
	}
	// 服务器可能在响应头里换发会话 token
	if t := resp.Header.Get("X-Session-Token"); t != "" && c.session != nil {
		c.session.Token = t
	}
	switch {
	case resp.StatusCode == http.StatusUpgradeRequired:
		return nil, nil, ErrOutdated
	case resp.StatusCode == http.StatusServiceUnavailable:
		return nil, nil, ErrMaintenance
	case resp.StatusCode/100 != 2:
		path := req.URL.Path
		if i := strings.Index(path, "/user/"); i >= 0 {
			path = path[:i] + "/user/…" // 不把 userId 写进错误和日志
		}
		return nil, nil, &HTTPError{Method: method, Path: req.URL.Host + path, Status: resp.StatusCode}
	}
	return resp, data, nil
}

// readBody 读响应体（Go 只在没设 Accept-Encoding 时自动解 gzip，这里自己处理）。
func readBody(resp *http.Response) ([]byte, error) {
	var r io.Reader = io.LimitReader(resp.Body, 512<<20)
	switch strings.ToLower(resp.Header.Get("Content-Encoding")) {
	case "gzip":
		gz, err := newGzipReader(r)
		if err != nil {
			return nil, err
		}
		defer gz.Close()
		r = gz
	case "deflate":
		r = newDeflateReader(r)
	}
	return io.ReadAll(r)
}

func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

func toInt(v any) (int64, bool) {
	switch x := v.(type) {
	case int64:
		return x, true
	case uint64:
		return int64(x), x <= 1<<63-1
	case string:
		n, err := strconv.ParseInt(x, 10, 64)
		return n, err == nil
	}
	return 0, false
}

func get(m *msgpack.Map, key string) any {
	if m == nil {
		return nil
	}
	v, _ := m.Get(key)
	return v
}
