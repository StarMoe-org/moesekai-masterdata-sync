package appmeta

import "os"

// readFile 让 Base 可以指向本地目录（file:///path），便于离线测试。
func readFile(path string) ([]byte, error) { return os.ReadFile(path) }
