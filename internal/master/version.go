package master

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/StarMoe-org/moesekai-masterdata-sync/internal/jsonout"
	"github.com/StarMoe-org/moesekai-masterdata-sync/internal/msgpack"
)

// Version 是 versions/current_version.json 的内容。
type Version struct {
	SystemProfile      string `json:"systemProfile,omitempty"`
	AppVersion         string `json:"appVersion,omitempty"`
	MultiPlayVersion   string `json:"multiPlayVersion,omitempty"`
	DataVersion        string `json:"dataVersion,omitempty"`
	AssetVersion       string `json:"assetVersion,omitempty"`
	AppHash            string `json:"appHash,omitempty"`
	AssetHash          string `json:"assetHash"`
	AppVersionStatus   string `json:"appVersionStatus,omitempty"`
	RemoveAssetVersion string `json:"removeAssetVersion,omitempty"`
	CDNVersion         int64  `json:"cdnVersion"`

	// Layout 决定输出的字段和顺序（不写进文件）。
	Layout Layout `json:"-"`
}

// Layout 是 Haruki 各区服仓库版本文件的格式。
type Layout int

const (
	// LayoutNuverse（cn 等）：systemProfile, appVersion, multiPlayVersion, dataVersion, assetVersion, appHash, assetHash, appVersionStatus, cdnVersion
	LayoutNuverse Layout = iota
	// LayoutSega（jp）：appHash, systemProfile, appVersion, multiPlayVersion, assetVersion, appVersionStatus, dataVersion, assetHash, removeAssetVersion, cdnVersion（恒为 0）
	LayoutSega
)

// Marshal 按 Layout 的字段顺序输出。
func (v Version) Marshal() ([]byte, error) {
	m := &msgpack.Map{}
	add := func(k string, val any) { m.Keys = append(m.Keys, k); m.Values = append(m.Values, val) }
	switch v.Layout {
	case LayoutSega:
		add("appHash", v.AppHash)
		add("systemProfile", v.SystemProfile)
		add("appVersion", v.AppVersion)
		add("multiPlayVersion", v.MultiPlayVersion)
		add("assetVersion", v.AssetVersion)
		add("appVersionStatus", v.AppVersionStatus)
		add("dataVersion", v.DataVersion)
		add("assetHash", v.AssetHash)
		add("removeAssetVersion", v.RemoveAssetVersion)
		add("cdnVersion", v.CDNVersion)
	default:
		add("systemProfile", v.SystemProfile)
		add("appVersion", v.AppVersion)
		add("multiPlayVersion", v.MultiPlayVersion)
		add("dataVersion", v.DataVersion)
		add("assetVersion", v.AssetVersion)
		add("appHash", v.AppHash)
		add("assetHash", v.AssetHash)
		add("appVersionStatus", v.AppVersionStatus)
		add("cdnVersion", v.CDNVersion)
	}
	return jsonout.Marshal(m)
}

// CompareVersions 按点分的数字逐段比较 "7.1.0.60" 这样的版本号；不是数字的段按字符串比较。
func CompareVersions(a, b string) int {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) || i < len(bs); i++ {
		var x, y string
		if i < len(as) {
			x = as[i]
		}
		if i < len(bs) {
			y = bs[i]
		}
		xn, xerr := strconv.ParseInt(x, 10, 64)
		yn, yerr := strconv.ParseInt(y, 10, 64)
		switch {
		case x == y:
			continue
		case xerr == nil && yerr == nil && xn != yn:
			if xn < yn {
				return -1
			}
			return 1
		case x < y:
			return -1
		default:
			return 1
		}
	}
	return 0
}

// ParseVersion 解析 current_version.json；data 为 nil 时返回 nil、nil。
func ParseVersion(data []byte) (*Version, error) {
	if data == nil {
		return nil, nil
	}
	var v Version
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, fmt.Errorf("current_version.json：%w", err)
	}
	return &v, nil
}

// WriteVersion 写 versions/<dataVersion>.json 和 versions/current_version.json。
func WriteVersion(sink Sink, v Version) error {
	if v.DataVersion == "" || !validName(v.DataVersion) {
		return fmt.Errorf("dataVersion %q 不能用作文件名", v.DataVersion)
	}
	data, err := v.Marshal()
	if err != nil {
		return err
	}
	if _, err := sink.Put("versions/"+v.DataVersion+".json", data); err != nil {
		return err
	}
	_, err = sink.Put("versions/current_version.json", data)
	return err
}
