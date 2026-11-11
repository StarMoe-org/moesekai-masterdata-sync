// Package crypto 实现游戏 API 和 master 用的 AES-128-CBC + PKCS7。
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"errors"
	"fmt"
	"strings"
)

// Key 是一套 AES-128 密钥和 IV。
type Key struct {
	Key [16]byte
	IV  [16]byte
}

// ErrPadding 表示解密后的 PKCS7 填充不对，通常是密钥用错了。
var ErrPadding = errors.New("crypto: PKCS7 填充无效（密钥不对或数据损坏）")

// Decrypt 解密并去掉填充。
func (k Key) Decrypt(data []byte) ([]byte, error) {
	if len(data) == 0 || len(data)%aes.BlockSize != 0 {
		return nil, fmt.Errorf("crypto: 密文长度 %d 不是 16 的倍数", len(data))
	}
	block, err := aes.NewCipher(k.Key[:])
	if err != nil {
		return nil, err
	}
	out := make([]byte, len(data))
	cipher.NewCBCDecrypter(block, k.IV[:]).CryptBlocks(out, data)
	n := int(out[len(out)-1])
	if n == 0 || n > aes.BlockSize {
		return nil, ErrPadding
	}
	for _, c := range out[len(out)-n:] {
		if int(c) != n {
			return nil, ErrPadding
		}
	}
	return out[:len(out)-n], nil
}

// Encrypt 加上 PKCS7 填充后加密。
func (k Key) Encrypt(plain []byte) []byte {
	block, err := aes.NewCipher(k.Key[:])
	if err != nil {
		panic(err) // 16 字节密钥不会出错
	}
	n := aes.BlockSize - len(plain)%aes.BlockSize
	buf := make([]byte, len(plain)+n)
	copy(buf, plain)
	for i := len(plain); i < len(buf); i++ {
		buf[i] = byte(n)
	}
	cipher.NewCBCEncrypter(block, k.IV[:]).CryptBlocks(buf, buf)
	return buf
}

// ParseBits 解码 moe-info.json 的 0/1 位串：每字节 8 位、高位在前，16 字节共 128 个字符。
func ParseBits(s string) ([16]byte, error) {
	var out [16]byte
	if len(s) != 128 {
		return out, fmt.Errorf("crypto: 位串长度 %d，应为 128", len(s))
	}
	if strings.Trim(s, "01") != "" {
		return out, errors.New("crypto: 位串只能包含 0 和 1")
	}
	for i := 0; i < 128; i++ {
		if s[i] == '1' {
			out[i/8] |= 0x80 >> (i % 8)
		}
	}
	return out, nil
}
