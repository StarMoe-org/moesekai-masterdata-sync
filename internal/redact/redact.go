// Package redact 去掉错误信息里的账号 id（设计文档 O6：uid 不写日志）。
package redact

import (
	"errors"
	"net/url"
	"regexp"
)

var userSeg = regexp.MustCompile(`/user/[^/?#]+`)

// UserPath 把地址里的 /user/<id> 换成 /user/…。
func UserPath(s string) string { return userSeg.ReplaceAllString(s, "/user/…") }

// URLError 把 net/http 返回的 *url.Error 里的地址脱敏；其他错误原样返回。
// 网络错误（超时、连接被拒）的错误信息里带完整地址，login / auth 的地址里有账号 id。
func URLError(err error) error {
	var ue *url.Error
	if err == nil || !errors.As(err, &ue) {
		return err
	}
	cp := *ue
	cp.URL = UserPath(ue.URL)
	return &cp
}
