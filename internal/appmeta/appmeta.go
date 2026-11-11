// Package appmeta 读取 pjsk-meta-builder 发布在 moesekai-app-meta 仓库里的 App 元数据
// （格式见 builder 设计文档 §4）。
package appmeta

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/StarMoe-org/moesekai-masterdata-sync/internal/crypto"
)

// DefaultBase 是公开仓库 main 分支的 raw 地址。
const DefaultBase = "https://raw.githubusercontent.com/StarMoe-org/moesekai-app-meta/main"

// Meta 是 meta.json（也是 latest.json）里本服务用到的字段。
type Meta struct {
	Schema       int    `json:"schema"`
	Region       string `json:"region"`
	AppVersion   string `json:"appVersion"`
	AppHash      string `json:"appHash"`
	DataVersion  string `json:"dataVersion"`
	UnityVersion string `json:"unityVersion"`
	Package      struct {
		Name        string `json:"name"`
		VersionName string `json:"versionName"`
		VersionCode int64  `json:"versionCode"`
	} `json:"package"`
	Files map[string]string `json:"files"`
}

// Dir 是这个版本在仓库里的目录名。同版本号的新构建写在 <appVersion>+<versionCode>/ 下，
// 但 meta.json 里的 appVersion 不带后缀，所以目录名要由调用方从 latest 的位置得知；
// 第一版只读 latest 指向的普通目录。
func (m *Meta) Dir() string { return m.Region + "/" + m.AppVersion }

// NuverseAppVersion 是 Nuverse 客户端实际发送的 X-App-Version：四段 {versionName}.{versionCode}
// （客户端 GameConfig.appVersion；服务器只看前三段）。
func (m *Meta) NuverseAppVersion() string {
	return fmt.Sprintf("%s.%d", m.Package.VersionName, m.Package.VersionCode)
}

// Keys 是 moe-info.json：[0] API 密钥，[1] ab 密钥（只有 Nuverse 服有）。
type Keys []crypto.Key

// API 返回 [0]。
func (k Keys) API() crypto.Key { return k[0] }

// ParseKeys 解析 moe-info.json。
func ParseKeys(data []byte) (Keys, error) {
	var raw []struct {
		Key    string `json:"Moe-Key"`
		Secret string `json:"Moe-Secret"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("moe-info.json: %w", err)
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("moe-info.json: 没有任何密钥")
	}
	keys := make(Keys, len(raw))
	for i, r := range raw {
		var err error
		if keys[i].Key, err = crypto.ParseBits(r.Key); err != nil {
			return nil, fmt.Errorf("moe-info.json [%d] Moe-Key: %w", i, err)
		}
		if keys[i].IV, err = crypto.ParseBits(r.Secret); err != nil {
			return nil, fmt.Errorf("moe-info.json [%d] Moe-Secret: %w", i, err)
		}
	}
	return keys, nil
}

// Bundle 是一个区服当前版本的全部元数据。
type Bundle struct {
	Meta   Meta
	Keys   Keys
	Schema []byte // 只有 Nuverse 服有；原始 JSON，由 schema 包解析
}

// Client 从 app-meta 仓库读取文件。
type Client struct {
	Base string
	HTTP *http.Client
}

// NewClient 用默认地址和 30 秒超时。
func NewClient() *Client {
	return &Client{Base: DefaultBase, HTTP: &http.Client{Timeout: 30 * time.Second}}
}

// Latest 读取区服的 latest.json 和同版本目录下的密钥、结构文件，并按 files 里的 sha256 校验。
func (c *Client) Latest(ctx context.Context, region string) (*Bundle, error) {
	raw, err := c.get(ctx, region+"/latest.json")
	if err != nil {
		return nil, err
	}
	var b Bundle
	if err := json.Unmarshal(raw, &b.Meta); err != nil {
		return nil, fmt.Errorf("%s/latest.json: %w", region, err)
	}
	if b.Meta.Schema != 1 {
		return nil, fmt.Errorf("%s/latest.json: 不支持的格式版本 %d", region, b.Meta.Schema)
	}
	if b.Meta.Region != region || b.Meta.AppVersion == "" || b.Meta.AppHash == "" {
		return nil, fmt.Errorf("%s/latest.json: region / appVersion / appHash 不对", region)
	}
	for _, name := range []string{"moe-info.json", "schema.json"} {
		want, ok := b.Meta.Files[name]
		if !ok {
			if name == "moe-info.json" {
				return nil, fmt.Errorf("%s/latest.json: files 里没有 moe-info.json", region)
			}
			continue
		}
		data, err := c.get(ctx, b.Meta.Dir()+"/"+name)
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(data)
		if got := hex.EncodeToString(sum[:]); !strings.EqualFold(got, want) {
			return nil, fmt.Errorf("%s/%s: sha256 %s 与 latest.json 记录的 %s 不一致", b.Meta.Dir(), name, got, want)
		}
		if name == "moe-info.json" {
			if b.Keys, err = ParseKeys(data); err != nil {
				return nil, err
			}
		} else {
			b.Schema = data
		}
	}
	return &b, nil
}

func (c *Client) get(ctx context.Context, path string) ([]byte, error) {
	if strings.HasPrefix(c.Base, "file://") {
		return readFile(strings.TrimPrefix(c.Base, "file://") + "/" + path)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(c.Base, "/")+"/"+path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("app-meta %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("app-meta %s: HTTP %d", path, resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 64<<20))
}
