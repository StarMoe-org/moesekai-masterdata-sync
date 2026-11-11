package master

import (
	"encoding/json"
	"fmt"

	"github.com/StarMoe-org/moesekai-masterdata-sync/internal/jsonout"
	"github.com/StarMoe-org/moesekai-masterdata-sync/internal/msgpack"
)

// Version 是 versions/current_version.json 的内容。字段顺序按 Haruki 对应区服的仓库（Nuverse 服见 nuverseOrder）。
type Version struct {
	SystemProfile    string `json:"systemProfile,omitempty"`
	AppVersion       string `json:"appVersion,omitempty"`
	MultiPlayVersion string `json:"multiPlayVersion,omitempty"`
	DataVersion      string `json:"dataVersion,omitempty"`
	AssetVersion     string `json:"assetVersion,omitempty"`
	AppHash          string `json:"appHash,omitempty"`
	AssetHash        string `json:"assetHash"`
	AppVersionStatus string `json:"appVersionStatus,omitempty"`
	CDNVersion       int64  `json:"cdnVersion"`
}

// Marshal 按 Haruki sc 仓库的字段顺序输出：
// systemProfile, appVersion, multiPlayVersion, dataVersion, assetVersion, appHash, assetHash, appVersionStatus, cdnVersion
func (v Version) Marshal() ([]byte, error) {
	m := &msgpack.Map{}
	add := func(k string, val any) { m.Keys = append(m.Keys, k); m.Values = append(m.Values, val) }
	add("systemProfile", v.SystemProfile)
	add("appVersion", v.AppVersion)
	add("multiPlayVersion", v.MultiPlayVersion)
	add("dataVersion", v.DataVersion)
	add("assetVersion", v.AssetVersion)
	add("appHash", v.AppHash)
	add("assetHash", v.AssetHash)
	add("appVersionStatus", v.AppVersionStatus)
	add("cdnVersion", v.CDNVersion)
	return jsonout.Marshal(m)
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
