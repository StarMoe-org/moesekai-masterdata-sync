package sega

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// AccountStore 保存账号。serve 里是 K8s Secret（N4），本机是 JSON 文件。
type AccountStore interface {
	// Load 返回已保存的账号；没有时返回 nil、nil。
	Load(ctx context.Context) (*Account, error)
	Save(ctx context.Context, a *Account) error
}

// FileStore 把账号存成一个 0600 的 JSON 文件。
type FileStore string

func (f FileStore) Load(context.Context) (*Account, error) {
	data, err := os.ReadFile(string(f))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var a Account
	if err := json.Unmarshal(data, &a); err != nil {
		return nil, fmt.Errorf("账号文件：%w", err)
	}
	if a.UserID == 0 || a.Credential == "" {
		return nil, errors.New("账号文件缺少 userId 或 credential")
	}
	return &a, nil
}

func (f FileStore) Save(_ context.Context, a *Account) error {
	data, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(string(f)), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(string(f)), ".account-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), string(f))
}

// EnsureAccount 读取账号；没有时（且 allowRegister）注册一个，先保存成功再返回。
func EnsureAccount(ctx context.Context, c *Client, store AccountStore, allowRegister bool) (*Account, bool, error) {
	a, err := store.Load(ctx)
	if err != nil || a != nil {
		return a, false, err
	}
	if !allowRegister {
		return nil, false, errors.New("sega: 没有账号，且不允许自动注册")
	}
	a, err = c.Register(ctx)
	if err != nil {
		return nil, false, err
	}
	if err := store.Save(ctx, a); err != nil {
		return nil, false, fmt.Errorf("保存新注册的账号失败（这个账号作废）：%w", err)
	}
	return a, true, nil
}
